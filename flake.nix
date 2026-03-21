{
  description = "rns-iface-email development shell";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };
  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };
      in
      {
        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.gcc
            pkgs.go
            pkgs.gopls
            pkgs.golangci-lint
          ];
          shellHook = ''
            echo "rns-iface-email dev shell — go $(go version | awk '{print $3}')"
          '';
        };
      }
    );
}
