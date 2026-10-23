{
  description = "Treesinger: central authentication broker for Hytale dedicated servers";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # ./run's major.commit-count.build, from what Nix knows: the count is
      # there for a clean git checkout, a fetch by tarball (github:) has only the
      # revision, and a dirty tree has neither, so says so.
      major = lib.fileContents ./MAJOR;
      version =
        if self ? revCount then "${major}.${toString self.revCount}.0"
        else if self ? shortRev then "${major}.0.0-${self.shortRev}"
        else "development";
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
