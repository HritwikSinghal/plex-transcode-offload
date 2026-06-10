package workerd

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// eaePollInterval is how often the supervisor re-checks for the EAE
	// binary while it is absent (it appears when the codec cache for the
	// current build is provisioned).
	eaePollInterval = 10 * time.Second
	eaeBackoffStart = time.Second
	eaeBackoffCap   = 30 * time.Second
	// eaeStableRun resets the restart backoff.
	eaeStableRun = time.Minute
)

// eaeSupervisor runs ONE long-lived EasyAudioEncoder per worker (design
// section 9, Blocker C): cwd = cfg.eae_root (EAE watchfolders its CWD),
// shared by every job via the EAE_ROOT env override. Restarts on exit with
// backoff; exits with the daemon.
func (d *daemon) eaeSupervisor() {
	backoff := eaeBackoffStart
	for {
		if d.ctx.Err() != nil {
			return
		}
		bin := d.eaeBinary()
		if bin == "" {
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(eaePollInterval):
			}
			continue
		}
		start := time.Now()
		err := d.runEAE(bin)
		if d.ctx.Err() != nil {
			return
		}
		d.logf("eae: exited: %v", err)
		if time.Since(start) >= eaeStableRun {
			backoff = eaeBackoffStart
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, eaeBackoffCap)
	}
}

// eaeBinary locates EasyAudioEncoder in the codec cache of the CURRENT
// build (it arrives bit-identical from the master's Codecs tree).
func (d *daemon) eaeBinary() string {
	base := d.codecsDir(d.plexBuild)
	for _, cand := range []string{
		filepath.Join(base, "EasyAudioEncoder"),
		filepath.Join(base, "EasyAudioEncoder", "EasyAudioEncoder"),
	} {
		if fileExists(cand) {
			return cand
		}
	}
	return ""
}

func (d *daemon) runEAE(bin string) error {
	if err := os.MkdirAll(d.cfg.EAERoot, 0o755); err != nil {
		return err
	}
	// A stale pid file from a crashed instance makes EAE refuse to start.
	for _, pidFile := range []string{"eae.pid", "EasyAudioEncoder.pid"} {
		_ = os.Remove(filepath.Join(d.cfg.EAERoot, pidFile))
	}
	cmd := exec.CommandContext(d.ctx, bin)
	cmd.Dir = d.cfg.EAERoot
	cmd.Env = []string{
		"HOME=" + d.cfg.EAERoot,
		"EAE_ROOT=" + d.cfg.EAERoot,
		"LD_LIBRARY_PATH=" + filepath.Join(d.cfg.PlexDir, "lib"),
		"TMPDIR=/tmp",
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = cancelGrace
	d.logf("eae: starting %s (root %s)", bin, d.cfg.EAERoot)
	return cmd.Run()
}
