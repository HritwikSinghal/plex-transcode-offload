//go:build linux

package workerd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// errWatcherOverflow is reported when the kernel inotify queue overflowed:
// completion events were lost, so the job must fail rather than risk
// announcing a chunk that was never pushed.
var errWatcherOverflow = errors.New("inotify event queue overflow")

// dirWatcher delivers the basenames of files COMPLETED in a single directory:
// IN_CLOSE_WRITE (the writer finished) and IN_MOVED_TO (renamed into place).
// Raw inotify is used instead of fsnotify because fsnotify folds IN_MOVED_TO
// and IN_CREATE into one Create op and exposes no stable close-write event --
// the segment pusher must only ever ship complete files.
type dirWatcher struct {
	f *os.File // nonblocking inotify fd, integrated with the runtime poller

	// Events carries completed-file basenames; closed when the read loop
	// exits (after Close or a fatal error).
	Events chan string
	// Errors carries at most one fatal watcher error (e.g. queue overflow).
	Errors chan error
}

// newDirWatcher starts watching dir. Call Close to release the inotify fd
// and end the read loop.
func newDirWatcher(dir string) (*dirWatcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("workerd: inotify_init1: %w", err)
	}
	// A nonblocking fd handed to os.NewFile registers with the runtime
	// poller: Read blocks cooperatively and Close unblocks a pending Read.
	f := os.NewFile(uintptr(fd), "inotify:"+dir)
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workerd: inotify_add_watch %s: %w", dir, err)
	}
	w := &dirWatcher{
		f:      f,
		Events: make(chan string, 256),
		Errors: make(chan error, 1),
	}
	go w.readLoop()
	return w, nil
}

// Close releases the inotify fd; the read loop then closes Events.
// Safe to call more than once.
func (w *dirWatcher) Close() error { return w.f.Close() }

func (w *dirWatcher) readLoop() {
	defer close(w.Events)
	buf := make([]byte, 64*1024)
	for {
		n, err := w.f.Read(buf)
		if err != nil {
			if !errors.Is(err, os.ErrClosed) {
				w.reportError(fmt.Errorf("workerd: inotify read: %w", err))
			}
			return
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameBytes := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+int(raw.Len)]
			off += unix.SizeofInotifyEvent + int(raw.Len)
			if raw.Mask&unix.IN_Q_OVERFLOW != 0 {
				w.reportError(errWatcherOverflow)
				return
			}
			if raw.Mask&unix.IN_ISDIR != 0 {
				continue
			}
			name := strings.TrimRight(string(nameBytes), "\x00")
			if name == "" {
				continue
			}
			w.Events <- name
		}
	}
}

func (w *dirWatcher) reportError(err error) {
	select {
	case w.Errors <- err:
	default:
	}
}
