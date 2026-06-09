// Package shim is the master-side "Plex Transcoder" replacement: it
// classifies the job, dispatches it to a worker via prt-masterd, mirrors
// the remote transcoder, and execs the local fallback on any pre-segment
// failure.
package shim

import (
	"fmt"
	"os"
)

// Run executes the shim role with the transcoder argv (everything after
// argv[0]) and returns the process exit code.
func Run(args []string) int {
	fmt.Fprintln(os.Stderr, "prt shim: not implemented")
	return 1
}
