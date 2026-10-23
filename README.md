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

The broker needs no Tidewaiter or Portical: restarting it never disturbs a
running server (servers only ask it for a session when they start), and it must
never be forwarded to the internet.

## Running it

How to run it depends on where your servers run:

- **[Run it in Docker](docs/docker.md)**, beside servers in containers on the
  same compose network. The image is `ghcr.io/danielbodart/treesinger`.
- **[Run it on systemd](docs/systemd.md)**, beside servers running natively, on a
  unix socket only their group can reach: from the flake's NixOS module, or from
  a release binary and unit on any systemd distribution.

The rest of this page applies however it runs.

## Quick start

From a checkout:

```sh
mise install                 # pins Bun
bun install

# One-time device login — prints a URL + code, writes the refresh token.
bun run login

# Run the broker.
bun run start                # listens on :8080
```

Or with the released binary, as `treesinger login` then `treesinger serve` - each
page above has the commands for its setup.

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

Each trunk push publishes a GitHub Release `v<version>` and a Docker image to
GHCR (`ghcr.io/danielbodart/treesinger:<version>` and `:latest`), both for
`x64` and `arm64`. The release carries standalone binaries
(`treesinger-linux-x64`, `treesinger-linux-arm64`), the plain systemd unit and
sysusers file, `treesinger-env`, and `SHA256SUMS`. A pull request builds
everything but publishes nothing.

```sh
bun run.ts version   # what this checkout would publish as
bun run.ts build     # compile dist/treesinger with the version baked in
bun run.ts release   # the release's binaries, unit files and checksums, into dist/release/
bun run.ts image     # build the same image CI publishes, locally
```

## License

Apache-2.0.
