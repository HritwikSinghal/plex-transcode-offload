// Package shim is the master-side "Plex Transcoder" replacement: it
// classifies the job, dispatches it to a worker via prt-masterd, mirrors
// the remote transcoder, and execs the local fallback on any pre-segment
// failure.
package shim

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/authtok"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/config"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/ndjson"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// Fixed sub-timeouts of the remote attempt (design section 10). The
// pre-dispatch ones are additionally bounded by the overall spawn budget
// (config.ShimConfig.SpawnBudgetMS, default 1500ms).
const (
	registerTimeout = 500 * time.Millisecond // POST /v1/sessions (localhost)
	dispatchTimeout = time.Second            // POST /v1/jobs (LAN)
	cancelTimeout   = time.Second            // DELETE /v1/jobs/{id} (SIGTERM path)
	cleanupTimeout  = 500 * time.Millisecond // best-effort session teardown

	// The events stream outlives the spawn budget once established; only
	// its setup is bounded.
	streamDialTimeout   = 2 * time.Second
	streamHeaderTimeout = 5 * time.Second
)

// mediaURLTTL is the validity window of signed media URLs. ffmpeg re-opens
// the input on Range seeks throughout the job, so this must comfortably
// exceed any plausible transcode runtime.
const mediaURLTTL = 12 * time.Hour

// exitSigterm mimics the shell convention for a SIGTERM death (128+15) --
// the real transcoder would exit the same way.
const exitSigterm = 143

// maxBodySize bounds JSON response bodies read from the daemons.
const maxBodySize = 1 << 20

// Run executes the shim role with the transcoder argv (everything after
// argv[0]) and returns the process exit code.
func Run(args []string) int {
	if classifyLocal(args) {
		return execLocal(args)
	}
	if code, handled := tryRemote(args); handled {
		return code
	}
	return execLocal(args)
}

// execLocal replaces this process with the real transcoder, passing the
// ORIGINAL argv and env through unchanged. The .orig binary sits next to the
// shim symlink: the master nix module does
// `mv "Plex Transcoder" "Plex Transcoder.orig"` before linking the shim in.
// Returns (an error exit code) only if the exec itself fails.
func execLocal(args []string) int {
	orig, err := origTranscoderPath()
	if err != nil {
		logf("cannot resolve local transcoder: %v", err)
		return 1
	}
	argv := append([]string{orig}, args...)
	if err := syscall.Exec(orig, argv, os.Environ()); err != nil {
		logf("exec %s: %v", orig, err)
	}
	return 1
}

// origTranscoderPath is os.Args[0] + ".orig", with a relative argv[0]
// resolved against the cwd. Symlinks are deliberately NOT resolved: the
// .orig binary is a sibling of the shim symlink, not of its target.
func origTranscoderPath() (string, error) {
	p, err := absArgv0()
	if err != nil {
		return "", err
	}
	return p + ".orig", nil
}

func absArgv0() (string, error) {
	p := os.Args[0]
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, p), nil
}

// tryRemote attempts the remote dispatch. handled=false means "fall back to
// the local transcoder with the original argv" -- returned for EVERY failure
// before the first event arrives on the lifeline. After the first event the
// job is owned remotely and failures map to exit codes instead.
func tryRemote(args []string) (exitCode int, handled bool) {
	cfg, err := config.LoadShim()
	if err != nil {
		return failLocal(err)
	}
	token, err := authtok.LoadToken(os.Getenv(config.EnvTokenFile))
	if err != nil {
		return failLocal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return failLocal(err)
	}
	argv0, err := absArgv0()
	if err != nil {
		return failLocal(err)
	}
	jobID, err := randomJobID()
	if err != nil {
		return failLocal(err)
	}

	budgetCtx, cancelBudget := context.WithTimeout(context.Background(),
		time.Duration(cfg.SpawnBudgetMS)*time.Millisecond)
	defer cancelBudget()

	cl := &apiClient{hc: &http.Client{}, token: token}
	masterd := strings.TrimRight(cfg.MasterdURL, "/")

	// 1. Register the segment-push session with masterd (localhost).
	regCtx, cancelReg := context.WithTimeout(budgetCtx, registerTimeout)
	var reg protocol.RegisterSessionResponse
	status, err := cl.doJSON(regCtx, http.MethodPost, masterd+"/v1/sessions",
		protocol.RegisterSessionRequest{JobID: jobID, TargetDir: cwd}, &reg)
	cancelReg()
	if err != nil {
		return failLocal(fmt.Errorf("register session: %w", err))
	}
	if status/100 != 2 {
		return failLocal(fmt.Errorf("register session: HTTP %d", status))
	}
	sessionURL := masterd + "/v1/sessions/" + jobID

	// 2. Pick a worker from masterd's cached health (no per-spawn probes).
	var workers protocol.WorkersResponse
	status, err = cl.doJSON(budgetCtx, http.MethodGet, masterd+"/v1/workers", nil, &workers)
	if err != nil || status/100 != 2 {
		deleteSession(cl, sessionURL)
		return failLocal(fmt.Errorf("list workers: HTTP %d, %v", status, err))
	}
	workerURL := pickWorker(workers.Workers)
	if workerURL == "" {
		deleteSession(cl, sessionURL)
		return failLocal(errors.New("no healthy worker with capacity"))
	}

	// 3. Build the self-contained job request (argv rewrite + media map).
	rw := newRewriter(cwd, filepath.Dir(argv0), reg.AdvertiseURL, token,
		time.Now().Add(mediaURLTTL))
	jobReq := protocol.JobRequest{
		JobID:     jobID,
		PlexBuild: plexBuild(filepath.Dir(argv0)),
		Argv:      rw.rewriteArgs(args),
		Media:     rw.media,
		Env:       jobEnv(),
		Session: protocol.SessionInfo{
			// The PMS session dir is named after the session; its basename
			// is the closest stable id the shim has without parsing argv.
			ID:        filepath.Base(cwd),
			PushURL:   strings.TrimRight(reg.AdvertiseURL, "/") + "/v1/sessions/" + jobID,
			PushToken: reg.PushToken,
		},
		MasterPMS: strings.TrimRight(reg.AdvertiseURL, "/") + "/relay/" + jobID,
	}

	// SIGTERM trap (design section 6): installed before we own a remote job.
	// SIGKILL -- the normal PMS stop path -- is untrappable; the events
	// lifeline covers it worker-side.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// 4. Dispatch. 409/429/503/timeout all land here as non-2xx or error.
	jobCtx, cancelJob := context.WithTimeout(budgetCtx, dispatchTimeout)
	status, err = cl.doJSON(jobCtx, http.MethodPost, workerURL+"/v1/jobs", jobReq, nil)
	cancelJob()
	if err != nil || status/100 != 2 {
		deleteSession(cl, sessionURL)
		return failLocal(fmt.Errorf("dispatch to %s: HTTP %d, %v", workerURL, status, err))
	}
	jobURL := workerURL + "/v1/jobs/" + jobID

	// 5. Open the events lifeline and mirror the remote transcoder.
	resp, err := openEvents(token, jobURL+"/events")
	if err != nil {
		cancelRemoteJob(cl, jobURL)
		deleteSession(cl, sessionURL)
		return failLocal(fmt.Errorf("open events: %w", err))
	}
	defer resp.Body.Close()
	return runEventLoop(cl, resp.Body, jobURL, sessionURL, sigCh)
}

// runEventLoop consumes the NDJSON lifeline: stderr events are mirrored,
// state/heartbeat events only feed liveness, the exit event terminates the
// shim with the remote exit code. Lifeline loss after the first event means
// worker lost (exit 75); before the first event it is still a clean local
// fallback.
func runEventLoop(cl *apiClient, body io.Reader, jobURL, sessionURL string, sigCh <-chan os.Signal) (int, bool) {
	reader := ndjson.NewReader(body)
	liveCtx := reader.WatchLiveness(context.Background(), protocol.PeerTimeout)

	type readResult struct {
		ev  protocol.Event
		err error
	}
	events := make(chan readResult, 4)
	go func() {
		for {
			ev, err := reader.ReadEvent()
			events <- readResult{ev, err}
			if err != nil {
				return
			}
		}
	}()

	gotEvent := false
	for {
		select {
		case res := <-events:
			if res.err != nil {
				if !gotEvent {
					cancelRemoteJob(cl, jobURL)
					deleteSession(cl, sessionURL)
					return failLocal(fmt.Errorf("events stream: %w", res.err))
				}
				logf("worker lost (events stream: %v)", res.err)
				return protocol.ExitWorkerLost, true
			}
			gotEvent = true
			switch res.ev.Type {
			case protocol.EventStderr:
				fmt.Fprintln(os.Stderr, res.ev.Line)
			case protocol.EventExit:
				deleteSession(cl, sessionURL)
				if res.ev.Code != nil {
					return *res.ev.Code, true
				}
				logf("exit event without code; reporting failure")
				return 1, true
			}
		case <-liveCtx.Done():
			if !gotEvent {
				cancelRemoteJob(cl, jobURL)
				deleteSession(cl, sessionURL)
				return failLocal(errors.New("no events within peer timeout"))
			}
			logf("worker lost (lifeline idle > %s)", protocol.PeerTimeout)
			return protocol.ExitWorkerLost, true
		case <-sigCh:
			cancelRemoteJob(cl, jobURL)
			return exitSigterm, true
		}
	}
}

// openEvents GETs the job events stream with its own client: the stream must
// outlive the spawn budget, so only dial and response-header are bounded.
func openEvents(token, url string) (*http.Response, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: streamDialTimeout}).DialContext,
		ResponseHeaderTimeout: streamHeaderTimeout,
	}}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// pickWorker returns the first healthy worker with spare capacity
// (first-healthy now; a least-loaded comparator is a later one-liner).
func pickWorker(ws []protocol.WorkerStatus) string {
	for _, w := range ws {
		if w.Healthy && w.Health.ActiveJobs < w.Health.MaxJobs {
			return w.URL
		}
	}
	return ""
}

// plexBuild best-effort derives the master PMS build string for the job's
// version pin. Sources, in order:
//  1. PLEX_MEDIA_SERVER_INFO_VERSION -- PMS exports its version into
//     transcoder children's env;
//  2. the version suffix of the "...plexmediaserver-<version>" store-path
//     component of the install dir (nix store names carry the full version);
//  3. the basename of FFMPEG_EXTERNAL_LIBS (the per-build Codecs dir name).
//
// A wrong or empty value is safe: the worker answers 409 VERSION_MISMATCH
// and the shim falls back local.
func plexBuild(plexDir string) string {
	if v := os.Getenv("PLEX_MEDIA_SERVER_INFO_VERSION"); v != "" {
		return v
	}
	const marker = "plexmediaserver-"
	for p := plexDir; p != "/" && p != "."; p = filepath.Dir(p) {
		base := filepath.Base(p)
		if i := strings.Index(base, marker); i >= 0 {
			return base[i+len(marker):]
		}
	}
	if libs := os.Getenv("FFMPEG_EXTERNAL_LIBS"); libs != "" {
		return filepath.Base(filepath.Clean(libs))
	}
	return ""
}

// jobEnv is os.Environ() minus the master-local trio the worker always
// resets per design section 3.7: FFMPEG_EXTERNAL_LIBS, EAE_ROOT,
// LD_LIBRARY_PATH, HOME and LIBVA_*.
func jobEnv() map[string]string {
	drop := map[string]bool{
		"FFMPEG_EXTERNAL_LIBS": true,
		"EAE_ROOT":             true,
		"LD_LIBRARY_PATH":      true,
		"HOME":                 true,
	}
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || drop[k] || strings.HasPrefix(k, "LIBVA_") {
			continue
		}
		env[k] = v
	}
	return env
}

// randomJobID returns 16 random bytes hex-encoded (32 chars).
func randomJobID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// apiClient is a minimal bearer-authed JSON client for the two daemons.
type apiClient struct {
	hc    *http.Client
	token string
}

// doJSON sends in (when non-nil) as a JSON body and decodes a 2xx response
// into out (when non-nil). Returns the HTTP status (0 on transport error).
func (c *apiClient) doJSON(ctx context.Context, method, url string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodySize)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
		return resp.StatusCode, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodySize))
	return resp.StatusCode, nil
}

// cancelRemoteJob best-effort DELETEs the worker job (graceful cancel).
func cancelRemoteJob(cl *apiClient, jobURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	_, _ = cl.doJSON(ctx, http.MethodDelete, jobURL, nil, nil)
}

// deleteSession best-effort closes the masterd session.
func deleteSession(cl *apiClient, sessionURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_, _ = cl.doJSON(ctx, http.MethodDelete, sessionURL, nil, nil)
}

// failLocal logs why the remote attempt is abandoned and signals the caller
// to exec the local fallback.
func failLocal(err error) (int, bool) {
	logf("falling back local: %v", err)
	return 0, false
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "prt-shim: "+format+"\n", args...)
}
