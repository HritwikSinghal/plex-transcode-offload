# CLAUDE.md -- plex-transcode-offload (prt)

## What this repo is
The `plex-transcode-offload` (prt) tool. It replaces Plex Media Server's "Plex Transcoder"
binary with a shim that forwards each transcode job over SSH to a remote GPU worker; the
worker runs the real transcoder against its iGPU and writes HLS segments to a temp dir shared
back to the master over NFS. The master's local GPU is the fallback. Pure-stdlib Python3 +
Bash -- there is NO build step.

## Layout
- bin/prt-transcoder -- the master-side Python shim (intercepts Plex transcoder calls, forwards
  over SSH with faithful stdio + signal handling; zero-state, persistence via SSH ControlMaster).
- bin/prt-status -- Bash diagnostic that verifies the master<->worker pipeline.
- etc/prt.conf.example -- INI config template ([worker] host/user/ssh_key/fallback_local,
  [paths] plex_transcoder/plex_transcoder_backup/ssh_control_dir).
- systemd/prt-ssh-keepalive.service -- keeps a persistent SSH ControlMaster connection up.
- install/install-master.sh, install/install-worker.sh -- idempotent node installers.

## How it works (1-liner)
master Plex -> prt-transcoder shim -> ssh (ControlMaster) -> worker runs real transcoder on
its iGPU -> HLS segments land in an NFS-shared temp dir at an identical path -> master serves
them. fallback_local=true runs the master's own transcoder if the worker is unreachable.

## Design contracts (do not break)
- Identical filesystem layout: media + transcode-temp at the SAME absolute path on both nodes
  (NFS). The shim forwards paths verbatim.
- Bounded env forwarding: only Plex-relevant env vars are forwarded (avoids ARG_MAX overflow).
- Faithful signal forwarding: SIGTERM/SIGINT/SIGHUP/SIGQUIT propagate to the remote job.

## Integration boundary
The nix package/module, Terraform, Ansible roles, and sops wiring that DEPLOY this tool into
the homelab live in a SEPARATE repo: ~/Projects/deploy-repo
(nix/pkgs/plex-transcode-offload, nix/modules/plex-transcode-offload, tf/, ansible/roles/{
plex_transcode_worker,nfs_server}). The imported claude/ docs reference those oakenshields
paths -- that cross-repo boundary is EXPECTED, not stale. Tool-source changes (e.g. Phase B
hardening: R1/R2/S2/R7) happen HERE; deployment/packaging changes happen in deploy-repo.

## Long-running project
- Read claude/progress.md and claude/tasks.md at session start (silently) to catch up.
- Update them after each task: mark [x] on completion, append new action items, log progress
  and blockers in progress.md.
- claude/plex-offload-handoff.md is the authoritative detailed status; claude/
  plex-offload-phase-a-plan.md is the approved Phase A plan; the two transcode-temp-storage
  docs hold the settled S1 storage research.
