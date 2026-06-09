package workerd

import "github.com/fsnotify/fsnotify"

// The segment pusher (implemented on a parallel branch) watches the job out
// dir with fsnotify. This reference pins the dependency in go.mod/go.sum NOW
// so the module graph -- and the nix vendorHash -- stay stable across the
// parallel role branches. Delete it once the watcher lands.
var _ = fsnotify.Op(0)
