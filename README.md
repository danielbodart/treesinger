<p align="center"><img src="logo.png" alt="Logo" width="600"></p>

# Treesinger

**A central authentication broker for Hytale dedicated servers.** Sign in once;
your whole fleet inherits.

A Hytale server must authenticate to Hypixel's services (OAuth 2.0) to run
online. Doing that *per server* means a device-login per server and a long-lived
credential in every container. Treesinger does it **once**: the operator logs in
a single time, Treesinger holds the one long-lived credential, and it **mints a
short-lived session for each server on demand**. Servers then keep themselves
authenticated by self-refreshing — Treesinger is not involved after boot.

> The name: a *Treesinger* is the Kweebec elder in Hytale who raises a Seedling
> and certifies it as ready to go out into the world — vouching for a new
> identity before it leaves. That is exactly this service's job.

## How it works — pull, not push

Treesinger is a **mint-on-request HTTP service**. It does not know how many
servers exist, their names, or hold any list — that is deliberate.

- It holds the **sole** OAuth refresh token and is its **only** writer, so the
  30-day rotation is never raced.
- It keeps a valid OAuth access token, refreshing on demand within a skew window.
- On request it mints a fresh `sessionToken` / `identityToken` pair for the
  operator profile.
- Each server **fetches a pair at boot**, then **self-refreshes** indefinitely
  (native game behaviour). Treesinger only re-issues when a server (re)starts.

Pull keeps the broker fleet-agnostic (adding a server is zero config) and avoids
a start-order cycle: servers `depends_on` the broker being healthy, and the
broker never waits to discover servers.

## Quick start

```sh
mise install                 # pins Bun
bun install

# One-time device login — prints a URL + code, writes the refresh token.
bun run login

# Run the broker.
bun run start                # listens on :8080
```

Or with the released binary / image:

```sh
treesinger login
treesinger serve
```

## HTTP surface

| Method & path        | Purpose                                                             |
|----------------------|---------------------------------------------------------------------|
| `POST /v1/session`   | Mint `{ sessionToken, identityToken, expiresAt }`. Optional body `{ "uuid": "<profile>" }` overrides which profile. What a booting server calls. |
| `GET /health`        | `200` only when a mint would succeed (refresh token present **and** a valid access token obtainable). Backs compose `service_healthy`. |

Endpoint auth is **off by default** — the boundary is Docker network isolation,
or under systemd the unix socket's group.
Set `TREESINGER_BEARER_TOKEN` to require `Authorization: Bearer <token>` on
`/v1/session` (`/health` stays open).

## Configuration

All via environment; every value has a default except the refresh token, which
`login` seeds into the store.

| Variable                  | Default                                  | Meaning |
|---------------------------|------------------------------------------|---------|
| `TREESINGER_STORE`        | `$XDG_CONFIG_HOME/treesinger/tokens.json`| Token store path (put it on a volume). |
| `TREESINGER_SOCKET`       | unset                                    | Listen on this unix socket (mode `0660`, the process's primary group). |
| `TREESINGER_PORT`         | `8080`, or unset when a socket is set    | Listen on this TCP port. Set it alongside a socket to listen on both. |
| `TREESINGER_HOST`         | `0.0.0.0`                                | Address the TCP listener binds. |
| `TREESINGER_SKEW_MS`      | `300000` (5 min)                         | Refresh the access token when less than this much life remains. |
| `TREESINGER_OWNER_UUID`   | first profile                            | Which profile to mint for when the account has several. |
| `TREESINGER_BEARER_TOKEN` | unset                                    | Optional shared secret for `/v1/session`. |

## Docker / compose

```yaml
services:
  treesinger:
    image: ghcr.io/danielbodart/treesinger:latest
    volumes:
      - treesinger-data:/data          # holds the token store
    networks: [fleet]
    # image ships a self-contained HEALTHCHECK (treesinger health)

  hytale-server:
    image: ghcr.io/danielbodart/chandlery-hytale:latest
    depends_on:
      treesinger:
        condition: service_healthy     # cold-start ordering; broker covers restarts
    networks: [fleet]

volumes:
  treesinger-data:
networks:
  fleet:
```

The consumer image fetches a pair from `POST /v1/session` at boot and passes them
as the game's own `--session-token` / `--identity-token`. No shared secret;
network isolation is the boundary.

Run `login` once against the same volume before the first `serve`:

```sh
docker run --rm -it -v treesinger-data:/data ghcr.io/danielbodart/treesinger login
```

## systemd

Natively, the broker listens on a **unix socket** instead of a network port:
`/run/treesinger/treesinger.sock`, mode `0660`, group `treesinger`. Being in
that group is what lets a server mint a session. There is no TCP listener
unless you set `TREESINGER_PORT` as well. The unit is `Type=notify`, so anything
ordered `After=treesinger.service` finds the socket already there.

There is no `treesinger.socket` unit: Bun cannot serve on a socket handed
over by systemd, so the service makes its own, in its runtime directory.

### Without Nix

[`contrib/systemd`](contrib/systemd) has the unit (hardened, see
`systemd-analyze security treesinger`) and a sysusers file for the
`treesinger` user and group.

```sh
sudo install -m 755 treesinger-linux-x64 /usr/local/bin/treesinger   # from Releases
sudo install -m 644 contrib/systemd/treesinger.sysusers /etc/sysusers.d/treesinger.conf
sudo install -m 644 contrib/systemd/treesinger.service /etc/systemd/system/
sudo systemd-sysusers
sudo systemctl daemon-reload
sudo systemctl enable --now treesinger

# The one-time login, as the broker's user, into its store. No restart after:
# the broker reads the store on every request.
sudo -u treesinger TREESINGER_STORE=/var/lib/treesinger/tokens.json treesinger login
```

### A Hytale server as a client

Join the group, order after the broker (which waits for
`network-online.target`, so the mint at boot can reach Hypixel), and mint the
session as the server starts.
[`contrib/treesinger-env`](contrib/treesinger-env) (needs `curl` and `jq`) does
that by wrapping `ExecStart`, so the tokens are never written to a file: it asks
the broker for a session, puts it in the variables the server
reads itself (`HYTALE_SERVER_SESSION_TOKEN` / `HYTALE_SERVER_IDENTITY_TOKEN`,
never argv), then `exec`s the server. If the broker is down or not logged in, it
warns and starts the server unauthenticated.

```ini
[Unit]
Wants=treesinger.service
After=treesinger.service

[Service]
SupplementaryGroups=treesinger
ExecStart=/usr/local/bin/treesinger-env --socket /run/treesinger/treesinger.sock -- \
    java -jar /opt/hytale/Server/HytaleServer.jar --assets /opt/hytale/Assets.zip
# A confined server: bind the broker's directory, not the socket, which a
# restart of the broker replaces. "-" boots it unauthenticated without one.
BindReadOnlyPaths=-/run/treesinger
```

An `ExecStartPre` works too: have it write the two variables to a 0600 file
in the unit's `RuntimeDirectory`, read with `EnvironmentFile=-` (systemd reads
it again before `ExecStart`), and remove the file when the mint fails, so a
stale session is never reused.

A client may use `PrivateUsers=yes`: the `treesinger` group is unmapped in its
user namespace, but the kernel checks the real group, so it still connects.

### NixOS

```nix
{
  inputs.treesinger.url = "github:danielbodart/treesinger";

  outputs = { nixpkgs, treesinger, ... }: {
    nixosConfigurations.server = nixpkgs.lib.nixosSystem {
      modules = [
        treesinger.nixosModules.default
        { services.treesinger.enable = true; }
      ];
    };
  };
}
```

Then, once, `sudo treesinger-login`: it runs the device login as the broker's
user, in the broker's sandbox and with its environment, so the token lands
where the broker reads it.

| Option | Default | |
|---|---|---|
| `enable` | `false` | |
| `package` | this flake's | |
| `user` | `"treesinger"` | Created when left as `treesinger`; any other user must exist and own `stateDir`. |
| `group` | `"treesinger"` | The socket's group; created when left as `treesinger`. |
| `stateDir` | `"/var/lib/treesinger"` | Holds `tokens.json`. Any other path must already exist; it is never created or chowned. |
| `socket` | `"/run/treesinger/treesinger.sock"` | `null` for none. |
| `port` | `null` | TCP as well. |
| `host` | `"127.0.0.1"` | TCP bind address. |
| `openFirewall` | `false` | Open `port`. |
| `ownerUuid` | `null` | Profile to mint for. |
| `bearerTokenFile` | `null` | Shared secret for `/v1/session`, read with `LoadCredential`, so never in the store: an absolute path as a string, not `./file`. |
| `environment` | `{}` | Extra environment, e.g. `TREESINGER_SKEW_MS`. |
| `environmentFile` | `null` | `EnvironmentFile=`. |

For instance, as an existing user with the state a container left behind
(its `/data/tokens.json`, here on the host) — stop the container, switch, no
new login:

```nix
services.treesinger = {
  enable = true;
  user = "dan";
  stateDir = "/data/servers/treesinger";
};
```

A client, by hand (Chandlery's `services.chandlery.servers.<n>.treesinger.socket`
does this for its Hytale servers). Ordering after the broker also waits for
`network-online.target`, which the broker wants, so the mint at boot can reach
Hypixel:

```nix
systemd.services.hytale = {
  wants = [ "treesinger.service" ];
  after = [ "treesinger.service" ];
  serviceConfig = {
    SupplementaryGroups = [ config.services.treesinger.group ];
    BindReadOnlyPaths = [ "-/run/treesinger" ];
    ExecStart = "${lib.getExe treesinger.packages.${pkgs.stdenv.hostPlatform.system}.treesinger-env} "
      + "--socket ${config.services.treesinger.socket} -- ${hytale}/bin/hytale-server";
  };
};
```

The flake also has `packages.<system>.treesinger-env`, `overlays.default`
(`pkgs.treesinger`, `pkgs.treesinger-env`) and a VM test in
`checks.x86_64-linux.nixos`. [`docs/systemd.md`](docs/systemd.md) has the
design and the reasons behind it.

The broker needs no Tidewaiter or Portical: restarting it never disturbs a
running server (servers only ask it for a session when they start), and it must
never be forwarded to the internet.

## Development

```sh
bun test          # all in-memory: fake upstream fetch + a stopped clock
bun run check     # typecheck
nix flake check   # the package, the tests and typecheck in the sandbox, and the VM test
```

The Nix build installs `node_modules` as a fixed-output derivation. When
`bun.lock` changes, set `outputHash` in `nix/node-modules.nix` to
`lib.fakeHash`, build, and copy the hash from the error.

**Design.** One yadic container factory builds the whole graph; production calls
it once, each test calls it fresh and overwrites the leaf doubles (`fetch`,
`clock`, `store`, `config`) *before* the graph is realised — the one legal
mutation, since a lazy dependency is settable until its first access. Every HTTP
surface, including the upstream client, is a `(Request) => Promise<Response>`, so
tests swap a fake that replays recorded response shapes. All time-driven
decisions are pure functions of the clock and stored expiry.

Concurrent refreshes are coalesced onto a single in-flight promise (see
`OAuthClient`), so the rotating refresh token is spent exactly once no matter how
many servers boot at the same moment — this, not a lock, is what makes a
double-spend structurally impossible. It assumes a single writer process; running
multiple broker instances against one store would reintroduce the race.

## Releases

**`trunk` is production.** Every push to it publishes: there are no manual tags
and no release branches. The version is *derived from the repository*, not stored
in it — `major.commit-count.ci-run` (see `run.ts`). The major is the one
deliberate part (in `package.json`); the minor only ever rises and names exactly
one commit; the patch separates two builds of the same commit. `treesinger
version` reports what a build calls itself (`development` when run from source).

Each trunk push builds and publishes both a standalone `bun-linux-x64` binary
(attached to a GitHub Release `v<version>`) and a Docker image to GHCR
(`ghcr.io/danielbodart/treesinger:<version>` and `:latest`). A pull request builds
everything but publishes nothing.

```sh
bun run.ts version   # what this checkout would publish as
bun run.ts build     # compile dist/treesinger with the version baked in
bun run.ts image     # build the same image CI publishes, locally
```

> arm64 is one line away in both the Dockerfile (`--target` already keys off
> `TARGETARCH`) and the workflow (`platforms:`); the fleet target was x64, so
> that is all CI builds today.

## License

Apache-2.0.
