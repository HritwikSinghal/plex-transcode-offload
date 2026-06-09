package ndjson

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

func TestWriteReadRoundTrip(t *testing.T) {
	code := 0
	events := []protocol.Event{
		{Type: protocol.EventState, State: protocol.JobRunning},
		{Type: protocol.EventStderr, Line: "frame=42 fps=30"},
		{Type: protocol.EventHeartbeat},
		{Type: protocol.EventExit, Code: &code},
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, ev := range events {
		if err := w.WriteEvent(ev); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "\n"); got != len(events) {
		t.Errorf("wrote %d lines, want %d", got, len(events))
	}

	r := NewReader(&buf)
	for i, want := range events {
		got, err := r.ReadEvent()
		if err != nil {
			t.Fatalf("ReadEvent[%d]: %v", i, err)
		}
		if got.Type != want.Type || got.State != want.State || got.Line != want.Line {
			t.Errorf("event[%d] = %+v, want %+v", i, got, want)
		}
		if (got.Code == nil) != (want.Code == nil) {
			t.Errorf("event[%d] code presence mismatch", i)
		} else if got.Code != nil && *got.Code != *want.Code {
			t.Errorf("event[%d] code = %d, want %d", i, *got.Code, *want.Code)
		}
	}
	if _, err := r.ReadEvent(); err != io.EOF {
		t.Errorf("after stream end err = %v, want io.EOF", err)
	}
}

type flushRecorder struct {
	bytes.Buffer
	flushes int
}

func (f *flushRecorder) Flush() { f.flushes++ }

func TestWriterFlushesPerEvent(t *testing.T) {
	var fr flushRecorder
	w := NewWriter(&fr)
	for range 3 {
		if err := w.WriteEvent(protocol.Event{Type: protocol.EventHeartbeat}); err != nil {
			t.Fatal(err)
		}
	}
	if fr.flushes != 3 {
		t.Errorf("flushes = %d, want 3", fr.flushes)
	}
}

func TestStartHeartbeats(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf) // bytes.Buffer is not a Flusher; exercises that path too

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := w.StartHeartbeats(ctx, 5*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	stop()
	wrote := buf.String() // safe: heartbeat goroutine has exited

	n := strings.Count(wrote, "\n")
	if n < 2 {
		t.Errorf("heartbeats written = %d, want >= 2; buf=%q", n, wrote)
	}
	r := NewReader(strings.NewReader(wrote))
	for {
		ev, err := r.ReadEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadEvent: %v", err)
		}
		if ev.Type != protocol.EventHeartbeat {
			t.Errorf("unexpected event type %q", ev.Type)
		}
	}
}

func TestReaderToleratesBlankLines(t *testing.T) {
	r := NewReader(strings.NewReader("\n{\"type\":\"heartbeat\"}\n\n"))
	ev, err := r.ReadEvent()
	if err != nil || ev.Type != protocol.EventHeartbeat {
		t.Errorf("got (%+v, %v)", ev, err)
	}
	if _, err := r.ReadEvent(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestReaderRejectsMalformedLine(t *testing.T) {
	r := NewReader(strings.NewReader("not json\n"))
	if _, err := r.ReadEvent(); err == nil {
		t.Error("expected error for malformed line")
	}
}

func TestReaderOversizedLine(t *testing.T) {
	huge := `{"type":"stderr","line":"` + strings.Repeat("x", MaxLineSize+1024) + `"}` + "\n"
	r := NewReader(strings.NewReader(huge))
	if _, err := r.ReadEvent(); !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("err = %v, want bufio.ErrTooLong", err)
	}
}

func TestLastEventAdvances(t *testing.T) {
	r := NewReader(strings.NewReader("{\"type\":\"heartbeat\"}\n"))
	before := r.LastEvent()
	time.Sleep(2 * time.Millisecond)
	if _, err := r.ReadEvent(); err != nil {
		t.Fatal(err)
	}
	if !r.LastEvent().After(before) {
		t.Error("LastEvent did not advance after a read")
	}
}

func TestWatchLivenessTimesOut(t *testing.T) {
	r := NewReader(strings.NewReader("")) // no traffic at all
	ctx := r.WatchLiveness(context.Background(), 20*time.Millisecond)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("liveness context not cancelled after timeout")
	}
}

func TestWatchLivenessParentCancel(t *testing.T) {
	r := NewReader(strings.NewReader(""))
	parent, cancel := context.WithCancel(context.Background())
	ctx := r.WatchLiveness(parent, time.Hour)
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("liveness context did not follow parent cancellation")
	}
}
