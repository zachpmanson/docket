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
          vendorHash = "sha256-JZIurQPik+IvIUXv9TQbqb7tIg1KTKkdALlOCYFvAFg=";
          # gmail/ is a nested Go module (own go.mod) consumed via the
          # root module's replace; buildGoModule's per-directory build
          # loop must not treat it as subpackages of the root.
          excludedPackages = [ "gmail" ];
          # The nested Gmail module is a local replace. buildGoModule's
          # goModules derivation hashes go.mod/go.sum, so it can retain stale
          # copies of local replacement sources. Overlay the current source
          # after vendoring, before the build uses -mod=vendor.
          postConfigure = ''
            chmod -R u+w vendor/github.com/zachpmanson/docket
            rm -rf vendor/github.com/zachpmanson/docket/gmail
            cp -r gmail vendor/github.com/zachpmanson/docket/gmail
          '';
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
