{
  description = "Treesinger: central authentication broker for Hytale dedicated servers";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # run.ts's major.commit-count.build, from what Nix knows: the count is
      # there for a clean git checkout, a fetch by tarball (github:) has only the
      # revision, and a dirty tree has neither, so says so.
      major = lib.versions.major (lib.importJSON ./package.json).version;
      version =
        if self ? revCount then "${major}.${toString self.revCount}.0"
        else if self ? shortRev then "${major}.0.0-${self.shortRev}"
        else "development";

      # The package's build with the tests beside the source, running one
      # run.ts command in place of the bundle.
      runCheck = treesinger: name: command: treesinger.overrideAttrs {
        pname = "treesinger-${name}";
        src = lib.fileset.toSource {
          root = ./.;
          fileset = lib.fileset.unions [ ./package.json ./bun.lock ./bunfig.toml ./tsconfig.json ./src ./run.ts ./test ];
        };
        buildPhase = ''
          runHook preBuild
          bun run.ts ${command}
          runHook postBuild
        '';
        installPhase = "touch $out";
      };
    in
    {
      overlays.default = final: prev: {
        treesinger = final.callPackage ./nix/package.nix { inherit version; };
        treesinger-env = final.callPackage ./nix/treesinger-env.nix { };
      };

      packages = forAllSystems (pkgs:
        let treesinger = pkgs.callPackage ./nix/package.nix { inherit version; };
        in {
          inherit treesinger;
          default = treesinger;
          treesinger-env = pkgs.callPackage ./nix/treesinger-env.nix { };
        });

      checks = forAllSystems (pkgs:
        let treesinger = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        in {
          package = treesinger;
          typecheck = runCheck treesinger "typecheck" "check";
          test = runCheck treesinger "test" "test";
          treesinger-env = self.packages.${pkgs.stdenv.hostPlatform.system}.treesinger-env;
        });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShellNoCC { packages = [ pkgs.bun ]; };
      });
    };
}
