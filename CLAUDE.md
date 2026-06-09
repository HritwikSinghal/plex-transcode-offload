# CLAUDE.md -- plex-transcode-offload (prt)

## What this repo is

The `prt` tool: one static Go binary that offloads Plex Media Server transcoding to a
remote worker over plain HTTP. Three roles in one binary, selected by argv[0] basename
("Plex Transcoder"/prt-shim, prt-masterd, prt-workerd) or subcommand (`prt shim|masterd|workerd|version`):

- shim -- replaces PMS's "Plex Transcoder" on the master; dispatches each job to a worker
  and execs the local `Plex Transcoder.orig` fallback on any pre-segment failure.
- masterd (:32499) -- master daemon: segment receiver (write tmp + rename into the PMS
  session dir), media/codecs file server (HMAC-signed URLs), PMS callback relay, worker
  health cache.
- workerd (:32500) -- worker daemon: runs the real transcoder on the local iGPU, pushes
  segments back over HTTP, gates seglist announces until every referenced chunk is
  ACKed (THE correctness invariant), supervises EasyAudioEncoder.

v1 (Python shim, SSH dispatch, NFS shared paths) is preserved at git tag `v1`; v2 is a
clean-slate rewrite, not a port.

## Layout

- `cmd/prt/main.go` -- role dispatch (`resolveRole` is the pure, tested core).
- `internal/protocol` -- wire types + constants. THE shared contract; never redefine
  these in role packages, and treat field/JSON-tag changes as breaking.
- `internal/config` -- ShimConfig/MasterdConfig/WorkerdConfig JSON schemas (snake_case)
  and validating loaders (defaults applied, required fields enforced, unknown keys
  rejected). Shim config comes from `$PRT_CONF`; its token from `$PRT_TOKEN_FILE`.
- `internal/authtok` -- bearer middleware (constant-time), MintToken, LoadToken,
  SignURL/VerifySignedQuery (HMAC-SHA256 over canonical path+query | expiry).
- `internal/ndjson` -- event stream Writer/Reader: per-event flush, 5s heartbeats,
  Reader.WatchLiveness for the 10s peer-lost rule.
- `internal/{shim,masterd,workerd}` -- role implementations (`Run(args []string) int`).
- `modules/master.nix`, `modules/worker.nix` -- generic NixOS modules
  (`services.prt-masterd` / `services.prt-workerd`); site specifics live in the
  consuming homelab repo, not here.

## Key invariants (do not break)

- Chunk-before-announce: a seglist/manifest POST may only reach PMS after every chunk
  it references is PUT-ACKed by masterd. Progress POSTs pass through immediately.
- Job liveness is connection-based, never signal-based: PMS SIGKILLs the shim; the
  NDJSON events stream is the lifeline (5s heartbeats, 10s timeout). Worker lost
  mid-job => shim exits 75 (protocol.ExitWorkerLost) and PMS restarts the session.
- Every pre-first-segment failure execs the local `Plex Transcoder.orig` with the
  original argv -- playback never breaks.
- Single third-party dep: fsnotify. Everything else stdlib (+httptest in tests).

## Commands

- `go build ./... && go vet ./... && go test ./...` -- must stay green.
- `nix build .#prt` -- static binary + role symlinks; tests run in checkPhase.
- `nix flake check` -- adds the treefmt formatting check; `nix fmt` to fix.
- vendorHash debugging: set `lib.fakeHash`-style placeholder, build, copy the real
  hash from the error.

## Integration boundary

This repo is the tool source only. Deployment (Colmena targets, PMS wrappedPlexRaw
swap, sops token, firewall) lives in the separate private homelab repo, which imports
this flake's `packages` and `nixosModules`.
