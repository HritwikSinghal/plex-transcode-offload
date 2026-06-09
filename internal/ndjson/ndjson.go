// Package ndjson implements the newline-delimited JSON event streams of the
// PRT protocol (GET /v1/jobs/{id}/events): one protocol.Event per line,
// heartbeats every protocol.HeartbeatInterval, peer considered lost after
// protocol.PeerTimeout without traffic.
//
// Writer is used by workerd (server side, writing the response body);
// Reader is used by the shim (client side, reading the response body). Any
// event -- heartbeats included -- counts as liveness traffic.
package ndjson

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// MaxLineSize bounds a single NDJSON line (generous: stderr lines and argv
// echoes are well under this). Reader returns bufio.ErrTooLong beyond it.
const MaxLineSize = 1 << 20 // 1 MiB

// Writer writes protocol.Events as NDJSON lines, flushing after every event
// when the underlying writer is an http.Flusher (heartbeats must reach the
// peer promptly, not sit in a buffer). Safe for concurrent use: the
// heartbeat goroutine and the event producer may interleave.
type Writer struct {
	mu      sync.Mutex
	enc     *json.Encoder
	flusher http.Flusher // nil if w does not support flushing
	err     error        // first write error; subsequent writes fail fast
}

// NewWriter wraps w. If w implements http.Flusher (an http.ResponseWriter
// does), every event is flushed to the wire as it is written.
func NewWriter(w io.Writer) *Writer {
	f, _ := w.(http.Flusher)
	return &Writer{enc: json.NewEncoder(w), flusher: f}
}

// WriteEvent writes one event as a single NDJSON line and flushes it. After
// the first error, every call returns that error.
func (w *Writer) WriteEvent(ev protocol.Event) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if err := w.enc.Encode(ev); err != nil { // Encode appends the newline
		w.err = err
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// StartHeartbeats starts a goroutine writing {"type":"heartbeat"} every
// interval until ctx is cancelled or a write fails. The returned stop
// function cancels it and waits for the goroutine to exit; calling stop is
// optional if ctx is cancelled anyway.
func (w *Writer) StartHeartbeats(ctx context.Context, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.WriteEvent(protocol.Event{Type: protocol.EventHeartbeat}); err != nil {
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// Reader reads protocol.Events from an NDJSON stream and tracks when the
// last event arrived (for liveness watching). Not safe for concurrent
// ReadEvent calls.
type Reader struct {
	scanner  *bufio.Scanner
	lastNano atomic.Int64 // unix nanos of the last successful read
}

// NewReader wraps r. Lines longer than MaxLineSize fail with
// bufio.ErrTooLong.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxLineSize)
	rd := &Reader{scanner: sc}
	rd.lastNano.Store(time.Now().UnixNano())
	return rd
}

// ReadEvent reads the next event. It blocks until a line arrives, the
// stream ends (io.EOF), or the underlying read fails. Heartbeat events are
// returned like any other so callers can observe traffic; skip them as
// needed.
func (r *Reader) ReadEvent() (protocol.Event, error) {
	for {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return protocol.Event{}, err
			}
			return protocol.Event{}, io.EOF
		}
		line := r.scanner.Bytes()
		if len(line) == 0 { // tolerate blank lines
			continue
		}
		var ev protocol.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return protocol.Event{}, err
		}
		r.lastNano.Store(time.Now().UnixNano())
		return ev, nil
	}
}

// LastEvent returns when the last event was successfully read (or when the
// Reader was created, before any event).
func (r *Reader) LastEvent() time.Time {
	return time.Unix(0, r.lastNano.Load())
}

// WatchLiveness returns a context that is cancelled with context.Canceled
// when no event has been read for at least timeout (the peer-lost
// condition; pass protocol.PeerTimeout), or when parent is done. Reads on
// the Reader (any event type, heartbeats included) reset the deadline.
//
// Typical shim use: derive the job's lifetime from this context and treat
// its cancellation -- without a prior terminal event -- as worker lost
// (exit protocol.ExitWorkerLost).
func (r *Reader) WatchLiveness(parent context.Context, timeout time.Duration) context.Context {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		defer cancel()
		for {
			idle := time.Since(r.LastEvent())
			if idle >= timeout {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(timeout - idle):
			}
		}
	}()
	return ctx
}
