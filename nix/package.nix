# The broker: src/ bundled by bun into one JS file, run by nixpkgs' bun.
# A bundle rather than `bun build --compile`, whose executable is a copy of
# bun with the JS appended, which the fixup phase's strip and patchelf can
# mangle; the runtime closure is the same bun either way, and the bundle needs
# no node_modules once built.
{ lib, stdenvNoCC, bun, makeWrapper, callPackage, version ? "development" }:

let
  nodeModules = callPackage ./node-modules.nix { };
in
stdenvNoCC.mkDerivation (finalAttrs: {
  pname = "treesinger";
  inherit version;

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../package.json
      ../bun.lock
      ../bunfig.toml
      ../tsconfig.json
      ../src
    ];
  };

  nativeBuildInputs = [ bun makeWrapper ];

  configurePhase = ''
    runHook preConfigure
    # Copied, not linked: bun resolves through a real tree.
    cp -R ${nodeModules}/node_modules .
    chmod -R u+w node_modules
    export HOME=$TMPDIR
    runHook postConfigure
  '';

  buildPhase = ''
    runHook preBuild
    bun build --target=bun --minify --define 'TREESINGER_VERSION="${finalAttrs.version}"' \
      src/main.ts --outfile treesinger.js
    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    install -Dm444 treesinger.js $out/lib/treesinger/treesinger.js
    # --no-env-file and an empty config: bun would otherwise read a .env and
    # bunfig.toml from whatever directory treesinger is run in.
    makeWrapper ${lib.getExe bun} $out/bin/treesinger \
      --add-flags "--no-env-file --config=/dev/null $out/lib/treesinger/treesinger.js"
    runHook postInstall
  '';

  passthru = {
    inherit nodeModules;
  };

  meta = {
    description = "Central authentication broker for Hytale dedicated servers";
    homepage = "https://github.com/danielbodart/treesinger";
    license = lib.licenses.asl20;
    mainProgram = "treesinger";
    platforms = [ "x86_64-linux" "aarch64-linux" ];
  };
})
