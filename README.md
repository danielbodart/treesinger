![Treesinger Logo](logo.png | width=300)

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

Endpoint auth is **off by default** — the boundary is Docker network isolation.
Set `TREESINGER_BEARER_TOKEN` to require `Authorization: Bearer <token>` on
`/v1/session` (`/health` stays open).

## Configuration

All via environment; every value has a default except the refresh token, which
`login` seeds into the store.

| Variable                  | Default                                  | Meaning |
|---------------------------|------------------------------------------|---------|
| `TREESINGER_STORE`        | `$XDG_CONFIG_HOME/treesinger/tokens.json`| Token store path (put it on a volume). |
| `TREESINGER_PORT`         | `8080`                                   | Listen port. |
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

## Development

```sh
bun test          # all in-memory: fake upstream fetch + a stopped clock
bun run typecheck
```

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
