# plex-transcode-offload (prt)

Offload Plex Media Server transcoding to a remote worker over plain HTTP.

`prt` is one static Go binary with three roles:

| Role | Invocation | Where | What it does |
|------|------------|-------|--------------|
| shim | symlinked in as `Plex Transcoder` | master, spawned per job by PMS | classifies the job, dispatches it to a worker, mirrors stderr/exit code, falls back to the local `Plex Transcoder.orig` on any pre-segment failure |
| masterd | `prt-masterd --config <json>` (`:32499`) | master | receives transcoded segments into the PMS session dir, serves media + codecs to workers (HMAC-signed URLs), relays the transcoder's PMS callbacks, caches worker health |
| workerd | `prt-workerd --config <json>` (`:32500`) | worker | runs the real transcoder against the local iGPU, pushes segments back, gates seglist announces until every referenced chunk is uploaded, supervises EasyAudioEncoder |

No shared storage: v1's SSH dispatch and the NFS mounts for media, transcode
temp, and codecs are all replaced by HTTP (Range-streamed media inputs,
PUT-pushed segments, per-build codec sync). Every remote failure degrades to
a local transcode -- playback never breaks.

## Flake outputs

- `packages.x86_64-linux.prt` -- the `prt` binary (plus `prt-shim`,
  `prt-masterd`, `prt-workerd` symlinks; role is picked by argv[0] or
  subcommand). `plex-transcode-offload` is a compat alias.
- `nixosModules.master` -- `services.prt-masterd` unit (generic; the consumer
  wires PMS, firewall, and secrets).
- `nixosModules.worker` -- `services.prt-workerd` unit plus the optional
  `plex-driver-fetch` oneshot that bootstraps Plex's Intel iHD VAAPI driver
  bundle by running a full PMS once.

## Layout

- `cmd/prt` -- entry point and role dispatch.
- `internal/protocol` -- wire types and constants (the contract between roles).
- `internal/config` -- JSON config schemas + loaders for all three roles.
- `internal/authtok` -- bearer-token auth, per-session push tokens, signed URLs.
- `internal/ndjson` -- heartbeating NDJSON event streams (the job lifeline).
- `internal/{shim,masterd,workerd}` -- the role implementations.
- `modules/` -- the two NixOS modules.

## Development

```bash
nix develop          # go, gopls, gotools, nixfmt
go test ./...
nix build .#prt
nix flake check      # build + tests + formatting
```

## History

v1 (Python shim, SSH dispatch, NFS shared paths) is preserved at the git tag
`v1`. v2 is a clean-slate rewrite, not a port.

## License

GPL-3.0. See [LICENSE](LICENSE).
