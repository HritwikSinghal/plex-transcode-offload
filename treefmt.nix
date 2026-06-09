{ ... }:
{
  # Used to find the project root.
  projectRootFile = "flake.nix";

  # Nix (flake.nix, package.nix, this file).
  programs.nixfmt.enable = true;

  # Bash/sh (bin/prt-status, install/*.sh). Match the existing 4-space indent
  # so the formatter does not churn the scripts.
  programs.shfmt.enable = true;
  programs.shfmt.indent_size = 4;

  # Python shim + tests. The code is already black-style, so this is near-noop.
  programs.black.enable = true;

  # Markdown docs (README, claude/*.md).
  programs.mdformat.enable = true;
}
