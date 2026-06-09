{
  lib,
  stdenvNoCC,
  makeWrapper,
  python3,
  openssh,
  coreutils,
  gawk,
}:

# Plex Remote Transcoder (prt) -- master-side shim + diagnostics.
#
# This repo IS upstream / the source of truth for prt; the sources live here
# under ./bin, ./etc, ./systemd (no vendored copy). The tool is pure stdlib
# python3 + bash with no build step.
#
# `prt-transcoder` replaces the master's "Plex Transcoder" binary (see
# deploy-repo modules/plex-transcode-offload, which renames the real one
# to .orig and drops this in via plexRaw.overrideAttrs). It forwards each
# transcode over SSH to the worker, so it MUST be able to find `ssh` -- and
# because Plex runs the transcoder inside its buildFHSEnv sandbox, the python3
# interpreter and ssh must be referenced by absolute store path. The
# substituteInPlace below pins the interpreter; wrapProgram injects openssh onto
# PATH. Both survive being symlinked into the FHS root because makeWrapper (and
# the pinned shebang) record absolute store paths.
stdenvNoCC.mkDerivation {
  pname = "plex-transcode-offload";
  # Matches the upstream `0-unstable-<date>` scheme; bump the date on meaningful
  # changes.
  version = "0-unstable-2026-06-09";

  # Scope the source to exactly the dirs we install so unrelated files
  # (claude/, tests/, README.md, .pytest_cache) do not trigger rebuilds.
  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./bin
      ./etc
      ./systemd
    ];
  };

  nativeBuildInputs = [
    makeWrapper
    python3 # interpreter the shebang is pinned to below
  ];

  dontConfigure = true;
  dontBuild = true;

  installPhase = ''
    runHook preInstall

    install -Dm755 bin/prt-transcoder $out/bin/prt-transcoder
    install -Dm755 bin/prt-status     $out/bin/prt-status

    # Pin the interpreter explicitly. patchShebangs is unreliable here (it left
    # the `env python3` line untouched in testing), and the shim runs inside
    # Plex's buildFHSEnv sandbox where `/usr/bin/env python3` may not resolve.
    substituteInPlace $out/bin/prt-transcoder \
      --replace-fail '#!/usr/bin/env python3' '#!${python3.interpreter}'

    # Reference material consumed by modules/plex-transcode-offload and the
    # Ansible worker role; not on PATH.
    install -Dm644 etc/prt.conf.example            $out/share/prt/prt.conf.example
    install -Dm644 systemd/prt-ssh-keepalive.service $out/share/prt/prt-ssh-keepalive.service

    runHook postInstall
  '';

  postFixup = ''
    # prt-transcoder dispatches over `ssh`; prt-status additionally shells out
    # to awk/sed (gawk/coreutils). systemctl/sudo/nvidia-smi are expected from
    # the master's system PATH at run time.
    wrapProgram $out/bin/prt-transcoder \
      --prefix PATH : ${lib.makeBinPath [ openssh ]}
    wrapProgram $out/bin/prt-status \
      --prefix PATH : ${
        lib.makeBinPath [
          openssh
          coreutils
          gawk
        ]
      }
  '';

  meta = {
    description = "Plex remote-transcode SSH offload shim (master side)";
    longDescription = ''
      Drops in for the master's "Plex Transcoder" binary and forwards every
      transcode job over a persistent SSH ControlMaster connection to a GPU
      worker, which runs the real transcoder against its iGPU and writes HLS
      segments to an NFS-shared temp dir. Falls back to the local (backed-up)
      transcoder when the worker is unreachable.
    '';
    homepage = "https://github.com/HritwikSinghal";
    license = lib.licenses.mit;
    maintainers = with lib.maintainers; [ oakenshield ];
    platforms = [ "x86_64-linux" ];
    mainProgram = "prt-transcoder";
  };
}
