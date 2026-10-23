# node_modules as a fixed-output derivation: the only step that touches the
# network, pinned by hash. Every dependency is plain JS, so one hash serves
# every system. When bun.lock changes, so does the hash: set it to
# lib.fakeHash, build, and copy the hash from the error.
{ lib, stdenvNoCC, bun, cacert }:

stdenvNoCC.mkDerivation {
  pname = "treesinger-node-modules";
  version = "0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [ ../package.json ../bun.lock ../.npmrc ];
  };

  nativeBuildInputs = [ bun cacert ];

  impureEnvVars = lib.fetchers.proxyImpureEnvVars;

  dontConfigure = true;

  buildPhase = ''
    runHook preBuild
    export HOME=$TMPDIR BUN_INSTALL_CACHE_DIR=$TMPDIR/bun-cache
    bun install --frozen-lockfile --ignore-scripts --no-progress
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p $out
    cp -R node_modules $out/
    runHook postInstall
  '';

  # Nothing to patch, and patching would change the hash.
  dontFixup = true;

  outputHashAlgo = "sha256";
  outputHashMode = "recursive";
  outputHash = "sha256-L0U+AeAy++Kc3E0lNLBXl1pUDClD9FSU8eQv/lrA10U=";
}
