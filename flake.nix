{
  description = "Command-line client for Scooter";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    systems.url = "github:nix-systems/default";
  };

  outputs = inputs@{ flake-parts, systems, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = import systems;

      perSystem = { pkgs, self', ... }: {
        packages.scooter = pkgs.buildGoModule {
          pname = "scooter";
          version = "0.1.0";
          src = ./.;
          subPackages = [ "cmd/scooter" ];
          vendorHash = null;
          meta.mainProgram = "scooter";
        };
        packages.default = self'.packages.scooter;

        checks.scooter = self'.packages.scooter;

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.golangci-lint
            pkgs.just
          ];
        };
      };
    };
}
