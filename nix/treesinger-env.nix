# contrib/treesinger-env with its curl and jq: mint a session, then exec the
# game with it in the environment. See the script for the details.
{ lib, writeShellApplication, curl, jq }:

writeShellApplication {
  name = "treesinger-env";
  runtimeInputs = [ curl jq ];
  text = builtins.readFile ../contrib/treesinger-env;
  meta = {
    description = "Run a Hytale server with a session minted by Treesinger";
    homepage = "https://github.com/danielbodart/treesinger";
    license = lib.licenses.asl20;
    platforms = lib.platforms.linux;
  };
}
