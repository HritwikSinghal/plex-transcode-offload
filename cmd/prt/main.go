// Command prt is the PRT v2 multi-role binary. The role is picked from
// basename(argv[0]) -- the nix package symlinks prt-shim / prt-masterd /
// prt-workerd to it, and the master module symlinks it in as
// "Plex Transcoder" -- or, for the plain "prt" name, from the first
// argument (prt shim|masterd|workerd|version).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/masterd"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/shim"
	"github.com/HritwikSinghal/plex-transcode-offload/internal/workerd"
)

// version is the prt release version (kept in lockstep with the nix package).
var version = "2.0.0"

// role names returned by resolveRole.
const (
	roleShim    = "shim"
	roleMasterd = "masterd"
	roleWorkerd = "workerd"
	roleVersion = "version"
)

// resolveRole maps the invocation basename (argv0Base) and the remaining
// arguments to a role and that role's args. ok is false when no role can be
// determined (caller prints usage and exits 2). Pure function for
// testability.
func resolveRole(argv0Base string, args []string) (role string, roleArgs []string, ok bool) {
	switch argv0Base {
	case "Plex Transcoder", "prt-shim":
		return roleShim, args, true
	case "prt-masterd":
		return roleMasterd, args, true
	case "prt-workerd":
		return roleWorkerd, args, true
	}
	if len(args) == 0 {
		return "", nil, false
	}
	switch args[0] {
	case roleShim, roleMasterd, roleWorkerd, roleVersion:
		return args[0], args[1:], true
	}
	return "", nil, false
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: prt <role> [args...]

roles:
  shim      [transcoder argv...]   Plex Transcoder replacement (master)
  masterd   --config <path.json>   master daemon (:32499)
  workerd   --config <path.json>   worker daemon (:32500)
  version                          print the prt version

The role is also selected by invocation name: "Plex Transcoder" / prt-shim,
prt-masterd, prt-workerd.
`)
}

func main() {
	role, roleArgs, ok := resolveRole(filepath.Base(os.Args[0]), os.Args[1:])
	if !ok {
		usage()
		os.Exit(2)
	}
	switch role {
	case roleShim:
		os.Exit(shim.Run(roleArgs))
	case roleMasterd:
		os.Exit(masterd.Run(roleArgs))
	case roleWorkerd:
		os.Exit(workerd.Run(roleArgs))
	case roleVersion:
		fmt.Printf("prt %s\n", version)
	}
}
