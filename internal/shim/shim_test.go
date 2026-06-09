package shim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/config"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// TestTryRemoteHappyPath drives the full dispatch path against fake masterd
// and workerd servers: register, pick worker, POST job, consume the events
// stream, exit with the remote code, close the session.
func TestTryRemoteHappyPath(t *testing.T) {
	const token = "test-token"

	var (
		mu             sync.Mutex
		gotJob         protocol.JobRequest
		sessionDeleted bool
	)

	// Fake workerd: accepts the job, then streams state/stderr/exit events.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("worker: missing bearer token on %s %s", r.Method, r.URL.Path)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
			mu.Lock()
			err := json.NewDecoder(r.Body).Decode(&gotJob)
			mu.Unlock()
			if err != nil {
				t.Errorf("worker: decode job: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			f := w.(http.Flusher)
			for _, line := range []string{
				`{"type":"state","state":"RUNNING"}`,
				`{"type":"stderr","line":"frame=1"}`,
				`{"type":"exit","code":3}`,
			} {
				fmt.Fprintln(w, line)
				f.Flush()
			}
		default:
			t.Errorf("worker: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer worker.Close()

	// Fake masterd: session registration, worker list, session close.
	var masterd *httptest.Server
	masterd = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			var req protocol.RegisterSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.JobID == "" {
				t.Errorf("masterd: bad session registration: %v", err)
			}
			json.NewEncoder(w).Encode(protocol.RegisterSessionResponse{
				PushToken:    "push-token",
				AdvertiseURL: masterd.URL,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workers":
			json.NewEncoder(w).Encode(protocol.WorkersResponse{
				Workers: []protocol.WorkerStatus{{
					URL:     worker.URL,
					Healthy: true,
					Health:  protocol.Health{ActiveJobs: 0, MaxJobs: 3},
				}},
			})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
			mu.Lock()
			sessionDeleted = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("masterd: unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer masterd.Close()

	dir := t.TempDir()
	confPath := filepath.Join(dir, "prt.json")
	conf := fmt.Sprintf(`{"masterd_url":%q,"spawn_budget_ms":5000}`, masterd.URL)
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvConf, confPath)
	t.Setenv(config.EnvTokenFile, tokenPath)

	args := []string{
		"-codec:0", "h264",
		"-i", "/mnt/media/Movies/A.mkv",
		"-progressurl", "http://127.0.0.1:32400/video/:/transcode/session/abc/0/progress",
		"-manifest_name", "http://127.0.0.1:32400/video/:/transcode/session/abc/0/seglist",
		"media-%05d.ts",
	}
	code, handled := tryRemote(args)
	if !handled {
		t.Fatal("tryRemote() fell back local, want remote handling")
	}
	if code != 3 {
		t.Errorf("tryRemote() exit code = %d, want 3 (remote transcoder code)", code)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sessionDeleted {
		t.Error("masterd session was not deleted after the exit event")
	}
	if gotJob.Session.PushToken != "push-token" {
		t.Errorf("job push_token = %q, want %q", gotJob.Session.PushToken, "push-token")
	}
	if want := masterd.URL + "/relay/" + gotJob.JobID; gotJob.MasterPMS != want {
		t.Errorf("job master_pms = %q, want %q", gotJob.MasterPMS, want)
	}
	if len(gotJob.Media) != 1 {
		t.Errorf("job media has %d entries, want 1: %v", len(gotJob.Media), gotJob.Media)
	}
	foundMedia, foundPMS := false, false
	for _, a := range gotJob.Argv {
		if a == "{{MEDIA:0}}" {
			foundMedia = true
		}
		if strings.HasPrefix(a, protocol.PlaceholderPMS+"/") {
			foundPMS = true
		}
	}
	if !foundMedia || !foundPMS {
		t.Errorf("job argv missing placeholders (media=%v pms=%v): %q", foundMedia, foundPMS, gotJob.Argv)
	}
}
