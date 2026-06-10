package workerd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// pushRetries is the number of RETRIES after the first attempt.
	pushRetries = 3
	// pushBackoffStart doubles per retry, capped at pushBackoffCap.
	pushBackoffStart = 500 * time.Millisecond
	pushBackoffCap   = 5 * time.Second
	// pushAttemptTimeout bounds one PUT (a segment is a few MB on a LAN).
	pushAttemptTimeout = 30 * time.Second
)

// permanentPushError marks a push failure that retrying cannot fix
// (session tombstoned, file gone).
type permanentPushError struct{ err error }

func (e *permanentPushError) Error() string { return e.err.Error() }
func (e *permanentPushError) Unwrap() error { return e.err }

// startPushers wires the segment plane: the inotify watcher on the out dir
// (started BEFORE the transcoder spawns, so no completion is missed), the
// bounded upload queue, and cfg.push_parallel upload workers sharing the
// daemon's keep-alive HTTP client.
func (j *job) startPushers() error {
	w, err := newDirWatcher(j.outDir)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.watcher = w
	j.queue = make(chan string, j.d.cfg.PushQueueCap)
	j.mu.Unlock()

	go func() {
		for {
			select {
			case name, ok := <-w.Events:
				if !ok {
					// Watcher closed; surface a pending fatal error if any.
					select {
					case werr := <-w.Errors:
						j.abortFail("segment watcher: " + werr.Error())
					default:
					}
					return
				}
				j.enqueue(name)
			case werr := <-w.Errors:
				j.abortFail("segment watcher: " + werr.Error())
				return
			}
		}
	}()
	for i := 0; i < j.d.cfg.PushParallel; i++ {
		go j.uploadWorker()
	}
	return nil
}

// enqueue adds a completed file to the upload queue. A full queue means
// masterd cannot keep up: the job FAILS rather than ballooning worker
// memory (the out dir is tmpfs).
func (j *job) enqueue(name string) {
	j.mu.Lock()
	if j.intakeClosed || j.failing || isTerminal(j.state) || j.acked[name] {
		j.mu.Unlock()
		return
	}
	overflow := false
	select {
	case j.queue <- name:
		j.pending++
	default:
		overflow = true
	}
	j.mu.Unlock()
	if overflow {
		j.abortFail(fmt.Sprintf("push queue overflow (cap %d)", j.d.cfg.PushQueueCap))
	}
}

// uploadWorker drains the queue: PUT with retries, then record the ACK and
// delete the local copy (worker disk holds in-flight files only).
func (j *job) uploadWorker() {
	for name := range j.queue {
		err := j.pushFile(name)
		j.mu.Lock()
		j.pending--
		if err == nil {
			j.acked[name] = true
		}
		j.cond.Broadcast()
		j.mu.Unlock()
		if err == nil {
			_ = os.Remove(filepath.Join(j.outDir, name))
		} else {
			j.abortFail(fmt.Sprintf("push %s: %v", name, err))
		}
	}
}

// pushFile PUTs one chunk to masterd, retrying transient failures.
func (j *job) pushFile(name string) error {
	target := strings.TrimSuffix(j.req.Session.PushURL, "/") + "/files/" + url.PathEscape(name)
	backoff := pushBackoffStart
	var lastErr error
	for attempt := 0; attempt <= pushRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-j.d.ctx.Done():
				return j.d.ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, pushBackoffCap)
		}
		err := j.putOnce(target, name)
		if err == nil {
			return nil
		}
		var perm *permanentPushError
		if errors.As(err, &perm) {
			return err
		}
		lastErr = err
	}
	return lastErr
}

func (j *job) putOnce(target, name string) error {
	f, err := os.Open(filepath.Join(j.outDir, name))
	if err != nil {
		return &permanentPushError{err}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return &permanentPushError{err}
	}
	ctx, cancel := context.WithTimeout(j.d.ctx, pushAttemptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, f)
	if err != nil {
		return &permanentPushError{err}
	}
	req.ContentLength = st.Size()
	req.Header.Set("Authorization", "Bearer "+j.req.Session.PushToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := j.d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusGone:
		// Session tombstoned by masterd: the shim is gone for good.
		return &permanentPushError{fmt.Errorf("PUT %s: session gone (410)", target)}
	default:
		return fmt.Errorf("PUT %s: status %d", target, resp.StatusCode)
	}
}

// drainUploads (DRAINING) closes intake and waits for every enqueued upload
// to resolve, bounded by timeout. Cancelled jobs drain too -- chunks
// already produced still reach masterd; only announces are gated off.
func (j *job) drainUploads(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		j.mu.Lock()
		j.cond.Broadcast()
		j.mu.Unlock()
	})
	defer timer.Stop()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.intakeClosed = true
	for j.pending > 0 && !j.failing && time.Now().Before(deadline) {
		j.cond.Wait()
	}
	switch {
	case j.failing:
		return errors.New(j.failMsg)
	case j.pending > 0:
		return fmt.Errorf("timed out with %d uploads in flight", j.pending)
	}
	return nil
}
