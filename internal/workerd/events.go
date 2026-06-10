package workerd

import (
	"sync"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

const (
	// stderrRingSize bounds the per-job stderr history replayed to a late
	// events subscriber.
	stderrRingSize = 200
	// subBuffer is the per-subscriber event buffer. It exceeds the maximum
	// snapshot size (state history + stderr ring + exit), so a snapshot
	// never blocks.
	subBuffer = 1024
)

// eventHub fans a job's protocol.Events out to events-stream subscribers.
// A subscriber arriving late first receives a snapshot (state history, the
// stderr ring, and the exit event if the job is already terminal), then live
// events. Subscriber channels are closed when the hub closes -- the final
// event a client sees before EOF is the exit event.
type eventHub struct {
	mu     sync.Mutex
	subs   map[chan protocol.Event]struct{}
	states []protocol.Event
	stderr []protocol.Event
	exit   *protocol.Event
	closed bool
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[chan protocol.Event]struct{})}
}

// publish records ev in the appropriate history and delivers it to every
// subscriber. A subscriber too slow to take a non-stderr event (state/exit
// must never be silently lost) is dropped: its channel is closed, the
// handler returns, and the lifeline monitor takes over. Slow subscribers
// merely miss stderr lines.
func (h *eventHub) publish(ev protocol.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	switch ev.Type {
	case protocol.EventState:
		h.states = append(h.states, ev)
	case protocol.EventStderr:
		h.stderr = append(h.stderr, ev)
		if len(h.stderr) > stderrRingSize {
			h.stderr = h.stderr[len(h.stderr)-stderrRingSize:]
		}
	case protocol.EventExit:
		e := ev
		h.exit = &e
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			if ev.Type != protocol.EventStderr {
				delete(h.subs, ch)
				close(ch)
			}
		}
	}
}

// subscribe returns a channel pre-loaded with the snapshot plus a cancel
// function (idempotent, never closes the channel itself -- only the hub
// closes channels). If the hub is already closed the channel arrives closed
// after the snapshot.
func (h *eventHub) subscribe() (chan protocol.Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan protocol.Event, subBuffer)
	for _, ev := range h.states {
		ch <- ev
	}
	for _, ev := range h.stderr {
		ch <- ev
	}
	if h.exit != nil {
		ch <- *h.exit
	}
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	h.subs[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, ch)
	}
}

// close ends the hub: all subscriber channels are closed and later
// publishes are dropped.
func (h *eventHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}
