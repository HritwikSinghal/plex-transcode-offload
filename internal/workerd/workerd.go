// Package workerd is the worker-side daemon (:32500): job runner, segment
// pusher, gating proxy, provisioner, and EAE supervisor.
package workerd

import (
	"fmt"
	"os"
)

// Run executes the workerd role with its CLI args (e.g. --config <path>)
// and returns the process exit code.
func Run(args []string) int {
	fmt.Fprintln(os.Stderr, "prt workerd: not implemented")
	return 1
}
