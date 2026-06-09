# plex-transcode-offload (prt)

Offload Plex Media Server transcoding to a remote GPU worker over SSH.

`prt` replaces Plex Media Server's `Plex Transcoder` binary with a thin shim. Every
transcode job Plex launches is forwarded over a persistent SSH connection to a remote
worker, which runs the *real* transcoder against its GPU and writes HLS segments back
to a temp directory shared with the master over NFS. The master then serves those
segments to clients as if it had produced them locally.

If the worker is unreachable, the master can transparently fall back to its own
transcoder, so playback keeps working (just without offload).

- Pure Python 3 standard library + Bash. **No build step, no dependencies to install.**
- Zero persistent state in the shim. Connection persistence is handled entirely by SSH
  `ControlMaster`.

## Why

A common homelab setup has a beefy storage/Plex master node with a weak (or no) GPU,
and a separate machine with a capable iGPU/dGPU sitting mostly idle. `prt` lets the
master keep owning the Plex library, metadata, and client connections while pushing the
CPU/GPU-heavy transcode work to the GPU box -- without running a second Plex server or
touching Plex's internals beyond swapping one binary.

## How it works

```
                  master (Plex Media Server)                 worker (GPU box)
  client <----> Plex <-> prt-transcoder shim ==ssh==> real Plex Transcoder -> iGPU/dGPU
                              |                                    |
                              |  HLS segments written to a temp dir at an
                              |  IDENTICAL absolute path on both nodes (NFS)
                              +------------------ NFS -------------------+
                              v
                  master reads the segments and serves them to the client
```

1. Plex on the master invokes what it thinks is `Plex Transcoder`. It is actually the
   `prt-transcoder` shim.
1. The shim builds a remote command -- `cd <cwd> && exec env -i <allowlisted env> -- <real transcoder> <argv...>` -- and runs it on the worker over SSH, reusing a
   `ControlMaster` socket kept warm by `prt-ssh-keepalive.service`.
1. The worker runs the real transcoder on its GPU. Because media and the transcode temp
   dir live at the **same absolute paths** on both nodes (via NFS), no path translation
   is needed -- the shim forwards argv and cwd verbatim.
1. HLS segments land in the shared temp dir; the master reads them straight back.
1. stdio and signals are forwarded faithfully end-to-end (see Design contracts).

## Repository layout

| Path | What it is |
|------|------------|
| `bin/prt-transcoder` | Master-side Python shim that intercepts Plex transcoder calls and forwards them over SSH. |
| `bin/prt-status` | Bash diagnostic that verifies the full master \<-> worker pipeline. |
| `etc/prt.conf.example` | Annotated config template (`[worker]` + `[paths]`). |
| `systemd/prt-ssh-keepalive.service` | Keeps a persistent SSH `ControlMaster` connection to the worker up. |
| `install/install-master.sh` | Idempotent installer for the Plex master node. |
| `install/install-worker.sh` | Idempotent installer for the GPU worker node. |

## Requirements

**Master (Plex node)**

- Plex Media Server installed.
- Python 3 and `openssh-client`.
- systemd.

**Worker (GPU node)**

- The **same Plex Media Server version** installed (only the transcoder binaries are
  needed -- the PMS service itself is stopped and disabled by the installer).
- A usable GPU: Intel/AMD VAAPI (`/dev/dri/renderD128`) and/or NVIDIA NVENC
  (`nvidia-smi`).
- The `plex` user with read access to the GPU devices.

**Both nodes**

- Shared NFS so that media files **and** Plex's transcode temp directory resolve at
  **identical absolute paths** on each node. This is the central design contract.

## Installation

### 1. Master

```bash
sudo ./install/install-master.sh <worker-host> [worker-user]
```

This backs up the real `Plex Transcoder` to `Plex Transcoder.orig`, installs the shim
in its place, writes `/etc/prt/prt.conf` (seeded with the worker host/user), creates the
`/run/prt` socket dir via `tmpfiles.d`, generates an SSH keypair for the `plex` user,
and installs (but does not yet start) `prt-ssh-keepalive.service`. It prints the public
key to authorize on the worker.

The installer is idempotent and re-run-safe after Plex package updates (it restores the
shim if the package overwrites it).

### 2. Worker

Copy the public key printed by the master installer, then on the worker:

```bash
sudo ./install/install-worker.sh "<master-public-key>"
# or, if you've already run ssh-copy-id, with no args:
sudo ./install/install-worker.sh
```

This stops/disables the PMS service (worker is transcode-only), gives the `plex` user a
real login shell so SSH `exec` works, adds it to the `video`/`render` groups for GPU
access, and authorizes the master's key.

### 3. Shared filesystem

Mount the same NFS exports the master uses, at the same paths, so media and the
transcode temp dir resolve identically on the worker.

### 4. Start and verify

```bash
# On the master, once the worker has authorized the key:
sudo systemctl start prt-ssh-keepalive.service
sudo systemctl restart plexmediaserver

# Verify the whole pipeline:
./bin/prt-status
```

## Configuration

Config lives at `/etc/prt/prt.conf` (root:plex, mode 0640). It is also read from
`~/.config/prt/prt.conf` if present. See `etc/prt.conf.example` for the annotated
template.

```ini
[worker]
host = worker.lan                          ; worker hostname/IP (must resolve on master)
user = plex                                ; UNIX user on the worker
ssh_key = /var/lib/plex/.ssh/id_ed25519    ; master's SSH private key
fallback_local = true                      ; fall back to local transcoder if worker is down

[paths]
plex_transcoder = /usr/lib/plexmediaserver/Plex Transcoder       ; real binary (worker + master)
plex_transcoder_backup = /usr/lib/plexmediaserver/Plex Transcoder.orig  ; for local fallback
ssh_control_dir = /run/prt                 ; ControlMaster socket dir (owned by plex)
```

`fallback_local` defaults to **off**. With it off, a missing worker makes transcoding
fail loudly, which is preferable while debugging. Turn it on for resilience once the
pipeline is proven.

## Fallback behaviour

The shim falls back to the local `Plex Transcoder.orig` (when `fallback_local = true`
and the backup binary exists) in two cases:

- `ssh` cannot be launched at all (`OSError`).
- `ssh` exits with code `255` (transport error -- worker unreachable mid-handshake).

Otherwise the remote transcoder's exit code is returned to Plex unchanged.

## Design contracts (do not break)

- **Identical filesystem layout.** Media and the transcode temp dir must live at the
  same absolute path on both nodes (NFS). The shim forwards paths verbatim; there is no
  translation layer.
- **Bounded env forwarding.** Only Plex-relevant environment variables are forwarded
  (prefixes `X_PLEX_`/`PLEX_`/`FFMPEG_`/`XDG_`/`LC_`, plus an explicit allowlist). This
  keeps the remote command line under `ARG_MAX` and avoids leaking unrelated state.
  `env -i` wipes the worker's login-shell env so only allowlisted vars survive.
- **Faithful signal forwarding.** `SIGTERM`/`SIGINT`/`SIGHUP`/`SIGQUIT` (plus
  `SIGUSR1`/`SIGUSR2`) propagate to `ssh`, which relays them to the remote process group.
  `ssh -tt` forces a remote TTY so the transcoder receives `SIGHUP` if the master
  disconnects -- no orphaned transcoders on a master crash.

## Diagnostics

Run `./bin/prt-status` on the master. It checks, with pass/warn/fail output:

- Shim is installed and the real binary is backed up.
- `/run/prt` exists and is writable by `plex`; the keepalive service is active.
- TCP/22 reachability, SSH login as `plex`, and a live `ControlMaster` socket.
- The transcoder binary, `/dev/dri` (VAAPI), optional NVIDIA GPU, and render-node
  read access on the worker.

Runtime activity is logged to syslog under the `prt-transcoder` tag (`LOG_DAEMON`).

## Deployment / packaging

This repository holds the **tool source only**. The Nix package/module, Terraform,
Ansible roles, and secrets wiring that deploy `prt` into a homelab live in a separate
private repository. Tool-source changes happen here; deployment/packaging changes happen
there.

## License

GPL-3.0. See [LICENSE](LICENSE).
