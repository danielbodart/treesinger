# The broker: the same static build as ./run, by nixpkgs' Go, its one module
# (gokit) vendored by vendorHash, and the check phase runs the tests on the way.
# With go_1_27, the major.minor mise.toml pins, rather than nixpkgs' default Go,
# which is 1.26 and would refuse go.mod's `go 1.27`.
{
  lib,
  buildGoModule,
  go_1_27,
  version,
  src,
}:

(buildGoModule.override { go = go_1_27; }) {
  pname = "treesinger";
  inherit version src;

  # gokit, as `go mod vendor` lays it out. A go.sum change needs it
  # recomputed: build once with lib.fakeHash and take the hash Nix reports.
  vendorHash = "sha256-0Cg6Fn3KDmoOSjC9sDqcVNu/cnZdTLS1YlMfBPQjArU=";

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
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
  };
}
