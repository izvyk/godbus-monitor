{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = with pkgs; [
    go
    gopls

    nil
    nixd
    package-version-server
  ];
}
