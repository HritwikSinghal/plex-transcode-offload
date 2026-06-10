package masterd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/authtok"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/config"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

const testToken = "test-shared-token"

// newTestServer builds a server over temp roots and an httptest frontend.
// The reaper and prober are not started (not needed for these smoke tests).
func newTestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	cfg := config.MasterdConfig{
		Listen:           ":0",
		PMSURL:           "http://127.0.0.1:1",
		AdvertiseURL:     "http://127.0.0.1:32499",
		TranscodeRoot:    t.TempDir(),
		MediaRoots:       []string{t.TempDir()},
		CodecsDir:        t.TempDir(),
		Workers:          []string{"http://127.0.0.1:1"},
		TokenFile:        "unused",
		SessionTTLSec:    600,
		ProbeIntervalSec: 10,
	}
	s, err := newServer(cfg, testToken)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func doReq(t *testing.T, method, url, bearer string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func register(t *testing.T, ts *httptest.Server, jobID, targetDir string) protocol.RegisterSessionResponse {
	t.Helper()
	body, _ := json.Marshal(protocol.RegisterSessionRequest{JobID: jobID, TargetDir: targetDir})
	resp := doReq(t, http.MethodPost, ts.URL+"/v1/sessions", testToken, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status %d", resp.StatusCode)
	}
	var out protocol.RegisterSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("register decode: %v", err)
	}
	return out
}

func TestRegisterAndPutSegment(t *testing.T) {
	s, ts := newTestServer(t)
	target := filepath.Join(s.transcodeRoot, "Sessions", "sess-1")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	out := register(t, ts, "j1", target)
	if out.PushToken == "" || out.AdvertiseURL != s.cfg.AdvertiseURL {
		t.Fatalf("bad register response: %+v", out)
	}

	// Happy-path PUT: 200 only after rename; no .prt-tmp residue.
	put := ts.URL + "/v1/sessions/j1/files/media-00001.ts"
	resp := doReq(t, http.MethodPut, put, out.PushToken, []byte("chunkdata"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: status %d", resp.StatusCode)
	}
	got, err := os.ReadFile(filepath.Join(target, "media-00001.ts"))
	if err != nil || string(got) != "chunkdata" {
		t.Fatalf("segment content: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(target, "media-00001.ts"+tmpSuffix)); !os.IsNotExist(err) {
		t.Fatalf("tmp file left behind: %v", err)
	}

	// Wrong push token -> 401; shared token is NOT valid for PUTs.
	if resp := doReq(t, http.MethodPut, put, testToken, []byte("x")); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PUT wrong token: status %d, want 401", resp.StatusCode)
	}

	// Unknown session -> 410, and nothing is created.
	resp = doReq(t, http.MethodPut, ts.URL+"/v1/sessions/nope/files/a.ts", out.PushToken, []byte("x"))
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("PUT unknown session: status %d, want 410", resp.StatusCode)
	}
}

func TestDeleteSessionTombstonesAfterDrain(t *testing.T) {
	s, ts := newTestServer(t)
	s.sessions.drain = 50 * time.Millisecond
	target := filepath.Join(s.transcodeRoot, "sess-2")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	out := register(t, ts, "j2", target)

	if resp := doReq(t, http.MethodDelete, ts.URL+"/v1/sessions/j2", testToken, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status %d", resp.StatusCode)
	}
	time.Sleep(200 * time.Millisecond) // past the drain window
	resp := doReq(t, http.MethodPut, ts.URL+"/v1/sessions/j2/files/late.ts", out.PushToken, []byte("x"))
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("PUT after tombstone: status %d, want 410", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(target, "late.ts")); !os.IsNotExist(err) {
		t.Fatal("tombstoned session wrote a file")
	}
}

func TestPutAfterSessionDirRemoved(t *testing.T) {
	s, ts := newTestServer(t)
	target := filepath.Join(s.transcodeRoot, "sess-gone")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	out := register(t, ts, "jg", target)

	// PMS abandoned the session and deleted its dir; the session is still
	// live in the registry. A late PUT must be 410 (expected race), not 500.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	put := ts.URL + "/v1/sessions/jg/files/media-00009.ts"
	if resp := doReq(t, http.MethodPut, put, out.PushToken, []byte("x")); resp.StatusCode != http.StatusGone {
		t.Fatalf("PUT after dir removed: status %d, want 410", resp.StatusCode)
	}
	// The ENOENT path tombstones the session: the next PUT 410s at lookup.
	if state, _ := s.sessions.lookup("jg"); state != sessionGone {
		t.Fatalf("session state after ENOENT PUT: %v, want sessionGone", state)
	}
	if resp := doReq(t, http.MethodPut, put, out.PushToken, []byte("x")); resp.StatusCode != http.StatusGone {
		t.Fatalf("second PUT after tombstone: status %d, want 410", resp.StatusCode)
	}
	// The dir was never resurrected.
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("session dir was recreated by a late PUT")
	}
}

func TestTraversalRejected(t *testing.T) {
	s, ts := newTestServer(t)

	// target_dir outside the transcode root -> 400.
	body, _ := json.Marshal(protocol.RegisterSessionRequest{JobID: "jx", TargetDir: "/etc"})
	if resp := doReq(t, http.MethodPost, ts.URL+"/v1/sessions", testToken, body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("register /etc: status %d, want 400", resp.StatusCode)
	}
	// target_dir that Cleans to outside (traversal) -> 400.
	body, _ = json.Marshal(protocol.RegisterSessionRequest{JobID: "jx", TargetDir: s.transcodeRoot + "/../evil"})
	if resp := doReq(t, http.MethodPost, ts.URL+"/v1/sessions", testToken, body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("register traversal: status %d, want 400", resp.StatusCode)
	}
	// target_dir equal to the root itself -> 400 (must be strictly under).
	body, _ = json.Marshal(protocol.RegisterSessionRequest{JobID: "jx", TargetDir: s.transcodeRoot})
	if resp := doReq(t, http.MethodPost, ts.URL+"/v1/sessions", testToken, body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("register root: status %d, want 400", resp.StatusCode)
	}

	// PUT relpath traversal -> 400 (handler-level; the mux may also block
	// the encoded form upstream, so exercise the handler directly).
	target := filepath.Join(s.transcodeRoot, "sess-3")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	out := register(t, ts, "j3", target)
	req := httptest.NewRequest(http.MethodPut, "/v1/sessions/j3/files/x", strings.NewReader("data"))
	req.SetPathValue("id", "j3")
	req.SetPathValue("relpath", "../evil.ts")
	req.Header.Set("Authorization", "Bearer "+out.PushToken)
	rec := httptest.NewRecorder()
	s.handlePutFile(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT traversal relpath: status %d, want 400", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(s.transcodeRoot, "evil.ts")); !os.IsNotExist(err) {
		t.Fatal("traversal PUT escaped the session dir")
	}
}

func TestSignedMediaURL(t *testing.T) {
	s, ts := newTestServer(t)
	mediaFile := filepath.Join(s.mediaRoots[0], "movie.mkv")
	if err := os.WriteFile(mediaFile, []byte("MEDIA-BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}

	signed := authtok.SignURL(testToken, "/v1/media?path="+url.QueryEscape(mediaFile),
		time.Now().Add(time.Minute))
	resp := doReq(t, http.MethodGet, ts.URL+signed, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed GET: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "MEDIA-BYTES" {
		t.Fatalf("signed GET body: %q", body)
	}

	// Tampered path (signature mismatch) -> 403.
	tampered := strings.Replace(ts.URL+signed, url.QueryEscape(mediaFile),
		url.QueryEscape(mediaFile+"x"), 1)
	if resp := doReq(t, http.MethodGet, tampered, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered GET: status %d, want 403", resp.StatusCode)
	}

	// Expired signature -> 403.
	expired := authtok.SignURL(testToken, "/v1/media?path="+url.QueryEscape(mediaFile),
		time.Now().Add(-time.Minute))
	if resp := doReq(t, http.MethodGet, ts.URL+expired, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired GET: status %d, want 403", resp.StatusCode)
	}

	// Correctly signed but outside every allowed root -> 403.
	outside := authtok.SignURL(testToken, "/v1/media?path="+url.QueryEscape("/etc/hostname"),
		time.Now().Add(time.Minute))
	if resp := doReq(t, http.MethodGet, ts.URL+outside, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outside-root GET: status %d, want 403", resp.StatusCode)
	}
}
