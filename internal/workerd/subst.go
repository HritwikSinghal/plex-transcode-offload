package workerd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/HritwikSinghal/plex-transcode-offload/internal/protocol"
)

// substituteArgv resolves the protocol placeholders of a dispatched argv
// (the program token, argv[0], is NOT included -- the worker always runs its
// own cfg.transcoder_path):
//
//   - {{OUTDIR}}  -> the job's local out dir;
//   - {{PMS}}     -> the gating-proxy relay base for this job
//     (http://127.0.0.1:32401/relay/<id>), so every PMS callback URL
//     (progress, seglist, manifest) is interposed;
//   - {{MEDIA:n}} -> media[placeholder]: the signed masterd stream URL
//     (ffmpeg fetches it directly; no extra protocol args are injected) or
//     the spooled local path. Replacement is substring-wise because spool
//     references are embedded inside filter strings (subtitles=...).
//
// A media placeholder left unresolved is an error: spawning would hand the
// transcoder a literal "{{MEDIA:n}}" path.
func substituteArgv(argv []string, outDir, proxyBase string, media map[string]string) ([]string, error) {
	out := make([]string, len(argv))
	for i, tok := range argv {
		orig := tok
		tok = strings.ReplaceAll(tok, protocol.PlaceholderOutDir, outDir)
		tok = strings.ReplaceAll(tok, protocol.PlaceholderPMS, proxyBase)
		for ph, val := range media {
			tok = strings.ReplaceAll(tok, ph, val)
		}
		if strings.Contains(tok, "{{MEDIA:") {
			return nil, fmt.Errorf("workerd: argv[%d] %q references a media input absent from the media map", i, orig)
		}
		out[i] = tok
	}
	return out, nil
}

// buildEnv assembles the transcoder environment: the dispatched env first,
// then the worker-side overrides that retarget every path at local
// resources (design section 3.7). driverDriDir is empty when no iHD bundle
// exists for the build -- the job then runs software and inherited LIBVA_*
// values (which describe the MASTER's GPU) are dropped entirely.
// The result is sorted KEY=value, ready for exec.Cmd.Env.
func buildEnv(reqEnv map[string]string, homeDir, codecsDir, eaeRoot, plexLibDir, driverDriDir string) []string {
	env := make(map[string]string, len(reqEnv)+6)
	for k, v := range reqEnv {
		env[k] = v
	}
	delete(env, "LIBVA_DRIVERS_PATH")
	delete(env, "LIBVA_DRIVER_NAME")
	// Trailing slash: PMS concatenates codec file names onto this value.
	env["FFMPEG_EXTERNAL_LIBS"] = strings.TrimSuffix(codecsDir, "/") + "/"
	env["EAE_ROOT"] = eaeRoot
	env["HOME"] = homeDir
	env["LD_LIBRARY_PATH"] = plexLibDir
	if driverDriDir != "" {
		env["LIBVA_DRIVERS_PATH"] = driverDriDir
		env["LIBVA_DRIVER_NAME"] = "iHD"
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
