{
  description = "Treesinger: central authentication broker for Hytale dedicated servers";

  # The release nix-config builds every host from. The package needs little
  # from it (a derivation that installs one file); the module check and the
  # VM test need the rest.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      # The systems the checks run on. The package is for every system the
      # release has a binary for, armv7l-linux (an older 32-bit Pi) as well.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # THE PACKAGE IS THE RELEASE BINARY, not a build from source (Dan,
      # 2026-11-15): every binary is built once, by mise's Go in CI's dist
      # job, tested in every channel, signed and published, and building it
      # again here only made a second binary that was not the one released.
      # So a release's tag is a commit of its own, a child of the released
      # trunk commit, that adds nix/release.json: the version, and for each
      # system the binary's URL and hash (./run release-json writes it;
      # ci.yml's release job commits and tags it). A flake fetched as
      # github:danielbodart/treesinger/v<V> sees its commit but not its tag,
      # which is why the version is in the file.
      #
      # On trunk there is no such file and no package: pin a release tag.
      # Absent rather than an error, so `nix flake check` still works there.
      releaseFile = ./nix/release.json;
      release = if builtins.pathExists releaseFile then lib.importJSON releaseFile else null;

      # The static binary installed as bin/treesinger, byte for byte the
      # release's: no stripping or patching, which a static Go binary does
      # not need, and which would make it differ from SHA256SUMS.
      mkTreesinger =
        pkgs: asset:
        pkgs.stdenvNoCC.mkDerivation {
          pname = "treesinger";
          inherit (release) version;
          src = pkgs.fetchurl { inherit (asset) url hash; };
          dontUnpack = true;
          dontConfigure = true;
          dontBuild = true;
          installPhase = ''
            runHook preInstall
            install -Dm0755 $src $out/bin/treesinger
            runHook postInstall
          '';
          dontFixup = true;
          meta = {
            description = "Central authentication broker for Hytale dedicated servers";
            homepage = "https://github.com/danielbodart/treesinger";
            license = lib.licenses.asl20;
            mainProgram = "treesinger";
            # x86_64, aarch64 and armv7l, as release.json lists them.
            platforms = builtins.attrNames release.assets;
            sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
          };
        };

      # The release's binary for pkgs' system.
      releaseFor =
        pkgs:
        let
          system = pkgs.stdenv.hostPlatform.system;
        in
        if release.assets ? ${system} then
          mkTreesinger pkgs release.assets.${system}
        else
          throw "treesinger ${release.version} has no release binary for ${system}";
    in
    {
      # pkgs.treesinger, which the module alone defaults to: the release's
      # binary, never a build from source. From trunk, it adds nothing. Which
      # attributes it adds depends only on release.json, never on `final`,
      # which would recurse.
      overlays.default =
        if release == null then
          final: prev: { }
        else
          final: prev: {
            treesinger = releaseFor final;
          };

      packages =
        if release == null then
          { }
        else
          lib.mapAttrs (
            system: _:
            let
              treesinger = releaseFor nixpkgs.legacyPackages.${system};
            in
            {
              inherit treesinger;
              default = treesinger;
            }
          ) release.assets;

      # services.treesinger, with this flake's package unless one is given
      # (the module alone defaults to pkgs.treesinger, from the overlay). Only
      # a release tag's flake has a package (see above).
      nixosModules.default =
        { lib, pkgs, ... }:
        let
          system = pkgs.stdenv.hostPlatform.system;
        in
        {
          imports = [ ./nix/module.nix ];
          services.treesinger.package = lib.mkDefault (
            self.packages.${system}.default or (throw ''
              services.treesinger.package: this flake has no package for ${system}.
              Pin a release, github:danielbodart/treesinger/v<version>, or set the package.
            '')
          );
        };
      nixosModules.treesinger = self.nixosModules.default;

      # The tests, gofmt and vet are mise run check's. The module check
      # renders configurations with a stand-in package, so it runs on trunk
      # too; the VM test runs the release's package, so it exists where
      # nix/release.json does: on a release tag, and in CI's nix job, which
      # writes one for the binaries its own run built (./run nix-local-release).
      checks = forAllSystems (
        pkgs:
        {
          module = import ./nix/module-check.nix { inherit pkgs self; };
        }
        // lib.optionalAttrs (self.packages ? ${pkgs.stdenv.hostPlatform.system}) {
          vm = import ./nix/vm-test.nix { inherit pkgs self; };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
