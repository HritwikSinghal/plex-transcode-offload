#!/usr/bin/env bash
# install-master.sh — set up the Plex Media Server node to offload
# transcoding to a remote worker.
#
# Idempotent. Safe to re-run after Plex package updates (the wrapper
# binary is restored if the package overwrites it).
#
# Run as root on the master:
#   sudo ./install-master.sh <worker-host> [worker-user]

set -euo pipefail

PLEX_DIR="/usr/lib/plexmediaserver"
REAL_BIN="${PLEX_DIR}/Plex Transcoder"
BACKUP_BIN="${PLEX_DIR}/Plex Transcoder.orig"
WRAPPER_SRC="$(cd "$(dirname "$0")/.." && pwd)/bin/prt-transcoder"
CONF_DIR="/etc/prt"
CONF_FILE="${CONF_DIR}/prt.conf"
EXAMPLE_CONF="$(cd "$(dirname "$0")/.." && pwd)/etc/prt.conf.example"
SYSTEMD_UNIT_SRC="$(cd "$(dirname "$0")/.." && pwd)/systemd/prt-ssh-keepalive.service"
SYSTEMD_UNIT_DST="/etc/systemd/system/prt-ssh-keepalive.service"
PLEX_USER="plex"
SSH_DIR="/var/lib/plex/.ssh"
SSH_KEY="${SSH_DIR}/id_ed25519"

die() { echo "FATAL: $*" >&2; exit 1; }
say() { echo "[install-master] $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
[[ -d $PLEX_DIR ]] || die "Plex Media Server is not installed (${PLEX_DIR} not found)"
[[ -f $WRAPPER_SRC ]] || die "wrapper source missing: ${WRAPPER_SRC}"

WORKER_HOST="${1:-}"
WORKER_USER="${2:-plex}"
[[ -n $WORKER_HOST ]] || die "usage: $0 <worker-host> [worker-user]"

# --- 1. backup real binary if not already backed up -----------------------
if [[ -f $REAL_BIN ]] && ! [[ -L $REAL_BIN ]]; then
    if ! [[ -f $BACKUP_BIN ]]; then
        say "backing up real Plex Transcoder -> ${BACKUP_BIN}"
        cp -a "$REAL_BIN" "$BACKUP_BIN"
    else
        say "backup already exists; leaving it"
    fi
fi

# --- 2. install wrapper as Plex Transcoder --------------------------------
say "installing wrapper to ${REAL_BIN}"
install -m 0755 "$WRAPPER_SRC" "$REAL_BIN"

# --- 3. config dir + skeleton ---------------------------------------------
install -d -m 0750 -o root -g "$PLEX_USER" "$CONF_DIR"
if ! [[ -f $CONF_FILE ]]; then
    say "writing ${CONF_FILE}"
    install -m 0640 -o root -g "$PLEX_USER" "$EXAMPLE_CONF" "$CONF_FILE"
    sed -i \
        -e "s|^host = .*|host = ${WORKER_HOST}|" \
        -e "s|^user = .*|user = ${WORKER_USER}|" \
        "$CONF_FILE"
else
    say "config already present; not overwriting"
fi

# --- 3b. env file consumed by the systemd keepalive unit ------------------
say "writing ${CONF_DIR}/prt.conf.env (for systemd EnvironmentFile)"
cat >"${CONF_DIR}/prt.conf.env" <<EOF
PRT_WORKER_HOST=${WORKER_HOST}
PRT_WORKER_USER=${WORKER_USER}
EOF
chmod 0640 "${CONF_DIR}/prt.conf.env"
chown root:"$PLEX_USER" "${CONF_DIR}/prt.conf.env"

# --- 4. SSH control-master socket dir (cleared on boot) -------------------
say "creating tmpfiles.d entry for /run/prt"
cat >/etc/tmpfiles.d/prt.conf <<EOF
d /run/prt 0700 ${PLEX_USER} ${PLEX_USER} -
EOF
systemd-tmpfiles --create /etc/tmpfiles.d/prt.conf

# --- 5. SSH keypair for the plex user -------------------------------------
install -d -m 0700 -o "$PLEX_USER" -g "$PLEX_USER" "$SSH_DIR"
if ! [[ -f $SSH_KEY ]]; then
    say "generating SSH keypair at ${SSH_KEY}"
    sudo -u "$PLEX_USER" ssh-keygen -t ed25519 -N "" -C "plex-remote-transcoder@$(hostname)" -f "$SSH_KEY"
else
    say "SSH key already exists; reusing"
fi
say
say "================================================================"
say " Copy this public key to ${WORKER_USER}@${WORKER_HOST}:"
say
cat "${SSH_KEY}.pub"
say
say " Or run on this host:"
say "   sudo -u ${PLEX_USER} ssh-copy-id -i ${SSH_KEY}.pub ${WORKER_USER}@${WORKER_HOST}"
say "================================================================"

# --- 6. systemd unit for persistent SSH ControlMaster ---------------------
if [[ -f $SYSTEMD_UNIT_SRC ]]; then
    say "installing prt-ssh-keepalive.service"
    install -m 0644 "$SYSTEMD_UNIT_SRC" "$SYSTEMD_UNIT_DST"
    systemctl daemon-reload
    systemctl enable prt-ssh-keepalive.service
    say "start the keepalive AFTER you've added the SSH key on the worker:"
    say "   sudo systemctl start prt-ssh-keepalive.service"
fi

say
say "Master install complete."
say "Next steps:"
say "  1. Run install-worker.sh on ${WORKER_HOST}"
say "  2. Authorize the public key above on the worker"
say "  3. Set up NFS so media + transcode tmp dirs match on both nodes"
say "  4. Restart Plex Media Server:  sudo systemctl restart plexmediaserver"
say "  5. Verify:  bin/prt-status"
