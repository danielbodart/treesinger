# The broker: the same static build as ./run, by nixpkgs' Go. go.mod requires
# nothing, so there are no modules to vendor (vendorHash = null), and the
# check phase runs the tests on the way.
# buildGo127Module rather than buildGoModule, which is still Go 1.26 in the
# pinned nixpkgs and would refuse go.mod's `go 1.27`.
{ lib, buildGo127Module, version ? "development" }:

buildGo127Module {
  pname = "treesinger";
  inherit version;

  # go.mod and every .go file, tests included, and the tests' recorded
  # upstream bodies; so editing the docs or the flake does not rebuild it.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      (lib.fileset.fileFilter (file: file.hasExt "go") ../.)
      ../internal/broker/testdata
    ];
  };

  vendorHash = null;

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X github.com/danielbodart/treesinger/internal/broker.Version=${version}"
  ];

  meta = {
    description = "Central authentication broker for Hytale dedicated servers";
    homepage = "https://github.com/danielbodart/treesinger";
    license = lib.licenses.asl20;
    mainProgram = "treesinger";
    platforms = [ "x86_64-linux" "aarch64-linux" ];
  };
}
