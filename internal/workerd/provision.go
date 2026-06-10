package workerd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// provisionTimeout bounds one full codec-cache fetch (a Codecs dir is tens
// of MB on a LAN).
const provisionTimeout = 10 * time.Minute

// codecSyncWait bounds how long a starting job waits for the per-job codec
// top-up before proceeding with whatever is cached (the shared fetch keeps
// running in the background).
const codecSyncWait = 30 * time.Second

// provisionRun deduplicates concurrent provisions of one build
// (singleflight): the POST /v1/provision handler and background kicks from
// cold-cache 503s share one fetch.
type provisionRun struct {
	done chan struct{}
	err  error
}

// provisionBuild syncs <data_dir>/codecs/<build> against the master's
// manifest. Existence of the build dir is NOT enough: PMS downloads codec
// libs on demand, so a new media's first play can add e.g.
// libac3_decoder.so to the master seconds before the job dispatches. Every
// call re-fetches the manifest and tops up what is missing. Blocks until
// the (possibly shared) fetch finishes or ctx is done; the fetch itself
// continues in the background regardless.
func (d *daemon) provisionBuild(ctx context.Context, build string) error {
	if !validID(build) {
		return fmt.Errorf("workerd: invalid build %q", build)
	}
	d.provMu.Lock()
	run := d.prov[build]
	if run == nil {
		run = &provisionRun{done: make(chan struct{})}
		d.prov[build] = run
		go func() {
			run.err = d.fetchCodecs(build)
			d.provMu.Lock()
			delete(d.prov, build)
			d.provMu.Unlock()
			close(run.done)
		}()
	}
	d.provMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-run.done:
		return run.err
	}
}

// fetchCodecs pulls the build's codec set from masterd: manifest, then each
// missing file with sha256 verification and an atomic per-file rename into
// place. A cold cache is assembled in <build>.partial and renamed as one
// unit; a warm cache is topped up in place (only manifest entries that are
// absent or size-changed locally are fetched). Integrity is checked HERE,
// at provision time only -- per-job verification is a bare stat.
func (d *daemon) fetchCodecs(build string) error {
	ctx, cancel := context.WithTimeout(d.ctx, provisionTimeout)
	defer cancel()

	var manifest protocol.CodecsManifest
	manifestURL := d.masterURL() + "/v1/codecs/" + url.PathEscape(build) + "/manifest"
	if err := d.getJSON(ctx, manifestURL, &manifest); err != nil {
		return fmt.Errorf("workerd: codecs manifest for %s: %w", build, err)
	}

	final := d.codecsDir(build)
	if dirExists(final) {
		return d.syncCodecs(ctx, build, manifest, final)
	}
	partial := final + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return fmt.Errorf("workerd: clean partial codecs dir: %w", err)
	}
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return err
	}
	for _, cf := range manifest.Files {
		if !safeRelPath(cf.Name) {
			return fmt.Errorf("workerd: codecs manifest for %s: unsafe file name %q", build, cf.Name)
		}
		if err := d.fetchCodecFile(ctx, build, cf, partial); err != nil {
			return fmt.Errorf("workerd: codecs %s/%s: %w", build, cf.Name, err)
		}
	}
	if err := os.Rename(partial, final); err != nil {
		if dirExists(final) { // lost a benign race; the cache is warm
			_ = os.RemoveAll(partial)
			return nil
		}
		return err
	}
	d.logf("provisioned codec cache for build %s (%d files)", build, len(manifest.Files))
	return nil
}

// syncCodecs tops up a warm codec dir: every manifest file that is absent
// or size-changed locally is fetched and renamed into place. Name+size is a
// sufficient identity check -- Plex codec files are immutable within a
// build, so a matching size never hides changed content -- and a bare stat
// per file keeps this per-job path nearly free.
func (d *daemon) syncCodecs(ctx context.Context, build string, manifest protocol.CodecsManifest, final string) error {
	fetched := 0
	for _, cf := range manifest.Files {
		if !safeRelPath(cf.Name) {
			return fmt.Errorf("workerd: codecs manifest for %s: unsafe file name %q", build, cf.Name)
		}
		st, err := os.Stat(filepath.Join(final, filepath.FromSlash(cf.Name)))
		if err == nil && st.Mode().IsRegular() && st.Size() == cf.Size {
			continue
		}
		if err := d.fetchCodecFile(ctx, build, cf, final); err != nil {
			return fmt.Errorf("workerd: codecs %s/%s: %w", build, cf.Name, err)
		}
		fetched++
	}
	if fetched > 0 {
		d.logf("codec cache for build %s: synced %d new file(s)", build, fetched)
	}
	return nil
}

func (d *daemon) fetchCodecFile(ctx context.Context, build string, cf protocol.CodecFile, destRoot string) error {
	target := d.masterURL() + "/v1/codecs/" + url.PathEscape(build) + "/files/" + escapePathSegments(cf.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", target, resp.StatusCode)
	}

	dest := filepath.Join(destRoot, filepath.FromSlash(cf.Name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// Write-then-rename: warm-path syncs land in a live dir that a running
	// transcoder may dlopen mid-download. 0755 for everything: the set is
	// shared libraries plus the EasyAudioEncoder binary; the manifest
	// carries no mode bits.
	tmp := dest + ".fetching"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != cf.Size {
		err = fmt.Errorf("size mismatch: got %d, manifest says %d", n, cf.Size)
	}
	if err == nil {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, cf.SHA256) {
			err = fmt.Errorf("sha256 mismatch: got %s, manifest says %s", got, cf.SHA256)
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// getJSON GETs target with the shared bearer token and decodes the JSON
// response.
func (d *daemon) getJSON(ctx context.Context, target string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", target, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// safeRelPath accepts clean, relative, non-escaping slash paths (codec
// manifest entries may nest, e.g. EasyAudioEncoder/EasyAudioEncoder).
func safeRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	clean := path.Clean(p)
	return clean == p && clean != ".." && !strings.HasPrefix(clean, "../")
}

// escapePathSegments escapes each segment of a slash path for use in a URL
// path (PathEscape alone would escape the separators too).
func escapePathSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
