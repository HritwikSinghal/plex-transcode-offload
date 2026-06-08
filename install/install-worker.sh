#!/usr/bin/env bash
# install-worker.sh — set up a node to act as a GPU transcode worker for
# the master Plex Media Server.
#
# Run as root on the worker:
#   sudo ./install-worker.sh <master-public-key>
#
# Or, if you've already copied the key over via ssh-copy-id, run with no
# args and it will skip the authorized_keys step.

set -euo pipefail

PLEX_USER="plex"
PLEX_DIR="/usr/lib/plexmediaserver"
PLEX_BIN="${PLEX_DIR}/Plex Transcoder"
SSH_DIR="/var/lib/plex/.ssh"
AUTH_KEYS="${SSH_DIR}/authorized_keys"

die() { echo "FATAL: $*" >&2; exit 1; }
say() { echo "[install-worker] $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"

# --- 1. ensure Plex is installed (we need the transcoder binaries) -------
if ! [[ -x "$PLEX_BIN" ]]; then
    die "Plex Media Server not installed; install it first (same version as master) — the service does NOT need to run, but the binaries do need to be on disk."
fi

# --- 2. ensure plex user is real and stop the PMS service ----------------
id "$PLEX_USER" >/dev/null 2>&1 || die "plex user missing — install the plexmediaserver package"

if systemctl is-active --quiet plexmediaserver; then
    say "stopping plexmediaserver on worker (transcoder-only node)"
    systemctl stop plexmediaserver
fi
if systemctl is-enabled --quiet plexmediaserver 2>/dev/null; then
    say "disabling plexmediaserver service on worker"
    systemctl disable plexmediaserver
fi

# --- 3. give plex user a real shell (default is /usr/sbin/nologin) -------
current_shell="$(getent passwd "$PLEX_USER" | cut -d: -f7)"
if [[ $current_shell == */nologin || $current_shell == */false ]]; then
    say "switching ${PLEX_USER} login shell to /bin/bash so ssh exec works"
    chsh -s /bin/bash "$PLEX_USER"
fi

# --- 4. GPU access -------------------------------------------------------
say "ensuring ${PLEX_USER} can use GPU devices"
for grp in video render; do
    if getent group "$grp" >/dev/null; then
        usermod -aG "$grp" "$PLEX_USER" || true
    fi
done

# Sanity check
if [[ -e /dev/dri ]]; then
    say "/dev/dri present:"
    ls -l /dev/dri | sed 's/^/  /'
else
    say "WARN: /dev/dri missing — Intel/AMD VAAPI offload will not work"
fi
if command -v nvidia-smi >/dev/null 2>&1; then
    say "NVIDIA GPU detected:"
    nvidia-smi -L | sed 's/^/  /' || true
fi

# --- 5. SSH authorized_keys ----------------------------------------------
install -d -m 0700 -o "$PLEX_USER" -g "$PLEX_USER" "$SSH_DIR"
touch "$AUTH_KEYS"
chown "$PLEX_USER:$PLEX_USER" "$AUTH_KEYS"
chmod 0600 "$AUTH_KEYS"

if [[ $# -ge 1 ]]; then
    MASTER_PUBKEY="$*"
    if ! grep -qF -- "$MASTER_PUBKEY" "$AUTH_KEYS"; then
        say "appending master pubkey to ${AUTH_KEYS}"
        echo "$MASTER_PUBKEY" >> "$AUTH_KEYS"
    else
        say "master pubkey already present"
    fi
else
    say "no pubkey arg given; assuming key was added via ssh-copy-id"
fi

say
say "Worker install complete."
say "Verify from the master with:"
say "  sudo -u ${PLEX_USER} ssh -i /var/lib/plex/.ssh/id_ed25519 ${PLEX_USER}@$(hostname) 'echo OK'"
say
say "Next: mount the same NFS exports the master uses, at the same paths,"
say "so media files and the transcode tmp dir resolve identically here."
