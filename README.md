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
- **[Run it on systemd](docs/systemd.md)**, beside servers running natively,
  socket-activated on a unix socket only their group can reach: from the
  flake's NixOS module, or from a release binary and units on any systemd
  distribution.

The rest of this page applies however it runs.

## Quick start

From a checkout:

```sh
mise install                 # pins Go

# One-time device login — prints a URL + code, writes the refresh token.
go run . login

# Run the broker.
go run . serve               # listens on :8080
```

Or with the released binary, as `treesinger login` then `treesinger serve` - each
page above has the commands for its setup.

## HTTP surface

| Method & path        | Purpose                                                             |
|----------------------|---------------------------------------------------------------------|
| `POST /v1/session`   | Mint `{ sessionToken, identityToken, expiresAt }`. Optional body `{ "uuid": "<profile>" }` overrides which profile. What a booting server calls. |
| `GET /health`        | `200` only when a mint would succeed (refresh token present **and** a valid access token obtainable). Backs compose `service_healthy`. |
| `POST /v1/login`     | Runs the device login in the broker, streaming newline-delimited JSON: the verification URL and code, then `{"ok":true}` or `{"error":"..."}`. One at a time (`409` otherwise). **Only on the admin socket** under systemd, never on TCP; what `treesinger login` uses. |

Endpoint auth is **off by default** — the boundary is Docker network isolation,
or under systemd the unix socket's group.
Set a bearer token (below) to require `Authorization: Bearer <token>` on
`/v1/session` (`/health` stays open).

## Commands

One binary:

| Command | |
|---|---|
| `treesinger serve` | Run the broker. Under systemd it serves the sockets systemd passes it (socket activation); otherwise TCP on `TREESINGER_HOST:TREESINGER_PORT`. |
| `treesinger login [--socket PATH]` | The one-time device login. Through the running broker's admin socket (`/run/treesinger-admin.sock`, so `sudo treesinger login`) when it exists; otherwise locally, into `TREESINGER_STORE` (how Docker logs in). |
| `treesinger exec [--socket PATH \| --url URL] -- COMMAND [ARGS...]` | Mint a session (default socket `/run/treesinger/treesinger.sock`), put it in `HYTALE_SERVER_SESSION_TOKEN` / `HYTALE_SERVER_IDENTITY_TOKEN`, then become `COMMAND`. On any failure it warns and runs `COMMAND` unauthenticated. For a game server's `ExecStart`. |
| `treesinger health [--socket PATH \| --url URL]` | Probe a broker: exit `0` only on `200`. Defaults to `http://127.0.0.1:$TREESINGER_PORT/health`, the image's `HEALTHCHECK`. |
| `treesinger version` | What this build calls itself. |

## Configuration

All via environment; every value has a default except the refresh token, which
`login` seeds into the store.

| Variable                  | Default                                  | Meaning |
|---------------------------|------------------------------------------|---------|
| `TREESINGER_STORE`        | `$XDG_CONFIG_HOME/treesinger/tokens.json`| Token store path (put it on a volume). |
| `TREESINGER_PORT`         | `8080`                                   | TCP port to listen on (and for `health` to probe). Not when socket-activated: the socket units have the listeners then, and setting it is an error. |
| `TREESINGER_HOST`         | `0.0.0.0`                                | Address the TCP listener binds. Likewise an error when socket-activated. |
| `TREESINGER_SKEW_MS`      | `300000` (5 min)                         | Refresh the access token when less than this much life remains. |
| `TREESINGER_OWNER_UUID`   | first profile                            | Which profile to mint for when the account has several. |
| `TREESINGER_BEARER_TOKEN` | `$CREDENTIALS_DIRECTORY/bearer-token`, else unset | Optional shared secret for `/v1/session`, for `serve` and `exec`. Under systemd, give it as `LoadCredential=bearer-token:/path` rather than in the environment (a trailing newline is trimmed). `exec` removes the variable before it runs its command, but a credential file stays readable by that command for the life of its unit (see docs/systemd.md). |

## Development

```sh
mise install      # installs the pinned Go, and cosign
mise run check    # gofmt, go vet and the tests, all in-memory: a fake upstream handler + a stopped clock
nix flake check   # the package (its build runs the tests) and the VM test
```

Go and the standard library, and one module of shared helpers,
[gokit](https://github.com/danielbodart/gokit), which needs nothing but the
standard library itself.

**Design.** One container builds the whole graph (`internal/broker/container.go`):
production wires it once, each test wires a fresh one after setting the leaf
doubles (`Transport`, `Clock`, `Store`, `Config`, ...) — a leaf left unset gets
its production default when `Wire()` runs, and everything downstream is derived,
never set. The upstream client is an `http.RoundTripper`, with retrying and
logging as gokit's `httpx` decorators around it, and the broker itself is an
`http.Handler`; `httpx.HandlerTransport` serves a handler in memory as a round
tripper, so the fake
Hytale in the tests is just a handler replaying recorded response shapes, with
no sockets or ports. All time-driven decisions are pure functions of the clock
and stored expiry.

Concurrent refreshes are coalesced onto a single in-flight refresh (see
`OAuthClient`), so the rotating refresh token is spent exactly once no matter how
many servers boot at the same moment — this, not a lock, is what makes a
double-spend structurally impossible. It assumes a single writer process; running
multiple broker instances against one store would reintroduce the race.

## Channels

- **Docker**: `docker run danielbodart/treesinger` (or
  `ghcr.io/danielbodart/treesinger`), for amd64, arm64 and armv7 - image `FROM scratch` + the binary `mise run dist` builds, with the
  CA bundle and the store's directory from a digest-pinned Alpine.
- **Tarball + systemd**: `treesinger-linux-x86_64.tar.gz`, `-aarch64.tar.gz`
  and `-armv7.tar.gz` on each [GitHub
  release](https://github.com/danielbodart/treesinger/releases), each the
  binary, `treesinger.service`, `treesinger.socket`,
  `treesinger-admin.socket`, `treesinger.sysusers`, README and LICENSE:
  unpack, install the binary as `/usr/local/bin/treesinger`, the units into
  `/etc/systemd/system` and the sysusers file into `/etc/sysusers.d`
  ([docs/systemd.md](docs/systemd.md)). The release also carries each plain
  binary (`treesinger-linux-x64`, `-arm64`, `-armv7`, the names the first releases gave
  them), the units, `SHA256SUMS`, and a cosign keyless signature of each asset
  (`cosign verify-blob --bundle <asset>.sigstore.json
  --certificate-identity-regexp '^https://github.com/danielbodart/treesinger/'
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
  <asset>`).
- **Nix**: flake input `github:danielbodart/treesinger/v<version>` with
  `inputs.nixpkgs.follows = "nixpkgs"`; `packages.default`, and
  `nixosModules.default` (`services.treesinger`), and `overlays.default`
  (`pkgs.treesinger`). Built by nixpkgs (nixos-26.05, `go_1_27`) and cached at
  `danielbodart.cachix.org`.

Updated weekly (Sunday 21:23 UTC): the flake's inputs, mise's tools to the
latest patch of their minor, the base image's digest and the Go modules,
committed straight to `trunk` once every channel's tests pass. A release is
cut when the Go source, `go.mod`/`go.sum`, `mise.toml`, `./run`, the
`Dockerfile` or `contrib/systemd` changed since the last release
(`RELEASE_PATHS` in `mise.toml`), and only once `mise run check`, the flake
check with its module check and NixOS VM test, the image run and the tarball
installed under systemd pass.

## Releases

**`trunk` is production.** Every push to it that changes what a release is built
from (`RELEASE_PATHS` in `mise.toml`) publishes, once every channel's tests pass:
there are no manual tags and no release branches. The version is *derived from the repository*, not stored
in it — `major.commit-count.ci-run` (see `./run`). The major is the one
deliberate part (in `MAJOR`); the minor only ever rises and names exactly
one commit; the patch separates two builds of the same commit. `treesinger
version` reports what a build calls itself (`development` when run from source).

Each release publishes a GitHub Release `v<version>` and a Docker image to
GHCR (`ghcr.io/danielbodart/treesinger:<version>` and `:latest`), both for
amd64 and arm64, its notes the commits since the last release. The release
carries standalone binaries (`treesinger-linux-x64`, `treesinger-linux-arm64`),
the plain systemd units (`treesinger.service`, `treesinger.socket`,
`treesinger-admin.socket`), the sysusers file, a tarball per architecture
holding all of them with the README and LICENSE
(`treesinger-linux-x86_64.tar.gz`, `treesinger-linux-aarch64.tar.gz`),
`SHA256SUMS`, and a sigstore bundle for each. A pull request
builds and tests everything but publishes nothing.

The binaries are static, so one build runs on any distribution, and the image
is that binary on an empty base.

```sh
./run version   # what this checkout would publish as
./run build     # compile dist/treesinger with the version baked in
./run dist      # the release's binaries, tarballs, unit files and checksums, into dist/
./run smoke     # the image for this machine from dist/, run: it must say its version
./run image     # build the same image CI publishes, from dist/
```

## License

Apache-2.0.
