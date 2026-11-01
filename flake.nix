{
  description = "Treesinger: central authentication broker for Hytale dedicated servers";

  # The release nix-config builds every host from, which has go_1_27, the Go
  # that go.mod asks for and mise.toml pins.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # nixpkgs' convention for a build from a commit rather than a release:
      # the commit's date, so it never claims to be one of ./run's versions,
      # and it is the same however the flake was fetched (a github: input has
      # no commit count).
      date = self.lastModifiedDate or "19700101";
      version = "0-unstable-${builtins.substring 0 4 date}-${builtins.substring 4 2 date}-${builtins.substring 6 2 date}";
    in
    {
      overlays.default = final: prev: {
        treesinger = final.callPackage ./nix/package.nix { inherit version; };
      };

      packages = forAllSystems (pkgs:
        let treesinger = pkgs.callPackage ./nix/package.nix { inherit version; };
        in {
          inherit treesinger;
          default = treesinger;
        });

      # services.treesinger, with this flake's package unless one is given
      # (the module alone defaults to pkgs.treesinger, from the overlay).
      nixosModules.default = { lib, pkgs, ... }: {
        imports = [ ./nix/module.nix ];
        services.treesinger.package =
          lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      };
      nixosModules.treesinger = self.nixosModules.default;

      checks = forAllSystems (pkgs:
        let treesinger = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        in {
          # Its check phase is `go test`, so building it runs the tests.
          package = treesinger;
        } // lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") {
          nixos = import ./nix/test.nix {
            inherit pkgs treesinger;
            module = self.nixosModules.default;
          };
        });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShellNoCC { packages = [ pkgs.go_1_27 ]; };
      });
    };
}
