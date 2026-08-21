{
  description = "docket — agent-facing mail & calendar CLI";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "docket";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-9+jYyNOePa1N4nfVe/MsS0DcPvVwX2gry6WVQjAufvE=";
          # The suite is hermetic — the Gmail client is driven through its
          # endpoint against an in-process fake — so it is safe to run in a
          # sandboxed build with no network.
          doCheck = true;
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools
            golangci-lint
            delve
          ];
        };
      });
}
