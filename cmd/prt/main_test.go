package main

import (
	"reflect"
	"testing"
)

func TestResolveRole(t *testing.T) {
	cases := []struct {
		name     string
		argv0    string
		args     []string
		role     string
		roleArgs []string
		ok       bool
	}{
		{"plex transcoder name", "Plex Transcoder", []string{"-i", "in.mkv"}, roleShim, []string{"-i", "in.mkv"}, true},
		{"prt-shim symlink", "prt-shim", []string{"-i", "x"}, roleShim, []string{"-i", "x"}, true},
		{"prt-masterd symlink", "prt-masterd", []string{"--config", "/etc/m.json"}, roleMasterd, []string{"--config", "/etc/m.json"}, true},
		{"prt-workerd symlink", "prt-workerd", []string{"--config", "/etc/w.json"}, roleWorkerd, []string{"--config", "/etc/w.json"}, true},
		{"subcommand shim", "prt", []string{"shim", "-i", "x"}, roleShim, []string{"-i", "x"}, true},
		{"subcommand masterd", "prt", []string{"masterd", "--config", "c"}, roleMasterd, []string{"--config", "c"}, true},
		{"subcommand workerd", "prt", []string{"workerd"}, roleWorkerd, []string{}, true},
		{"subcommand version", "prt", []string{"version"}, roleVersion, []string{}, true},
		{"no args", "prt", nil, "", nil, false},
		{"unknown subcommand", "prt", []string{"bogus"}, "", nil, false},
		{"unknown basename no args", "something-else", nil, "", nil, false},
		{"unknown basename falls back to subcommand", "something-else", []string{"workerd", "--x"}, roleWorkerd, []string{"--x"}, true},
		{"symlink name wins over subcommand", "prt-masterd", []string{"workerd"}, roleMasterd, []string{"workerd"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, roleArgs, ok := resolveRole(tc.argv0, tc.args)
			if role != tc.role || ok != tc.ok {
				t.Errorf("resolveRole(%q, %v) = (%q, _, %v), want (%q, _, %v)",
					tc.argv0, tc.args, role, ok, tc.role, tc.ok)
			}
			if tc.ok && !reflect.DeepEqual(roleArgs, tc.roleArgs) {
				t.Errorf("roleArgs = %#v, want %#v", roleArgs, tc.roleArgs)
			}
		})
	}
}
