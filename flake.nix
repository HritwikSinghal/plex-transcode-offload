{
  description = "prt -- HTTP-based remote Plex transcoder (shim + masterd + workerd)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # `nix fmt` + the formatting check.
    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      treefmt-nix,
    }:
    let
      # Hand-rolled multi-system via genAttrs so we avoid pulling in flake-utils
      # as an input. The daemons only make sense on Linux, but the binary
      # builds everywhere.
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      pkgsFor = system: nixpkgs.legacyPackages.${system};

      # Eval the treefmt modules from ./treefmt.nix, per system.
      treefmtEval = forAllSystems (system: treefmt-nix.lib.evalModule (pkgsFor system) ./treefmt.nix);

      prtFor =
        system:
        let
          pkgs = pkgsFor system;
        in
        pkgs.buildGoModule {
          pname = "prt";
          version = "2.0.0";
          src = self;

          vendorHash = "sha256-FLxr4UQtwBsjts3NN5uE3x/hpB4lizLjd6SnJuk8VPk=";

          subPackages = [ "cmd/prt" ];

          env.CGO_ENABLED = "0";
          ldflags = [
            "-s"
            "-w"
          ];

          # Role selection is by argv[0] basename; ship the canonical names.
          # The master module additionally symlinks prt-shim in as
          # "Plex Transcoder".
          postInstall = ''
            ln -s $out/bin/prt $out/bin/prt-shim
            ln -s $out/bin/prt $out/bin/prt-masterd
            ln -s $out/bin/prt $out/bin/prt-workerd
          '';

          meta = with nixpkgs.lib; {
            description = "HTTP-based remote Plex transcoder (shim, masterd, workerd)";
            homepage = "https://github.com/HritwikSinghal/plex-transcode-offload";
            license = licenses.gpl3Only;
            mainProgram = "prt";
          };
        };
    in
    {
      packages = forAllSystems (
        system:
        let
          prt = prtFor system;
        in
        {
          inherit prt;
          # Compat alias for consumers of the v1 attribute name.
          plex-transcode-offload = prt;
          default = prt;
        }
      );

      overlays.default = final: prev: {
        prt = prtFor final.stdenv.hostPlatform.system;
        plex-transcode-offload = final.prt;
      };

      nixosModules = {
        master = ./modules/master.nix;
        worker = ./modules/worker.nix;
      };

      # `nix fmt` -- formats nix, go, and markdown (see treefmt.nix).
      formatter = forAllSystems (system: treefmtEval.${system}.config.build.wrapper);

      devShells = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.gotools
              pkgs.nixfmt-rfc-style
            ];
          };
        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          # buildGoModule runs `go test ./...` in its checkPhase.
          prt = self.packages.${system}.prt;
          # Fails `nix flake check` if any tracked file is not formatted.
          formatting = treefmtEval.${system}.config.build.check self;
        }
      );
    };
}
