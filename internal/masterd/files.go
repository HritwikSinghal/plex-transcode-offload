package masterd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/authtok"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// handleMedia implements GET /v1/media?path=...&exp=...&sig=...: a
// Range-capable read-only file server over the allowlisted roots,
// authenticated by the HMAC-signed query (signed with the shared token).
func (s *server) handleMedia(w http.ResponseWriter, r *http.Request) {
	if err := authtok.VerifySignedQuery(s.token, r); err != nil {
		writeError(w, http.StatusForbidden, "FORBIDDEN", err.Error())
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || !filepath.IsAbs(p) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "path must be an absolute file path")
		return
	}
	clean := filepath.Clean(p)
	allowed := atOrUnder(s.transcodeRoot, clean)
	for _, root := range s.mediaRoots {
		if allowed {
			break
		}
		allowed = atOrUnder(root, clean)
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "path is outside the allowed roots")
		return
	}
	f, err := os.Open(clean)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such file")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() { // no directory listings
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such file")
		return
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// codecCache serves the master's Codecs dir per build, caching each build's
// manifest (name/size/sha256 of the top-level regular files) after the first
// computation. Only flat names are served -- /v1/codecs/{build}/files/{name}
// forbids path separators by contract.
type codecCache struct {
	dir       string
	mu        sync.Mutex
	manifests map[string]*protocol.CodecsManifest
}

func newCodecCache(dir string) *codecCache {
	return &codecCache{dir: dir, manifests: make(map[string]*protocol.CodecsManifest)}
}

// safeName accepts a single path component: no separators, no traversal.
func safeName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00")
}

// resolveDir maps a worker-reported build string onto the master's actual
// Codecs subdir. Plex names these dirs after the TRANSCODER's internal
// codec version ("c75335c-<hash>-linux-x86_64"), which is not derivable
// from the PMS build string the shim pins -- so an exact match is tried
// first and the lookup then falls back over the non-EAE subdirs: with
// several candidates (old dirs survive Plex upgrades) the most recently
// modified wins, which is the one the running PMS maintains.
func (c *codecCache) resolveDir(build string) (string, error) {
	exact := filepath.Join(c.dir, build)
	if st, err := os.Stat(exact); err == nil && st.IsDir() {
		return exact, nil
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return "", err
	}
	var best string
	var bestMod time.Time
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "EasyAudioEncoder") {
			continue
		}
		full := filepath.Join(c.dir, e.Name())
		st, err := os.Stat(full) // follows symlinks (nix-store friendliness)
		if err != nil || !st.IsDir() {
			continue
		}
		if best == "" || st.ModTime().After(bestMod) {
			best, bestMod = full, st.ModTime()
		}
	}
	if best == "" {
		return "", fs.ErrNotExist
	}
	return best, nil
}

// manifest returns the (possibly cached) manifest for build. fs.ErrNotExist
// is returned when no codec dir resolves for it.
func (c *codecCache) manifest(build string) (*protocol.CodecsManifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.manifests[build]; ok {
		return m, nil
	}
	dir, err := c.resolveDir(build)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &protocol.CodecsManifest{Build: build, Files: []protocol.CodecFile{}}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		info, err := os.Stat(full) // follows symlinks (nix-store friendliness)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		sum, err := sha256File(full)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", full, err)
		}
		m.Files = append(m.Files, protocol.CodecFile{
			Name:   e.Name(),
			Size:   info.Size(),
			SHA256: sum,
		})
	}
	c.manifests[build] = m
	return m, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// handleCodecsManifest implements GET /v1/codecs/{build}/manifest.
func (s *server) handleCodecsManifest(w http.ResponseWriter, r *http.Request) {
	build := r.PathValue("build")
	if !safeName(build) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid build")
		return
	}
	m, err := s.codecs.manifest(build)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown build")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleCodecsFile implements GET /v1/codecs/{build}/files/{name}.
func (s *server) handleCodecsFile(w http.ResponseWriter, r *http.Request) {
	build := r.PathValue("build")
	name := r.PathValue("name")
	if !safeName(build) || !safeName(name) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid build or file name")
		return
	}
	dir, err := s.codecs.resolveDir(build)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown build")
		return
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such codec file")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "no such codec file")
		return
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}
