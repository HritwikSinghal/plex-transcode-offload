{ ... }:
{
  # Used to find the project root.
  projectRootFile = "flake.nix";

  # Nix (flake.nix, modules/, this file).
  programs.nixfmt.enable = true;

  # Go (cmd/, internal/).
  programs.gofmt.enable = true;

  # Markdown docs (README, CLAUDE.md).
  programs.mdformat.enable = true;
}
