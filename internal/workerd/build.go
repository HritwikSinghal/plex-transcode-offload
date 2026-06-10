package workerd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// renderDevice is the worker iGPU render node probed for vaapi_ok.
const renderDevice = "/dev/dri/renderD128"

// plexBuildFromDir derives the worker's own Plex build string from the
// configured plex_dir: a VERSION file inside it wins; otherwise the version
// is parsed out of the nix store path name (a path component of the form
// "<hash>-plexmediaserver-<version>", e.g.
// /nix/store/abc...-plexmediaserver-1.43.0.10166-deadbeef/lib/plexmediaserver).
func plexBuildFromDir(plexDir string) (string, error) {
	if raw, err := os.ReadFile(filepath.Join(plexDir, "VERSION")); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v, nil
		}
	}
	const marker = "plexmediaserver-"
	for _, seg := range strings.Split(filepath.Clean(plexDir), string(filepath.Separator)) {
		i := strings.Index(seg, marker)
		if i < 0 {
			continue
		}
		v := seg[i+len(marker):]
		if v != "" && v[0] >= '0' && v[0] <= '9' {
			return v, nil
		}
	}
	return "", fmt.Errorf("workerd: cannot derive plex build from plex_dir %s (no VERSION file, no plexmediaserver-<version> path component)", plexDir)
}

// versionComponent returns the dotted version part of a Plex build string,
// i.e. everything before the first '-' ("1.43.0.10166-deadbeef" ->
// "1.43.0.10166").
func versionComponent(build string) string {
	if i := strings.IndexByte(build, '-'); i >= 0 {
		return build[:i]
	}
	return build
}

// buildsMatch compares two Plex build strings LENIENTLY: the version
// components must match; build-suffix differences (the trailing -<sha>, which
// can differ between byte-identical nix rebuilds) are ignored.
func buildsMatch(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return a == b || versionComponent(a) == versionComponent(b)
}

// driverDriDir locates the persisted iHD VAAPI driver directory (the dri/
// dir containing iHD_drv_video.so) appropriate for build, under
// cfg.drivers_dir. Matching, in order:
//
//  1. a subdir named exactly after the build;
//  2. a subdir whose name contains the build's version component;
//  3. if exactly ONE subdir holds an iHD driver, that one -- the bundle
//     persisted by the plex-driver-fetch oneshot is named after Plex's
//     internal driver version (rsv-<ver>-linux-x86_64), which is not
//     derivable from the build string. Multiple candidates are ambiguous
//     and refused (vaapi_ok=false; jobs run software).
func (d *daemon) driverDriDir(build string) (string, bool) {
	entries, err := os.ReadDir(d.cfg.DriversDir)
	if err != nil {
		return "", false
	}
	ver := versionComponent(build)
	var fallbacks []string
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		dri, ok := ihdDriDir(filepath.Join(d.cfg.DriversDir, e.Name()))
		if !ok {
			continue
		}
		if e.Name() == build || (ver != "" && strings.Contains(e.Name(), ver)) {
			return dri, true
		}
		fallbacks = append(fallbacks, dri)
	}
	if len(fallbacks) == 1 {
		return fallbacks[0], true
	}
	return "", false
}

// ihdDriDir checks dir (or dir/dri) for iHD_drv_video.so and returns the
// directory suitable for LIBVA_DRIVERS_PATH.
func ihdDriDir(dir string) (string, bool) {
	for _, cand := range []string{filepath.Join(dir, "dri"), dir} {
		if fileExists(filepath.Join(cand, "iHD_drv_video.so")) {
			return cand, true
		}
	}
	return "", false
}

// vaapiOK reports whether hardware jobs are possible: an iHD driver bundle
// for the current build exists and the render node is openable.
func (d *daemon) vaapiOK() bool {
	if _, ok := d.driverDriDir(d.plexBuild); !ok {
		return false
	}
	f, err := os.OpenFile(renderDevice, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// validIDRE constrains job ids and build strings used as path components:
// no separators, no leading dot (excludes "." and ".." and hidden dirs).
var validIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func validID(s string) bool { return validIDRE.MatchString(s) }

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
