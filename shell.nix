{ pkgs ? import <nixpkgs> {} }:

pkgs.mkShell {
  packages = with pkgs; [
    go
    gopls
    dbus # dbus-daemon, for the private-bus integration tests

    nil
    nixd
    package-version-server
  ];
}
