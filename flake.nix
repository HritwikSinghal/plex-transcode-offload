{
  description = "Plex remote-transcode SSH offload shim (prt) -- master-side flake";

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
      # as an input.
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
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
          pkg = pkgs.callPackage ./package.nix { };
        in
        {
          plex-transcode-offload = pkg;
          default = pkg;
        }
      );

      # Lets the deployment repo pull the package via overlay if it prefers that
      # to a direct package reference.
      overlays.default = final: prev: {
        plex-transcode-offload = final.callPackage ./package.nix { };
      };

      # `nix fmt` -- formats nix, bash, python, and markdown (see treefmt.nix).
      formatter = forAllSystems (system: treefmtEval.${system}.config.build.wrapper);

      devShells = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          # pytest is here for local convenience; the sandboxed check below
          # uses stdlib unittest only, to keep its closure minimal.
          default = pkgs.mkShell {
            packages = [
              pkgs.python3
              pkgs.python3Packages.pytest
              pkgs.shellcheck
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
          # Pure-stdlib unittest suite -- needs only python3. ${./.} copies the
          # whole repo into the store so bin/ and tests/ are both present.
          tests = pkgs.runCommand "prt-tests" { nativeBuildInputs = [ pkgs.python3 ]; } ''
            cd ${./.} && python3 -m unittest discover -s tests -v && touch $out
          '';
          shellcheck = pkgs.runCommand "prt-shellcheck" { nativeBuildInputs = [ pkgs.shellcheck ]; } ''
            cd ${./.} && shellcheck bin/prt-status install/install-master.sh install/install-worker.sh && touch $out
          '';
          # Fails `nix flake check` if any tracked file is not formatted.
          formatting = treefmtEval.${system}.config.build.check self;
        }
      );

      apps = forAllSystems (
        system:
        let
          pkg = (pkgsFor system).callPackage ./package.nix { };
        in
        {
          # prt-status is the diagnostic entry point. prt-transcoder is NOT
          # exposed as an app: it needs /etc/prt config and is only meaningful
          # when Plex invokes it directly.
          prt-status = {
            type = "app";
            program = "${pkg}/bin/prt-status";
          };
        }
      );
    };
}
