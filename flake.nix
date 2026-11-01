{
  description = "Treesinger: central authentication broker for Hytale dedicated servers";

  # The release nix-config builds every host from, which has go_1_27, the Go
  # that go.mod asks for and mise.toml pins.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # nixpkgs' convention for a build from a commit rather than a release:
      # the commit's date, so it never claims to be one of ./run's versions,
      # and it is the same however the flake was fetched (a github: input has
      # no commit count).
      date = self.lastModifiedDate or "19700101";
      version = "0-unstable-${builtins.substring 0 4 date}-${builtins.substring 4 2 date}-${builtins.substring 6 2 date}";

      src = lib.fileset.toSource {
        root = ./.;
        # go.mod, go.sum and every .go file, tests included, and the tests'
        # recorded upstream bodies; so editing the docs or the flake does not
        # rebuild it.
        fileset = lib.fileset.unions [
          ./go.mod
          ./go.sum
          (lib.fileset.fileFilter (file: file.hasExt "go") ./.)
          ./internal/broker/testdata
        ];
      };
    in
    {
      # pkgs.treesinger, which the module alone defaults to.
      overlays.default = final: prev: {
        treesinger = final.callPackage ./nix/package.nix { inherit version src; };
      };

      packages = forAllSystems (
        pkgs:
        let
          treesinger = pkgs.callPackage ./nix/package.nix { inherit version src; };
        in
        {
          inherit treesinger;
          default = treesinger;
        }
      );

      # services.treesinger, with this flake's package unless one is given
      # (the module alone defaults to pkgs.treesinger, from the overlay).
      nixosModules.default =
        { lib, pkgs, ... }:
        {
          imports = [ ./nix/module.nix ];
          services.treesinger.package =
            lib.mkDefault
              self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        };
      nixosModules.treesinger = self.nixosModules.default;

      checks = forAllSystems (pkgs: {
        # Its check phase is `go test`, so building it runs the tests.
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        module = import ./nix/module-check.nix { inherit pkgs self; };
        vm = import ./nix/vm-test.nix { inherit pkgs self; };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
