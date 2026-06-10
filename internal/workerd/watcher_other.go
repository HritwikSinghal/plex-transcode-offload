//go:build !linux

package workerd

import "errors"

// dirWatcher requires Linux inotify (IN_CLOSE_WRITE / IN_MOVED_TO have no
// portable equivalent). This stub keeps the package compiling on other
// platforms; workerd itself only ever runs on the Linux worker.
type dirWatcher struct {
	Events chan string
	Errors chan error
}

func newDirWatcher(dir string) (*dirWatcher, error) {
	return nil, errors.New("workerd: directory watching requires linux inotify")
}

func (w *dirWatcher) Close() error { return nil }
