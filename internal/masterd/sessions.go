package masterd

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/authtok"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// tmpSuffix marks an in-flight segment upload; the rename(2) into the final
// name is the ACK boundary (chunk-before-announce ground truth).
const tmpSuffix = ".prt-tmp"

// sessionState classifies a session lookup.
type sessionState int

const (
	sessionUnknown sessionState = iota
	sessionLive
	sessionGone // tombstoned: late PUTs get 410; dirs are never recreated
)

// session is one registered segment-push session.
type session struct {
	jobID        string
	targetDir    string
	tokenHash    [sha256.Size]byte // sha256 of the push token
	expires      time.Time
	tombstoned   bool
	tombstonedAt time.Time
}

// tokenMatches compares a presented push token against the session's in
// constant time (hash-then-compare, like authtok.Middleware).
func (s *session) tokenMatches(presented string) bool {
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], s.tokenHash[:]) == 1
}

// sessionStore is the in-memory session table with TTL reaping and
// tombstones. Tombstoned and unknown sessions answer PUTs identically (410),
// so tombstone retention is memory hygiene, not correctness.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
	ttl      time.Duration
	drain    time.Duration // DELETE -> tombstone window for in-flight PUTs
	keep     time.Duration // tombstone retention before purge
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{
		sessions: make(map[string]*session),
		ttl:      ttl,
		drain:    2 * time.Second,
		keep:     ttl,
	}
}

// register creates (or replaces) the session for jobID. Re-registration onto
// the SAME target_dir under a fresh job_id is the quality-switch path and is
// always allowed; replacing an existing job_id mints a fresh token and
// clears any tombstone.
func (st *sessionStore) register(jobID, targetDir, pushToken string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions[jobID] = &session{
		jobID:     jobID,
		targetDir: targetDir,
		tokenHash: sha256.Sum256([]byte(pushToken)),
		expires:   time.Now().Add(st.ttl),
	}
}

// lookup returns the state of jobID plus a value copy of the session (valid
// only for sessionLive).
func (st *sessionStore) lookup(jobID string) (sessionState, session) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[jobID]
	if !ok {
		return sessionUnknown, session{}
	}
	if s.tombstoned {
		return sessionGone, session{}
	}
	return sessionLive, *s
}

// touch extends the TTL of a live session. Segment-PUT traffic is the
// shim-liveness signal: a long transcode outlives any fixed TTL, so activity
// must refresh it (the TTL only reaps sessions whose shim vanished).
func (st *sessionStore) touch(jobID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.sessions[jobID]; ok && !s.tombstoned {
		s.expires = time.Now().Add(st.ttl)
	}
}

// close schedules the session's tombstone after the drain window, so PUTs
// already in flight (and the worker's final flush) still land. Idempotent;
// closing an unknown session is a no-op.
//
// A re-registration of the SAME job_id inside the drain window would be
// tombstoned with it -- acceptable: quality switches use a fresh job_id.
func (st *sessionStore) close(jobID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.sessions[jobID]
	if !ok || s.tombstoned {
		return
	}
	time.AfterFunc(st.drain, func() { st.tombstone(jobID) })
}

func (st *sessionStore) tombstone(jobID string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s, ok := st.sessions[jobID]; ok && !s.tombstoned {
		s.tombstoned = true
		s.tombstonedAt = time.Now()
	}
}

// reap tombstones expired sessions and purges old tombstones.
func (st *sessionStore) reap() {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for id, s := range st.sessions {
		switch {
		case !s.tombstoned && now.After(s.expires):
			s.tombstoned = true
			s.tombstonedAt = now
		case s.tombstoned && now.After(s.tombstonedAt.Add(st.keep)):
			delete(st.sessions, id)
		}
	}
}

// runReaper reaps periodically until ctx is done.
func (st *sessionStore) runReaper(ctx context.Context) {
	interval := st.ttl / 10
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st.reap()
		}
	}
}

// handleRegisterSession implements POST /v1/sessions (shared bearer auth).
func (s *server) handleRegisterSession(w http.ResponseWriter, r *http.Request) {
	var req protocol.RegisterSessionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	if req.JobID == "" || req.TargetDir == "" {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "job_id and target_dir are required")
		return
	}
	target := filepath.Clean(req.TargetDir)
	// Strictly under: a session dir is always a subdir of the transcode root.
	if !filepath.IsAbs(target) || !isUnder(s.transcodeRoot, target) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST",
			"target_dir must be an absolute path under the transcode root")
		return
	}
	pushToken, err := authtok.MintToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	s.sessions.register(req.JobID, target, pushToken)
	s.log.Printf("session %s registered (target %s)", req.JobID, target)
	writeJSON(w, http.StatusOK, protocol.RegisterSessionResponse{
		PushToken:    pushToken,
		AdvertiseURL: s.cfg.AdvertiseURL,
	})
}

// handleDeleteSession implements DELETE /v1/sessions/{id} (shared bearer
// auth). Idempotent: closing an unknown or already-closed session is 204.
func (s *server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.sessions.close(id)
	s.log.Printf("session %s closed (tombstone after drain)", id)
	w.WriteHeader(http.StatusNoContent)
}

// safeRelPath validates and normalizes a segment-upload relpath: relative,
// non-empty, and not escaping the target dir after cleaning. It must NOT be
// pre-rooted before Clean (Clean("/"+rel) would silently swallow ".."
// components instead of rejecting them).
func safeRelPath(rel string) (string, bool) {
	if rel == "" || strings.ContainsRune(rel, 0) || path.IsAbs(rel) {
		return "", false
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// handlePutFile implements PUT /v1/sessions/{id}/files/{relpath...},
// authenticated with the session's push token. The write is tmp + rename(2)
// with NO fsync (transcode temp is disposable); 200 is sent only after the
// rename succeeds -- that ACK is the chunk-before-announce ground truth.
func (s *server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state, sess := s.sessions.lookup(id)
	if state != sessionLive {
		// Unknown and tombstoned are answered identically; the dir is never
		// recreated (PMS cleanup race, ClusterPlex #257).
		writeError(w, http.StatusGone, "GONE", "session is closed or unknown")
		return
	}
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !sess.tokenMatches(presented) {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid push token")
		return
	}
	rel, ok := safeRelPath(r.PathValue("relpath"))
	if !ok {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid relpath")
		return
	}
	final := filepath.Join(sess.targetDir, filepath.FromSlash(rel))
	if !isUnder(sess.targetDir, final) { // defense in depth after Join
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "relpath escapes the session dir")
		return
	}

	// No MkdirAll anywhere: segment names are flat, and creating dirs here
	// could resurrect a session dir PMS already cleaned up.
	tmp := final + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.putAfterDirRemoved(w, id, rel)
			return
		}
		s.log.Printf("session %s: PUT %s open failed: %v -> 500", id, rel, err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "open: "+err.Error())
		return
	}
	_, copyErr := io.Copy(f, r.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		err := copyErr
		if err == nil {
			err = closeErr
		}
		s.log.Printf("session %s: PUT %s write failed: %v -> 500", id, rel, err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "write: "+err.Error())
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, fs.ErrNotExist) { // dir vanished between open and rename
			s.putAfterDirRemoved(w, id, rel)
			return
		}
		s.log.Printf("session %s: PUT %s rename failed: %v -> 500", id, rel, err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "rename: "+err.Error())
		return
	}
	s.sessions.touch(id)
	w.WriteHeader(http.StatusOK)
}

// putAfterDirRemoved answers a PUT whose session dir PMS already deleted
// (abandoned-session cleanup race). This is an expected race, not a server
// error: 410 tells the worker to stop retrying, and tombstoning the session
// makes any further PUTs short-circuit at lookup instead of re-probing the
// filesystem (and re-logging).
func (s *server) putAfterDirRemoved(w http.ResponseWriter, id, rel string) {
	s.log.Printf("session %s: PUT %s after session dir removed -> 410", id, rel)
	s.sessions.tombstone(id)
	writeError(w, http.StatusGone, "GONE", "session dir removed")
}
