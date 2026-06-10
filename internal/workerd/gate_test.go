package workerd

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/config"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

const testBuild = "1.43.0.10166-deadbeef"

func newTestDaemon(t *testing.T) *daemon {
	t.Helper()
	cfg := config.WorkerdConfig{
		Listen:         "127.0.0.1:0",
		ProxyListen:    "127.0.0.1:32401",
		MasterURL:      "http://127.0.0.1:1",
		DataDir:        t.TempDir(),
		EAERoot:        t.TempDir(),
		MaxJobs:        3,
		TokenFile:      "unused",
		TranscoderPath: "/bin/false",
		PlexDir:        "/nix/store/zz-plexmediaserver-" + testBuild + "/lib/plexmediaserver",
		DriversDir:     t.TempDir(),
		PushParallel:   4,
		PushQueueCap:   64,
	}
	d := newDaemon(cfg, "test-token", testBuild)
	t.Cleanup(d.cancel)
	return d
}

type relayHit struct {
	path string
	auth string
	body string
}

// newUpstream fakes the masterd relay (which forwards to PMS): it records
// every request and answers a recognizable response.
func newUpstream(t *testing.T) (*httptest.Server, chan relayHit) {
	t.Helper()
	hits := make(chan relayHit, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits <- relayHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(b)}
		w.Header().Set("X-Pms-Test", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "pms-response")
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func newRunningJob(t *testing.T, d *daemon, masterPMS string) *job {
	t.Helper()
	req := protocol.JobRequest{
		JobID:     "job1",
		PlexBuild: testBuild,
		Argv:      []string{"Plex Transcoder", "-i", "x", "out"},
		Session: protocol.SessionInfo{
			ID:        "sess1",
			PushURL:   "http://127.0.0.1:1/v1/sessions/job1",
			PushToken: "push-token",
		},
		MasterPMS: masterPMS,
	}
	j, err := newJob(d, req)
	if err != nil {
		t.Fatalf("newJob: %v", err)
	}
	d.mu.Lock()
	d.jobs[j.id] = j
	d.mu.Unlock()
	j.mu.Lock()
	j.state = protocol.JobRunning
	j.mu.Unlock()
	return j
}

func writeChunk(t *testing.T, j *job, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(j.outDir, name), []byte("chunk-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ack(j *job, name string) {
	j.mu.Lock()
	j.acked[name] = true
	j.cond.Broadcast()
	j.mu.Unlock()
}

func assertNoHit(t *testing.T, hits chan relayHit, within time.Duration) {
	t.Helper()
	select {
	case h := <-hits:
		t.Fatalf("announce forwarded prematurely: %+v", h)
	case <-time.After(within):
	}
}

// TestGateHoldsAnnounceUntilAcked is THE invariant test: a seglist POST is
// held until every referenced chunk is ACKed (out-of-order ACKs included),
// then forwarded verbatim with the PMS response returned untouched. Names
// that are not artifacts of the job (never on disk) do not block.
func TestGateHoldsAnnounceUntilAcked(t *testing.T) {
	d := newTestDaemon(t)
	upstream, hits := newUpstream(t)
	j := newRunningJob(t, d, upstream.URL+"/relay/job1")
	proxy := httptest.NewServer(d.proxyHandler())
	t.Cleanup(proxy.Close)

	writeChunk(t, j, "media-00001.ts")
	writeChunk(t, j, "media-00002.ts")

	seglist := "media-00001.ts,1.0021,307200\nmedia-00002.ts,1.0021,289792\nmedia-99999.ts,0,0\n"
	type result struct {
		status int
		header string
		body   string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Post(
			proxy.URL+"/relay/job1/video/:/transcode/session/sess1/u1/seglist",
			"text/plain", strings.NewReader(seglist))
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- result{status: resp.StatusCode, header: resp.Header.Get("X-Pms-Test"), body: string(b)}
	}()

	// Nothing ACKed: the announce must be held.
	assertNoHit(t, hits, 150*time.Millisecond)
	// Out-of-order ACK (second chunk first): still held.
	ack(j, "media-00002.ts")
	assertNoHit(t, hits, 150*time.Millisecond)
	// Final ACK releases the gate.
	ack(j, "media-00001.ts")

	select {
	case h := <-hits:
		if want := "/relay/job1/video/:/transcode/session/sess1/u1/seglist"; h.path != want {
			t.Errorf("forwarded path = %q, want %q", h.path, want)
		}
		if h.body != seglist {
			t.Errorf("forwarded body = %q, want verbatim %q", h.body, seglist)
		}
		if want := "Bearer test-token"; h.auth != want {
			t.Errorf("forwarded auth = %q, want %q", h.auth, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("announce never forwarded after all ACKs")
	}
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("announce POST: %v", res.err)
		}
		if res.status != http.StatusOK || res.body != "pms-response" || res.header != "yes" {
			t.Errorf("PMS response not relayed verbatim: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("announce response never returned to the transcoder")
	}
}

// TestProxyPassthroughPaths: progress (and any non-announce) POSTs forward
// immediately, with zero ACKs recorded.
func TestProxyPassthroughPaths(t *testing.T) {
	d := newTestDaemon(t)
	upstream, hits := newUpstream(t)
	newRunningJob(t, d, upstream.URL+"/relay/job1")
	proxy := httptest.NewServer(d.proxyHandler())
	t.Cleanup(proxy.Close)

	resp, err := http.Post(
		proxy.URL+"/relay/job1/video/:/transcode/session/sess1/progress?offset=1.2",
		"text/plain", strings.NewReader("progress-body"))
	if err != nil {
		t.Fatalf("progress POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("progress status = %d", resp.StatusCode)
	}
	select {
	case h := <-hits:
		if h.body != "progress-body" {
			t.Errorf("progress body = %q", h.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("progress never forwarded")
	}
}

// TestGateAbortsOnFailingJob: a held announce 502s when the job starts
// failing instead of blocking forever (or forwarding).
func TestGateAbortsOnFailingJob(t *testing.T) {
	d := newTestDaemon(t)
	upstream, hits := newUpstream(t)
	j := newRunningJob(t, d, upstream.URL+"/relay/job1")
	proxy := httptest.NewServer(d.proxyHandler())
	t.Cleanup(proxy.Close)

	writeChunk(t, j, "media-00001.ts")
	done := make(chan int, 1)
	go func() {
		resp, err := http.Post(
			proxy.URL+"/relay/job1/video/:/transcode/session/sess1/u1/seglist",
			"text/plain", strings.NewReader("media-00001.ts,1.0,1024\n"))
		if err != nil {
			done <- -1
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	assertNoHit(t, hits, 150*time.Millisecond)

	j.mu.Lock()
	j.failing = true
	j.cond.Broadcast()
	j.mu.Unlock()

	select {
	case status := <-done:
		if status != http.StatusBadGateway {
			t.Errorf("announce status = %d, want 502", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gate did not abort on failing job")
	}
	assertNoHit(t, hits, 100*time.Millisecond)
}

func TestReferencedChunks(t *testing.T) {
	body := "media-00001.ts,1.0,1\n" +
		`<MPD><SegmentTemplate initialization="init-stream$RepresentationID$.m4s" media="chunk-stream$RepresentationID$-$Number%05d$.m4s"/></MPD>` +
		"\nsub-chunk-00004\nmedia-00001.ts,dup\n"
	got := referencedChunks([]byte(body))
	want := []string{
		"media-00001.ts",
		"init-stream$RepresentationID$.m4s",
		"chunk-stream$RepresentationID$-$Number%05d$.m4s",
		"sub-chunk-00004",
	}
	if len(got) != len(want) {
		t.Fatalf("referencedChunks = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("referencedChunks[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
