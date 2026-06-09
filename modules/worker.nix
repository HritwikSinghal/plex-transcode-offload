# Generic prt-workerd NixOS module: the worker-side daemon (:32500) that
# runs the real "Plex Transcoder" against the local iGPU, pushes segments
# back to prt-masterd, gates seglist announces behind chunk-upload ACKs, and
# supervises the EasyAudioEncoder.
#
# Also ships the optional `plex-driver-fetch` oneshot (design section 9,
# Blocker B): Plex 1.43+ fetches a musl VAAPI driver bundle matched to the
# DETECTED GPU, so the Intel iHD driver can only be obtained by running a
# full PMS on the worker once. Manual trigger only (WantedBy=[]); re-run on
# Plex bumps that change the driver requirement.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.prt-workerd;
  settingsFormat = pkgs.formats.json { };
  configFile = settingsFormat.generate "prt-workerd.json" (
    cfg.settings
    // {
      token_file = cfg.tokenFile;
    }
  );
  # Mirrors the WorkerdConfig defaults in internal/config; needed here for
  # the unit's filesystem grants.
  dataDir = cfg.settings.data_dir or "/var/lib/prt";
in
{
  options.services.prt-workerd = {
    enable = lib.mkEnableOption "prt worker daemon";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The prt package to run (no default: the consumer pins it).";
    };

    settings = lib.mkOption {
      type = settingsFormat.type;
      default = { };
      description = ''
        prt-workerd JSON config (WorkerdConfig schema). Required keys:
        transcoder_path, plex_dir. token_file is injected from `tokenFile`.
      '';
      example = lib.literalExpression ''
        {
          transcoder_path = "''${pkgs.plexRaw}/lib/plexmediaserver/Plex Transcoder";
          plex_dir = "''${pkgs.plexRaw}/lib/plexmediaserver";
          max_jobs = 3;
        }
      '';
    };

    tokenFile = lib.mkOption {
      type = lib.types.str;
      description = ''
        Path to the shared bearer token file (e.g. a sops-nix secret path).
        Injected into settings as token_file; the token value never enters
        the nix store.
      '';
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "plex";
      description = "User the daemon (and the transcoder) runs as.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "plex";
      description = "Group the daemon runs as.";
    };

    extraGroups = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "video"
        "render"
      ];
      description = "Supplementary groups; video+render grant /dev/dri access for VAAPI.";
    };

    driverFetch = {
      enable = lib.mkEnableOption "the plex-driver-fetch oneshot (iHD musl driver bootstrap)";

      plexPackage = lib.mkOption {
        type = lib.types.package;
        description = ''
          The plexRaw package whose PMS is started once to download the
          Drivers bundle. Must match the build prt serves (same nixpkgs pin
          as the master).
        '';
      };

      dataDir = lib.mkOption {
        type = lib.types.str;
        default = "/var/lib/prt";
        description = ''
          Where to persist the fetched Drivers dir (under <dataDir>/drivers,
          the workerd drivers_dir default) and to keep the throwaway PMS
          application support dir.
        '';
      };
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.prt-workerd = {
      description = "prt worker daemon (job runner, segment pusher, gating proxy)";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      serviceConfig = {
        ExecStart = "${cfg.package}/bin/prt-workerd --config ${configFile}";
        User = cfg.user;
        Group = cfg.group;
        SupplementaryGroups = cfg.extraGroups;
        Restart = "on-failure";
        RestartSec = 2;

        # /run/prt-eae is the EAE_ROOT parent; recreated each boot.
        RuntimeDirectory = "prt-eae";

        # Hardening: writable state is the job/codec data dir plus the EAE
        # runtime dir; the iGPU render node is the only extra device.
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        # Quote each entry: systemd splits unquoted unit-file path lists on
        # whitespace, which breaks paths containing spaces.
        ReadWritePaths = map (p: ''"${p}"'') [
          dataDir
          "/run/prt-eae"
        ];
        DeviceAllow = [ "/dev/dri/renderD128 rw" ];
      };
    };

    # Bootstrap the worker data dir so the daemon can write before any job.
    systemd.tmpfiles.rules = [
      "d ${dataDir} 0750 ${cfg.user} ${cfg.group} -"
    ];

    systemd.services.plex-driver-fetch = lib.mkIf cfg.driverFetch.enable {
      description = "One-shot: run a full PMS to fetch the iHD musl VAAPI driver bundle";
      # Manual trigger only: systemctl start plex-driver-fetch
      wantedBy = [ ];
      conflicts = [ "prt-workerd.service" ];

      path = [
        pkgs.coreutils
        pkgs.findutils
        pkgs.procps
      ];

      serviceConfig = {
        Type = "oneshot";
        User = cfg.user;
        Group = cfg.group;
        SupplementaryGroups = cfg.extraGroups;
        # PMS start + GPU detection + bundle download can be slow.
        TimeoutStartSec = "30min";
        ReadWritePaths = [ cfg.driverFetch.dataDir ];
      };

      script = ''
        set -euo pipefail
        support="${cfg.driverFetch.dataDir}/driver-fetch-pms"
        drivers_out="${cfg.driverFetch.dataDir}/drivers"
        pms_dir="$support/Plex Media Server"

        mkdir -p "$support"
        export PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR="$support"
        export LD_LIBRARY_PATH="${cfg.driverFetch.plexPackage}/lib"
        export HOME="$support"

        echo "starting throwaway PMS to trigger the Drivers download..."
        "${cfg.driverFetch.plexPackage}/lib/plexmediaserver/Plex Media Server" &
        pms=$!
        trap 'kill "$pms" 2>/dev/null || true' EXIT

        driver=""
        for _ in $(seq 1 300); do
          driver=$(find "$pms_dir/Drivers" -path '*ihd-*' -name 'iHD_drv_video.so' 2>/dev/null | head -n1 || true)
          [ -n "$driver" ] && break
          sleep 5
        done

        kill "$pms" 2>/dev/null || true
        wait "$pms" || true
        trap - EXIT

        if [ -z "$driver" ]; then
          echo "iHD driver did not appear under $pms_dir/Drivers -- is the iGPU visible?" >&2
          exit 1
        fi

        echo "persisting Drivers bundle (found $driver)"
        mkdir -p "$drivers_out"
        cp -a "$pms_dir/Drivers/." "$drivers_out/"
        echo "done; workerd serves HW jobs once drivers_dir contains the bundle"
      '';
    };
  };
}
