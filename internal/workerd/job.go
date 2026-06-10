package workerd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

const (
	// cancelGrace is the SIGTERM -> SIGKILL grace on job cancellation.
	cancelGrace = 5 * time.Second
	// settleDelay lets the final inotify events flow after transcoder exit
	// before the watcher is closed and the upload queue drained.
	settleDelay = 250 * time.Millisecond
	// drainTimeout bounds the DRAINING upload flush.
	drainTimeout = 30 * time.Second
	// spoolTimeout bounds the download of ONE spooled aux file (small:
	// subtitles, fonts).
	spoolTimeout = 15 * time.Second
	// reapAge is how long terminal job dirs survive before the reaper
	// removes them.
	reapAge = 5 * time.Minute
)

// errGateBroken is returned by gateWait when the job is failing or
// cancelled: held announces must 502, never forward.
var errGateBroken = errors.New("job is failing; announce dropped")

// job is one dispatched transcode: state machine
// PENDING -> PREPARING -> RUNNING -> DRAINING -> terminal
// (COMPLETED / FAILED / CANCELLED / LOST).
type job struct {
	d        *daemon
	id       string
	req      protocol.JobRequest
	dir      string // <data_dir>/jobs/<id>; also the job's HOME
	outDir   string // transcoder cwd + segment output
	spoolDir string // spooled aux media

	// procCtx scopes the transcoder process; procCancel triggers
	// exec.Cmd.Cancel (SIGTERM to the process group).
	procCtx    context.Context
	procCancel context.CancelFunc

	hub *eventHub

	mu   sync.Mutex
	cond *sync.Cond // broadcast on: ack recorded, pending change, abort, terminal

	state       protocol.JobState
	failing     bool // push pipeline broken or fatal error: gates bail, drain aborts
	failMsg     string
	cancelState protocol.JobState // CANCELLED or LOST once a cancel was requested ("" otherwise)

	acked        map[string]bool // chunk basenames ACKed by masterd
	pending      int             // uploads enqueued and not yet resolved
	intakeClosed bool            // no further enqueues (draining/terminal)
	queueClosed  bool

	cmd    *exec.Cmd
	exited bool // cmd.Wait returned

	clients    int       // connected events-stream clients
	clientGone time.Time // when clients last dropped to zero (or RUNNING began with none)
	terminalAt time.Time

	queue   chan string
	watcher *dirWatcher
}

// newJob creates the job dirs and the job in PENDING. The caller registers
// it and starts run().
func newJob(d *daemon, req protocol.JobRequest) (*job, error) {
	dir := d.jobDir(req.JobID)
	// A stale dir with the same id (crash leftover not yet reaped) must not
	// leak old chunks into the new job's gate.
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("workerd: clean job dir: %w", err)
	}
	j := &job{
		d:        d,
		id:       req.JobID,
		req:      req,
		dir:      dir,
		outDir:   filepath.Join(dir, "out"),
		spoolDir: filepath.Join(dir, "spool"),
		hub:      newEventHub(),
		state:    protocol.JobPending,
		acked:    make(map[string]bool),
	}
	j.cond = sync.NewCond(&j.mu)
	j.procCtx, j.procCancel = context.WithCancel(d.ctx)
	for _, p := range []string{j.outDir, j.spoolDir} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return nil, fmt.Errorf("workerd: create job dir: %w", err)
		}
	}
	j.hub.publish(protocol.Event{Type: protocol.EventState, State: protocol.JobPending})
	return j, nil
}

// run drives the job from PREPARING to a terminal state. It runs in its own
// goroutine; every exit path ends in finalize.
func (j *job) run() {
	j.setState(protocol.JobPreparing)
	// Top up the codec cache against the master's CURRENT manifest: PMS
	// downloads decoders on demand, so this session may need a lib the
	// master acquired after our last sync. Best-effort -- a sync error
	// means running with the cached set, which at worst reproduces the
	// fast transcoder failure the sync exists to prevent.
	syncCtx, cancelSync := context.WithTimeout(j.procCtx, codecSyncWait)
	if err := j.d.provisionBuild(syncCtx, j.req.PlexBuild); err != nil {
		j.d.logf("job %s: codec sync: %v (continuing with cached set)", j.id, err)
	}
	cancelSync()
	argv, env, err := j.prepare()
	if err != nil {
		j.publishStderr("prt: prepare failed: " + err.Error())
		j.finalize(j.terminalFor(true), 1)
		return
	}
	if st, aborted := j.abortState(); aborted {
		j.finalize(st, 1)
		return
	}
	if err := j.startPushers(); err != nil {
		j.publishStderr("prt: segment watcher failed: " + err.Error())
		j.finalize(j.terminalFor(true), 1)
		return
	}
	if err := j.startProcess(argv, env); err != nil {
		j.publishStderr("prt: spawn failed: " + err.Error())
		j.finalize(j.terminalFor(true), 1)
		return
	}
	j.setState(protocol.JobRunning)
	go j.monitorLifeline()

	werr := j.cmd.Wait()
	j.mu.Lock()
	j.exited = true
	j.mu.Unlock()
	code := exitCodeOf(werr)

	time.Sleep(settleDelay) // let trailing inotify events reach the queue
	_ = j.watcher.Close()
	j.setState(protocol.JobDraining)
	drainErr := j.drainUploads(drainTimeout)
	if drainErr != nil {
		j.publishStderr("prt: upload drain: " + drainErr.Error())
	}

	j.mu.Lock()
	final := protocol.JobCompleted
	switch {
	case j.cancelState != "":
		final = j.cancelState
	case j.failing || drainErr != nil || code != 0:
		final = protocol.JobFailed
	}
	j.mu.Unlock()
	j.finalize(final, code)
}

// prepare implements PREPARING: spool mode=spool media, then resolve the
// argv placeholders and assemble the env. (The codec cache was already
// verified warm at admission.)
func (j *job) prepare() (argv, env []string, err error) {
	if len(j.req.Argv) < 2 {
		return nil, nil, fmt.Errorf("argv too short (%d tokens)", len(j.req.Argv))
	}
	values := make(map[string]string, len(j.req.Media))
	// Deterministic spool order (media keys are decimal indices).
	keys := make([]string, 0, len(j.req.Media))
	for k := range j.req.Media {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		idx, aerr := strconv.Atoi(k)
		if aerr != nil || idx < 0 {
			return nil, nil, fmt.Errorf("bad media key %q", k)
		}
		ref := j.req.Media[k]
		ph := protocol.MediaPlaceholder(idx)
		switch ref.Mode {
		case protocol.MediaModeStream:
			values[ph] = ref.URL
		case protocol.MediaModeSpool:
			local, serr := j.spool(idx, ref.URL)
			if serr != nil {
				return nil, nil, fmt.Errorf("spool media %d: %w", idx, serr)
			}
			values[ph] = local
		default:
			return nil, nil, fmt.Errorf("media %d: unknown mode %q", idx, ref.Mode)
		}
	}
	// req.Argv carries only the transcoder ARGUMENTS: the shim strips the
	// program token (os.Args[1:]) before dispatch, and the worker spawns
	// its own cfg.transcoder_path. Slicing here would eat the first flag.
	argv, err = substituteArgv(j.req.Argv, j.outDir, j.d.proxyBase(j.id), j.d.cfg.PlexDir, values)
	if err != nil {
		return nil, nil, err
	}
	driverDir, _ := j.d.driverDriDir(j.req.PlexBuild)
	env = buildEnv(
		j.req.Env,
		j.dir,
		j.d.codecsDir(j.req.PlexBuild),
		j.d.cfg.EAERoot,
		filepath.Join(j.d.cfg.PlexDir, "lib"),
		driverDir,
	)
	return argv, env, nil
}

// spool downloads one small aux file (subtitle, font) into the job spool
// dir and returns its local path.
func (j *job) spool(idx int, rawURL string) (string, error) {
	ctx, cancel := context.WithTimeout(j.procCtx, spoolTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := j.d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	local := filepath.Join(j.spoolDir, fmt.Sprintf("m%d%s", idx, spoolExt(rawURL)))
	f, err := os.OpenFile(local, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return local, nil
}

// spoolExt preserves the original file extension (libass and font loaders
// sniff it) from the masterd media URL: the ?path= query param if present,
// else the URL path.
func spoolExt(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if p := u.Query().Get("path"); p != "" {
		return filepath.Ext(p)
	}
	return filepath.Ext(u.Path)
}

// startProcess spawns the real transcoder: cwd = out dir, stderr mirrored
// to the events stream, own process group (SIGTERM/SIGKILL must reach
// spawned helpers too).
func (j *job) startProcess(argv, env []string) error {
	cmd := exec.CommandContext(j.procCtx, j.d.cfg.TranscoderPath, argv...)
	cmd.Dir = j.outDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = cancelGrace + time.Second
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	j.mu.Lock()
	j.cmd = cmd
	j.mu.Unlock()
	go j.mirrorStderr(stderr)
	return nil
}

func (j *job) mirrorStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		j.publishStderr(sc.Text())
	}
}

func (j *job) publishStderr(line string) {
	j.hub.publish(protocol.Event{Type: protocol.EventStderr, Line: line})
}

// exitCodeOf maps cmd.Wait's error to the exit code carried on the exit
// event (128+signal for signal deaths, shell convention).
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
	}
	return 1
}

// setState advances a non-terminal job and publishes the state event.
func (j *job) setState(s protocol.JobState) {
	j.mu.Lock()
	if isTerminal(j.state) {
		j.mu.Unlock()
		return
	}
	j.state = s
	if s == protocol.JobRunning && j.clients == 0 {
		// The lifeline clock starts at RUNNING even if no client ever
		// connected.
		j.clientGone = time.Now()
	}
	j.cond.Broadcast()
	j.mu.Unlock()
	j.hub.publish(protocol.Event{Type: protocol.EventState, State: s})
}

// finalize moves the job to its terminal state, emits the exit event,
// closes the hub and the upload queue, and (for CANCELLED/LOST) removes the
// job dir immediately; COMPLETED/FAILED dirs are left for the reaper.
func (j *job) finalize(state protocol.JobState, code int) {
	j.mu.Lock()
	if isTerminal(j.state) {
		j.mu.Unlock()
		return
	}
	j.state = state
	j.terminalAt = time.Now()
	j.intakeClosed = true
	closeQueue := j.queue != nil && !j.queueClosed
	if closeQueue {
		j.queueClosed = true
	}
	j.cond.Broadcast()
	j.mu.Unlock()

	j.hub.publish(protocol.Event{Type: protocol.EventState, State: state})
	c := code
	j.hub.publish(protocol.Event{Type: protocol.EventExit, Code: &c})
	j.hub.close()
	if j.watcher != nil {
		_ = j.watcher.Close()
	}
	if closeQueue {
		close(j.queue)
	}
	if state == protocol.JobCancelled || state == protocol.JobLost {
		_ = os.RemoveAll(j.dir)
	}
	j.d.logf("job %s: %s (exit %d)%s", j.id, state, code, j.failSuffix())
}

func (j *job) failSuffix() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failMsg == "" {
		return ""
	}
	return ": " + j.failMsg
}

// abortFail marks the job as failing (push pipeline broken, fatal internal
// error): gate waiters bail, the drain aborts, and the transcoder is
// killed. run() then finalizes with FAILED.
func (j *job) abortFail(msg string) {
	j.mu.Lock()
	if j.failing || isTerminal(j.state) {
		j.mu.Unlock()
		return
	}
	j.failing = true
	j.failMsg = msg
	j.cond.Broadcast()
	j.mu.Unlock()
	j.d.logf("job %s: failing: %s", j.id, msg)
	j.publishStderr("prt: " + msg)
	j.killProcess()
}

// requestCancel asks for a graceful cancel (DELETE -> CANCELLED, lifeline
// lost -> LOST): SIGTERM, cancelGrace, SIGKILL. Uploads still drain --
// gates bail immediately. Idempotent.
func (j *job) requestCancel(target protocol.JobState) {
	j.mu.Lock()
	if j.cancelState != "" || isTerminal(j.state) {
		j.mu.Unlock()
		return
	}
	j.cancelState = target
	j.cond.Broadcast()
	j.mu.Unlock()
	j.d.logf("job %s: cancel requested (-> %s)", j.id, target)
	j.killProcess()
}

// killProcess triggers procCtx cancellation (exec.Cmd.Cancel SIGTERMs the
// process group) and schedules a group SIGKILL after the grace period.
func (j *job) killProcess() {
	j.procCancel()
	j.mu.Lock()
	pid := 0
	if j.cmd != nil && j.cmd.Process != nil && !j.exited {
		pid = j.cmd.Process.Pid
	}
	j.mu.Unlock()
	if pid == 0 {
		return
	}
	time.AfterFunc(cancelGrace, func() {
		j.mu.Lock()
		exited := j.exited
		j.mu.Unlock()
		if !exited {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}

// abortState reports whether the job was aborted pre-RUNNING and the
// terminal state to use.
func (j *job) abortState() (protocol.JobState, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.cancelState != "":
		return j.cancelState, true
	case j.failing:
		return protocol.JobFailed, true
	}
	return "", false
}

// terminalFor picks the terminal state for an early failure path,
// preferring an already-requested cancel state.
func (j *job) terminalFor(failed bool) protocol.JobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancelState != "" {
		return j.cancelState
	}
	if failed {
		return protocol.JobFailed
	}
	return protocol.JobCompleted
}

// monitorLifeline implements the implicit cancel: a RUNNING job whose
// events stream has had no connected client for protocol.PeerTimeout is
// LOST (the shim was SIGKILLed, or its connection died).
func (j *job) monitorLifeline() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-j.d.ctx.Done():
			return
		case <-ticker.C:
		}
		j.mu.Lock()
		if isTerminal(j.state) || j.cancelState != "" {
			j.mu.Unlock()
			return
		}
		lost := j.state == protocol.JobRunning &&
			j.clients == 0 &&
			time.Since(j.clientGone) >= protocol.PeerTimeout
		j.mu.Unlock()
		if lost {
			j.d.logf("job %s: events lifeline lost for %s; implicit cancel", j.id, protocol.PeerTimeout)
			j.requestCancel(protocol.JobLost)
			return
		}
	}
}

// subscribeEvents attaches an events-stream client: hub subscription plus
// lifeline accounting. The returned cancel is idempotent.
func (j *job) subscribeEvents() (<-chan protocol.Event, func()) {
	ch, unsub := j.hub.subscribe()
	j.mu.Lock()
	j.clients++
	j.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			unsub()
			j.mu.Lock()
			j.clients--
			if j.clients == 0 {
				j.clientGone = time.Now()
			}
			j.mu.Unlock()
		})
	}
}

// gateWait blocks until every name in names is ACKed by masterd -- THE
// chunk-before-announce invariant. Names that are neither ACKed, in
// flight, nor present in the out dir are skipped: they are not artifacts
// of this job (DASH template strings, false-positive matches). Returns
// errGateBroken when the job is failing or cancelled.
func (j *job) gateWait(ctx context.Context, names []string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Wake cond waiters when the announce's client (the blocked
	// transcoder) goes away.
	stop := context.AfterFunc(ctx, func() {
		j.mu.Lock()
		j.cond.Broadcast()
		j.mu.Unlock()
	})
	defer stop()
	for _, name := range names {
		for !j.acked[name] {
			if j.gateBrokenLocked() {
				return errGateBroken
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !fileExists(filepath.Join(j.outDir, name)) {
				break // not a local artifact of this job
			}
			j.cond.Wait()
		}
	}
	return nil
}

func (j *job) gateBrokenLocked() bool {
	return j.failing ||
		j.cancelState != "" ||
		(isTerminal(j.state) && j.state != protocol.JobCompleted)
}

func isTerminal(s protocol.JobState) bool {
	switch s {
	case protocol.JobCompleted, protocol.JobFailed, protocol.JobCancelled, protocol.JobLost:
		return true
	}
	return false
}
