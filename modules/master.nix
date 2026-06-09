# Generic prt-masterd NixOS module: the master-side daemon (:32499) that
# receives segments, serves media/codecs to workers, relays PMS callbacks,
# and caches worker health.
#
# Deliberately site-agnostic: the consumer sets `package`, fills `settings`
# (rendered verbatim to the --config JSON; see internal/config MasterdConfig
# for the schema), and grants filesystem access via readWritePaths /
# readOnlyPaths. Swapping PMS's "Plex Transcoder" for the shim and injecting
# PRT_CONF / PRT_TOKEN_FILE into the PMS unit is also the consumer's job.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.services.prt-masterd;
  settingsFormat = pkgs.formats.json { };
  configFile = settingsFormat.generate "prt-masterd.json" (
    cfg.settings
    // {
      token_file = cfg.tokenFile;
    }
  );
in
{
  options.services.prt-masterd = {
    enable = lib.mkEnableOption "prt master daemon";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The prt package to run (no default: the consumer pins it).";
    };

    settings = lib.mkOption {
      type = settingsFormat.type;
      default = { };
      description = ''
        prt-masterd JSON config (MasterdConfig schema). Required keys:
        transcode_root, media_roots, codecs_dir, workers. token_file is
        injected from `tokenFile`.
      '';
      example = lib.literalExpression ''
        {
          transcode_root = "/var/lib/plex/transcode";
          media_roots = [ "/mnt/media" ];
          codecs_dir = "/var/lib/plex/Plex Media Server/Codecs";
          workers = [ "http://10.0.50.52:32500" ];
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
      description = "User the daemon runs as (must be able to write the PMS session dirs).";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "plex";
      description = "Group the daemon runs as.";
    };

    readWritePaths = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "ReadWritePaths for the unit; typically the PMS transcode root.";
      example = [ "/var/lib/plex/transcode" ];
    };

    readOnlyPaths = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "ReadOnlyPaths for the unit; typically the media roots and the Codecs dir.";
      example = [
        "/mnt/media"
        "/var/lib/plex/Plex Media Server/Codecs"
      ];
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.prt-masterd = {
      description = "prt master daemon (segment receiver, media/codecs server, PMS relay)";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      serviceConfig = {
        ExecStart = "${cfg.package}/bin/prt-masterd --config ${configFile}";
        User = cfg.user;
        Group = cfg.group;
        Restart = "on-failure";
        RestartSec = 2;

        # Hardening. ProtectSystem=strict makes the whole FS read-only for
        # the unit; access is granted back explicitly via the path options.
        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        ReadWritePaths = cfg.readWritePaths;
        ReadOnlyPaths = cfg.readOnlyPaths;
      };
    };
  };
}
