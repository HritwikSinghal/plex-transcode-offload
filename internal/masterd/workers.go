package masterd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// workerCache is masterd's cached view of the configured workers, refreshed
// by the background prober. Order matches cfg.Workers.
type workerCache struct {
	mu       sync.RWMutex
	statuses []protocol.WorkerStatus
}

func newWorkerCache(urls []string) *workerCache {
	statuses := make([]protocol.WorkerStatus, len(urls))
	for i, u := range urls {
		statuses[i] = protocol.WorkerStatus{URL: u} // unhealthy until probed
	}
	return &workerCache{statuses: statuses}
}

func (c *workerCache) set(i int, st protocol.WorkerStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses[i] = st
}

func (c *workerCache) snapshot() []protocol.WorkerStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]protocol.WorkerStatus, len(c.statuses))
	copy(out, c.statuses)
	return out
}

// runProber probes every worker immediately, then on each tick, until ctx is
// done. The shim only ever reads the cache (no per-spawn probe storms).
func (s *server) runProber(ctx context.Context) {
	interval := time.Duration(s.cfg.ProbeIntervalSec) * time.Second
	client := &http.Client{Timeout: probeTimeout(interval)}
	s.probeAll(ctx, client)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeAll(ctx, client)
		}
	}
}

// probeTimeout bounds one probe well inside the probe interval.
func probeTimeout(interval time.Duration) time.Duration {
	t := interval / 2
	if t > 5*time.Second {
		t = 5 * time.Second
	}
	if t < time.Second {
		t = time.Second
	}
	return t
}

// probeAll GETs each worker's /v1/health (unauthenticated by contract) in
// parallel and stores the results.
func (s *server) probeAll(ctx context.Context, client *http.Client) {
	var wg sync.WaitGroup
	for i, base := range s.cfg.Workers {
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			s.workers.set(i, probeWorker(ctx, client, base))
		}(i, base)
	}
	wg.Wait()
}

func probeWorker(ctx context.Context, client *http.Client, base string) protocol.WorkerStatus {
	st := protocol.WorkerStatus{URL: base}
	url := strings.TrimSuffix(base, "/") + "/v1/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return st
	}
	resp, err := client.Do(req)
	if err != nil {
		return st
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st
	}
	var h protocol.Health
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&h) != nil {
		return st
	}
	st.Healthy = true
	st.Health = h
	return st
}

// handleWorkers implements GET /v1/workers from the cache.
func (s *server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, protocol.WorkersResponse{Workers: s.workers.snapshot()})
}
