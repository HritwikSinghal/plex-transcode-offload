// Package workerd is the worker-side daemon (:32500): job runner, segment
// pusher, gating proxy, provisioner, and EAE supervisor.
//
// Two listeners: the authenticated API on cfg.listen (bearer token on
// everything except GET /v1/health) and the unauthenticated gating proxy on
// cfg.proxy_listen (loopback only -- it is the transcoder's 127.0.0.1:32400
// stand-in).
package workerd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/authtok"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/config"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/ndjson"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// Run executes the workerd role with its CLI args (--config <path>) and
// returns the process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("prt-workerd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "", "path to the workerd JSON config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "prt-workerd: --config is required")
		fs.Usage()
		return 2
	}
	cfg, err := config.LoadWorkerd(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prt-workerd: %v\n", err)
		return 1
	}
	token, err := authtok.LoadToken(cfg.TokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prt-workerd: %v\n", err)
		return 1
	}
	build, err := plexBuildFromDir(cfg.PlexDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prt-workerd: %v\n", err)
		return 1
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := newDaemon(cfg, token, build)
	defer d.cancel()
	if err := d.serve(sigCtx); err != nil {
		fmt.Fprintf(os.Stderr, "prt-workerd: %v\n", err)
		return 1
	}
	return 0
}

// daemon is the workerd runtime state shared by both listeners.
type daemon struct {
	cfg       config.WorkerdConfig
	token     string
	plexBuild string
	// client is the single keep-alive HTTP client for every outbound call:
	// segment PUTs, relay forwards, spooling, provisioning.
	client *http.Client
	logger *log.Logger

	// ctx is the daemon lifetime; cancelling it kills jobs and the EAE.
	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	jobs map[string]*job

	provMu sync.Mutex
	prov   map[string]*provisionRun
}

func newDaemon(cfg config.WorkerdConfig, token, plexBuild string) *daemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &daemon{
		cfg:       cfg,
		token:     token,
		plexBuild: plexBuild,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: cfg.PushParallel + 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger: log.New(os.Stderr, "prt-workerd: ", log.LstdFlags),
		ctx:    ctx,
		cancel: cancel,
		jobs:   make(map[string]*job),
		prov:   make(map[string]*provisionRun),
	}
}

func (d *daemon) logf(format string, args ...any) { d.logger.Printf(format, args...) }

func (d *daemon) jobsRoot() string   { return filepath.Join(d.cfg.DataDir, "jobs") }
func (d *daemon) codecsRoot() string { return filepath.Join(d.cfg.DataDir, "codecs") }

func (d *daemon) jobDir(id string) string       { return filepath.Join(d.jobsRoot(), id) }
func (d *daemon) codecsDir(build string) string { return filepath.Join(d.codecsRoot(), build) }
func (d *daemon) masterURL() string             { return strings.TrimSuffix(d.cfg.MasterURL, "/") }

// proxyBase is the gating-proxy relay base substituted for {{PMS}}.
func (d *daemon) proxyBase(jobID string) string {
	host, port, err := net.SplitHostPort(d.cfg.ProxyListen)
	if err != nil {
		host, port = "127.0.0.1", fmt.Sprint(protocol.PortWorkerProxy)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/relay/" + jobID
}

// serve runs both listeners plus the background loops until sigCtx fires
// or a listener fails.
func (d *daemon) serve(sigCtx context.Context) error {
	for _, dir := range []string{d.jobsRoot(), d.codecsRoot()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Explicit TCP keepalive on the API listener: the events stream is the
	// job lifeline and must detect half-open peers.
	lc := &net.ListenConfig{KeepAlive: protocol.HeartbeatInterval}
	apiLn, err := lc.Listen(d.ctx, "tcp", d.cfg.Listen)
	if err != nil {
		return fmt.Errorf("workerd: listen %s: %w", d.cfg.Listen, err)
	}
	proxyLn, err := lc.Listen(d.ctx, "tcp", d.cfg.ProxyListen)
	if err != nil {
		_ = apiLn.Close()
		return fmt.Errorf("workerd: listen %s: %w", d.cfg.ProxyListen, err)
	}
	// No global write timeouts: events streams and held announces are
	// long-lived by design.
	apiSrv := &http.Server{Handler: d.apiHandler(), ReadHeaderTimeout: 10 * time.Second}
	proxySrv := &http.Server{Handler: d.proxyHandler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- apiSrv.Serve(apiLn) }()
	go func() { errc <- proxySrv.Serve(proxyLn) }()
	go d.reapLoop()
	go d.eaeSupervisor()
	d.logf("listening on %s (api) and %s (gating proxy); plex build %s; max jobs %d",
		d.cfg.Listen, d.cfg.ProxyListen, d.plexBuild, d.cfg.MaxJobs)

	var serveErr error
	select {
	case <-sigCtx.Done():
		d.logf("shutting down")
	case serveErr = <-errc:
	}
	d.cancel() // kills transcoders (procCtx children) and the EAE
	sctx, sdone := context.WithTimeout(context.Background(), 8*time.Second)
	defer sdone()
	_ = apiSrv.Shutdown(sctx)
	_ = proxySrv.Shutdown(sctx)
	_ = apiSrv.Close()
	_ = proxySrv.Close()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// apiHandler wires the :32500 routes; everything except GET /v1/health is
// behind bearer auth.
func (d *daemon) apiHandler() http.Handler {
	authed := http.NewServeMux()
	authed.HandleFunc("POST /v1/jobs", d.handleCreateJob)
	authed.HandleFunc("GET /v1/jobs/{id}/events", d.handleJobEvents)
	authed.HandleFunc("DELETE /v1/jobs/{id}", d.handleDeleteJob)
	authed.HandleFunc("POST /v1/provision/{build}", d.handleProvision)

	root := http.NewServeMux()
	root.HandleFunc("GET /v1/health", d.handleHealth)
	root.Handle("/", authtok.Middleware(d.token, authed))
	return root
}

func (d *daemon) getJob(id string) *job {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.jobs[id]
}

func (d *daemon) activeJobsLocked() int {
	n := 0
	for _, j := range d.jobs {
		j.mu.Lock()
		if !isTerminal(j.state) {
			n++
		}
		j.mu.Unlock()
	}
	return n
}

func (d *daemon) activeJobs() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeJobsLocked()
}

// handleHealth implements GET /v1/health (unauthenticated).
func (d *daemon) handleHealth(w http.ResponseWriter, r *http.Request) {
	codecs := d.codecsReady()
	h := protocol.Health{
		Status:      protocol.HealthOK,
		PlexBuild:   d.plexBuild,
		ActiveJobs:  d.activeJobs(),
		MaxJobs:     d.cfg.MaxJobs,
		VaapiOK:     d.vaapiOK(),
		CodecsReady: codecs,
	}
	if !h.VaapiOK || !codecs[d.plexBuild] {
		h.Status = protocol.HealthDegraded
	}
	writeJSON(w, http.StatusOK, h)
}

// codecsReady maps every warm codec-cache build to true; the worker's own
// build is always present in the map (false when cold).
func (d *daemon) codecsReady() map[string]bool {
	ready := map[string]bool{d.plexBuild: false}
	entries, err := os.ReadDir(d.codecsRoot())
	if err != nil {
		return ready
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasSuffix(e.Name(), ".partial") {
			ready[e.Name()] = true
		}
	}
	return ready
}

// handleCreateJob implements POST /v1/jobs: admission (capacity 429,
// version 409, cold codecs 503 + background provision kick), then job
// creation and the async PREPARING -> RUNNING lifecycle.
func (d *daemon) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req protocol.JobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "decode job request: "+err.Error())
		return
	}
	switch {
	case !validID(req.JobID):
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid job_id")
		return
	case !validID(req.PlexBuild):
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid plex_build")
		return
	case len(req.Argv) < 2:
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "argv too short")
		return
	case req.Session.PushURL == "" || req.Session.PushToken == "" || req.MasterPMS == "":
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "missing session push_url/push_token/master_pms")
		return
	}

	d.mu.Lock()
	if _, dup := d.jobs[req.JobID]; dup {
		d.mu.Unlock()
		writeError(w, http.StatusConflict, "DUPLICATE_JOB", "job "+req.JobID+" already exists")
		return
	}
	if d.activeJobsLocked() >= d.cfg.MaxJobs {
		d.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, protocol.ErrAtCapacity,
			fmt.Sprintf("worker at max_jobs (%d)", d.cfg.MaxJobs))
		return
	}
	if !buildsMatch(req.PlexBuild, d.plexBuild) {
		d.mu.Unlock()
		writeError(w, http.StatusConflict, protocol.ErrVersionMismatch,
			fmt.Sprintf("worker build %s, job build %s", d.plexBuild, req.PlexBuild))
		return
	}
	if !dirExists(d.codecsDir(req.PlexBuild)) {
		d.mu.Unlock()
		// Fast 503 so the shim falls back local NOW; warm the cache in the
		// background so the NEXT job goes remote.
		build := req.PlexBuild
		go func() {
			if err := d.provisionBuild(d.ctx, build); err != nil {
				d.logf("background provision %s: %v", build, err)
			}
		}()
		writeError(w, http.StatusServiceUnavailable, protocol.ErrNotReady,
			"codec cache cold for build "+req.PlexBuild+"; provisioning")
		return
	}
	j, err := newJob(d, req)
	if err != nil {
		d.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	d.jobs[req.JobID] = j
	d.mu.Unlock()

	go j.run()
	writeJSON(w, http.StatusCreated, map[string]any{
		"job_id": req.JobID,
		"state":  protocol.JobPending,
	})
}

// handleJobEvents implements GET /v1/jobs/{id}/events: the NDJSON lifeline.
func (d *daemon) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	j := d.getJob(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "unknown job")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	nw := ndjson.NewWriter(w)
	stopHB := nw.StartHeartbeats(ctx, protocol.HeartbeatInterval)
	defer stopHB()
	ch, unsub := j.subscribeEvents()
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return // client gone; lifeline accounting via unsub
		case ev, ok := <-ch:
			if !ok {
				return // job terminal, exit event delivered
			}
			if err := nw.WriteEvent(ev); err != nil {
				return
			}
		}
	}
}

// handleDeleteJob implements DELETE /v1/jobs/{id}: graceful cancel,
// idempotent (unknown ids 204 too -- the job may already be reaped).
func (d *daemon) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if j := d.getJob(r.PathValue("id")); j != nil {
		j.requestCancel(protocol.JobCancelled)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProvision implements POST /v1/provision/{build}: synchronous
// pre-warm of the codec cache (deploy-time, not hot path).
func (d *daemon) handleProvision(w http.ResponseWriter, r *http.Request) {
	build := r.PathValue("build")
	if !validID(build) {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid build")
		return
	}
	if err := d.provisionBuild(r.Context(), build); err != nil {
		writeError(w, http.StatusBadGateway, "PROVISION_FAILED", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reapLoop removes terminal job dirs older than reapAge (and their map
// entries), plus orphan dirs left by a previous crash.
func (d *daemon) reapLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.reapOnce(time.Now())
		}
	}
}

func (d *daemon) reapOnce(now time.Time) {
	d.mu.Lock()
	var expired []*job
	for id, j := range d.jobs {
		j.mu.Lock()
		gone := isTerminal(j.state) && now.Sub(j.terminalAt) >= reapAge
		j.mu.Unlock()
		if gone {
			expired = append(expired, j)
			delete(d.jobs, id)
		}
	}
	d.mu.Unlock()
	for _, j := range expired {
		if err := os.RemoveAll(j.dir); err != nil {
			d.logf("reap %s: %v", j.dir, err)
		}
	}
	// Orphans: job dirs with no in-memory job (crash leftovers).
	entries, err := os.ReadDir(d.jobsRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if d.getJob(e.Name()) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < reapAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.jobsRoot(), e.Name())); err != nil {
			d.logf("reap orphan %s: %v", e.Name(), err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(protocol.ErrorBody{Error: code, Message: msg})
}
