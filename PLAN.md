# Treesinger — build plan

> **Status:** greenfield. This file is the complete build spec. Read it end to
> end before writing code. Nothing else exists in the repo yet.

## What it is

**Treesinger is a central authentication broker for Hytale dedicated servers.**
A Hytale server must authenticate to Hypixel's services (OAuth 2.0) to run
online. Doing that *per server* means a device-login per server and a credential
in every container. Treesinger does it **once**: the operator signs in a single
time, Treesinger holds the one long-lived credential, and it **mints a
short-lived session for each server on demand**. Servers then keep themselves
authenticated by self-refreshing. One login, a whole fleet inherits.

The name: a **Treesinger** is the Kweebec elder in Hytale who raises a Seedling
and certifies it as ready to go out into the world — i.e. vouches for a new
identity before it leaves. That is exactly this service's job.

## Non-negotiable: clean-room, official spec only

**Build this solely from Hypixel Studios' official documentation and our own
work.** The authoritative spec is the public **"Server Provider Authentication
Guide"** (support.hytale.com) plus the **Hytale Server Manual**. The full API
(endpoints, request/response shapes, token lifecycle) is reproduced in the
[Spec](#the-hytale-auth-spec) section below so you do not need to re-fetch it.

Do **not** search for, read, or copy any third-party implementation of this idea,
and do **not** reference any such project anywhere in the code, comments, commits,
issues, or docs. The endpoints and semantics are Hypixel's public API — facts, not
anyone's expression. Implement independently. Apache-2.0.

## The Hytale auth spec

OAuth 2.0 device-code flow (RFC 8628) + a session-issuing service. Confirmed both
from the official guide and from the server binary's own string constants.

**OAuth client:** `client_id=hytale-server`, scopes `openid offline auth:server`.

**Endpoints**

| Purpose | URL |
|---|---|
| Device authorization | `POST https://oauth.accounts.hytale.com/oauth2/device/auth` |
| Token (device grant + refresh grant) | `POST https://oauth.accounts.hytale.com/oauth2/token` |
| Get profiles | `GET https://account-data.hytale.com/my-account/get-profiles` |
| Create game session | `POST https://sessions.hytale.com/game-session/new` |
| Refresh game session | `POST https://sessions.hytale.com/game-session/refresh` |
| Terminate game session | `DELETE https://sessions.hytale.com/game-session` |
| Player-JWT validation keys | `GET https://sessions.hytale.com/.well-known/jwks.json` |

**1. Device authorization** — `POST /oauth2/device/auth`, form-encoded
`client_id=hytale-server&scope=openid offline auth:server` →
```json
{ "device_code": "...", "user_code": "ABCD-1234",
  "verification_uri": "https://accounts.hytale.com/device",
  "verification_uri_complete": "https://accounts.hytale.com/device?user_code=ABCD-1234",
  "expires_in": 900, "interval": 5 }
```
Show the user `verification_uri(_complete)` + `user_code`.

**2. Poll token** — `POST /oauth2/token`, form-encoded
`client_id=hytale-server&grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code=...`
every `interval` s. Pending → `{"error":"authorization_pending"}`. Success →
```json
{ "access_token": "...", "token_type": "Bearer", "expires_in": 3600,
  "refresh_token": "...", "scope": "openid offline auth:server" }
```

**3. Get profiles** — `GET account-data.hytale.com/my-account/get-profiles`,
`Authorization: Bearer <access_token>` →
```json
{ "owner": "<uuid>", "profiles": [ { "uuid": "<profileUuid>", "username": "..." } ] }
```

**4. Create game session** — `POST sessions.hytale.com/game-session/new`,
`Authorization: Bearer <access_token>`, body `{"uuid":"<profileUuid>"}` →
```json
{ "sessionToken": "<JWT>", "identityToken": "<JWT>", "expiresAt": "2026-01-07T15:00:00Z" }
```
These two JWTs are what a server runs with.

**5. Refresh game session** — `POST /game-session/refresh`,
`Authorization: Bearer <session_token>` → a fresh pair + new `expiresAt`. **This is
what a running server does itself — it does not touch the OAuth refresh token.**

**6. Refresh OAuth token** — `POST /oauth2/token`,
`client_id=hytale-server&grant_type=refresh_token&refresh_token=<rt>` → new
`access_token` **and a new `refresh_token`; the old refresh token is invalidated
(rotation).** Store the new one before doing anything else.

**Token lifetimes**

| Token | TTL | Notes |
|---|---|---|
| OAuth access token | 1 h | mints sessions, calls service APIs |
| OAuth refresh token | 30 days | **rotates on every use**; old invalidated. Stays alive indefinitely while used; re-auth only if unused > 30 days |
| Game session (session/identity) | 1 h | server auto-refreshes 5 min before expiry via `/game-session/refresh` |

**Limits / errors:** one account = **≤ 500 concurrent server sessions** (`403`
over that). `401` invalid/expired auth; `403` insufficient entitlement/limit.

## Architecture — pull, not push

Treesinger is a **mint-on-request HTTP service**. It does **not** know how many
servers exist, their names, or hold a list — that was a deliberate rejection.

- It holds the **sole** OAuth refresh token (the one login) and is the **only**
  writer of it → the 30-day rotation is never raced.
- It keeps a valid OAuth access token (refresh-on-demand with skew).
- On request it calls `/game-session/new` for the operator profile and returns a
  fresh `sessionToken`/`identityToken` pair.
- Each Hytale server **fetches a pair at boot**, then **self-refreshes** its own
  session indefinitely (native game behaviour) — Treesinger is not involved after
  boot. It only re-issues when a server (re)starts.

Why pull: the provisioner stays fleet-agnostic (adding a server is zero config),
and there is no start-order cycle (a push/enumerate design deadlocks: servers
`depends_on` the broker being ready, while the broker would be waiting to
discover the servers). See the consumer contract below.

## Stack & conventions

- **Runtime/tooling:** `mise` (pin bun + any tools) + **Bun** + **TypeScript**.
- **DI:** **`@bodar/yadic`** (`jsr:@bodar/yadic`) — `LazyMap` container, lazy +
  cached + type-tracked. `instance()`, `constructor()`, `alias()`, `.decorate()`.
- **License:** Apache-2.0 (matches yadic).
- **HTTP is a function:** every HTTP surface is `(Request) => Promise<Response>`
  (web-standard `Request`/`Response`). The broker's own handler is one; the
  upstream client is an injected `fetch`-shaped function so tests swap fakes.
  Prod: `Bun.serve({ fetch: container().app })`.

### Dependency injection (yadic)

One **container factory** builds the whole graph; prod calls it once, each test
calls it fresh. Injected seams:

| key | prod | test double |
|---|---|---|
| `fetch` | `globalThis.fetch.bind(globalThis)` | in-memory `(Request)=>Promise<Response>` replaying recorded real responses |
| `clock` | `SystemClock` | `StoppedClock` |
| `store` | file-on-volume token store | in-memory store |
| `mutex` | in-process async mutex (single-writer refresh) | same / instrumented |
| `logger` | `console` | capturing |

`.decorate('fetch', …)` adds retry/logging as same-shape wrappers.

### Clock

```ts
interface Clock { now(): Date }
class SystemClock  implements Clock { now() { return new Date(); } }
class StoppedClock implements Clock {
  constructor(private t: Date) {}
  now() { return this.t; }
  advance(ms: number) { this.t = new Date(this.t.getTime() + ms); }
}
```
All time-driven decisions (expiry, skew, refresh-before-expiry) read `clock.now()`
and are **pure functions of clock + stored expiry**. Prod wraps the decision in a
`setInterval` poll; tests drive it with `StoppedClock.advance()` + a manual tick.
No `setTimeout`/`sleep` in tests.

### Test pattern — override before realize (important)

yadic parent/child is for **extension, not override**: a child re-`set`ting a key
is invisible to the parent's factories. So there is **no test child container**.
Build the production container and **overwrite the leaf doubles before anything
accesses them** — the one legal mutation, because a lazy prop is settable until
its first access, after which it freezes to a read-only property.

```ts
const c = container();                       // the real graph
const clock = new StoppedClock(new Date('2026-08-20T00:00:00Z'));
c.set('clock', instance(clock));             // unobserved: nothing read clock yet
c.set('fetch', instance(fakeHytale));
c.set('store', constructor(MemoryTokenStore));
const res = await c.app(new Request('http://x/v1/session', { method: 'POST' })); // realizes here
clock.advance(55 * 60_000);
```

The container is a **factory** so each test gets an isolated graph.

## HTTP surface (treesinger's own)

- `POST /v1/session` → mint and return `{ sessionToken, identityToken, expiresAt }`
  for the operator profile. (Optionally accept a profile uuid in the body; default
  to the configured/owner profile.) This is what a booting server calls.
- `GET /health` → 200 only when a refresh token is present **and** a valid OAuth
  access token is obtainable — i.e. "ready to mint." This backs compose
  `depends_on: { condition: service_healthy }`.
- Endpoint auth: **none by default — rely on Docker network isolation** (the
  broker sits on a private network only the fleet can reach). Leave a hook for an
  optional bearer token for hardening, off by default.

**Bootstrap (the one-time login):** a `login` command runs the device flow
(§1–2), prints the `verification_uri`/`user_code`, polls, and writes the
`refresh_token` to the store. Then `serve` runs the broker. Both share the same
container/deps. (Design decision for the builder: two subcommands of one binary,
e.g. `treesinger login` / `treesinger serve`.)

## Token store

Persists `{ refresh_token, access_token?, access_expires_at? }` on a volume.
Requirements, mirroring the proven pattern in chandlery's `tools/hytale-token`
(read it — same repo family, shell, but the exact semantics we want):
- **Single-writer** refresh via the mutex — two concurrent refreshes must not both
  spend the rotating refresh token.
- **Atomic write-back BEFORE handing a token out** — a crash after refresh must not
  strand a spent refresh token as the stored one.
- Cache the access token; refresh only within a skew window of expiry.
- Credentials never on argv; owner-only files.

## Refresh / mint logic

- `ensureAccessToken()`: if cached access token has > skew life, use it; else
  refresh the OAuth token (rotate, store-first), under the mutex.
- `mintSession(profileUuid)`: `ensureAccessToken()` → `POST /game-session/new` →
  return the pair.
- Cache the operator profile uuid from `get-profiles` (refresh occasionally).
- Treesinger does **not** manage servers' live sessions — they self-refresh. It
  has no per-server state.

## Proposed module layout

```
src/
  clock.ts        Clock, SystemClock, StoppedClock
  http.ts         Fetch type = (Request)=>Promise<Response>; RetryingFetch, LoggingFetch decorators
  store.ts        TokenStore iface; FileTokenStore; MemoryTokenStore
  mutex.ts        AsyncMutex
  oauth.ts        OAuthClient: deviceAuth, pollToken, refresh; ensureAccessToken (deps: fetch, clock, store, mutex, logger)
  sessions.ts     SessionClient: getProfiles, newSession, (refresh/terminate) (deps: fetch, clock, oauth)
  app.ts          handler(deps): (Request)=>Promise<Response>  — routes /v1/session, /health
  container.ts    container(): LazyMap factory wiring all of the above
  main.ts         CLI: `login` | `serve`; Bun.serve({ fetch: container().app })
test/
  fixtures/       recorded real request/response JSON for the fake upstream fetch
  *.test.ts       bun test
mise.toml  bunfig.toml  tsconfig.json  Dockerfile  .github/workflows/release.yml  README.md  LICENSE
```

## Testing

`bun test`, all in-memory: fake upstream `fetch` replaying **recorded real
responses** (capture the actual shapes; the spec above is the reference), plus
`StoppedClock`. Cover:
- device flow: pending → success; stores refresh token.
- `ensureAccessToken`: cache hit (no network); refresh when near expiry; **rotation
  stored before return**; **two concurrent calls don't double-spend** (mutex).
- `mintSession`: profiles → new session → pair returned.
- refresh-before-expiry decision is pure over the clock (advance + tick).
- error paths: `401` (re-auth needed), `403` session-limit.
- `app`: `/v1/session` happy path; `/health` reflects mint-readiness.

## Consumer contract (chandlery side — separate follow-up, not this repo)

The chandlery Hytale image already reads `HYTALE_SERVER_SESSION_TOKEN` /
`HYTALE_SERVER_IDENTITY_TOKEN` (the game's own vars) and `--session-token` /
`--identity-token`. The adapter change (in chandlery, tracked there): at boot,
**fetch a pair from Treesinger** (`POST /v1/session`) and pass them as
`--session-token`/`--identity-token`. Compose wires
`depends_on: { treesinger: { condition: service_healthy } }` for cold start; the
persistent broker covers restarts. Network isolation, no shared secret. **This
paragraph is context only — do not implement chandlery changes here.**

## CI/CD — GitHub Actions (required)

Default branch is **`trunk`**. On a version tag / release, publish **both**:

1. **Bun single-binary GitHub Release.** `bun build --compile` a standalone binary
   (see Bun's cross-compile `--target` matrix — e.g. `bun-linux-x64`,
   `bun-linux-arm64`, and the mac/win targets you choose), and attach each to the
   GH Release (`gh release create`/`upload` or `softprops/action-gh-release`).
2. **Docker image to GHCR.** Multi-stage: build with Bun, ship the compiled binary
   (or `bun run`) on a slim base. Push `ghcr.io/danielbodart/treesinger:<version>`
   + `:latest`. `packages: write`, `docker/login-action` + `build-push-action`.

Also a `ci` workflow: `bun install`, `bun test`, typecheck on push/PR.

## Open decisions for the builder

- `login` and `serve` as subcommands of one binary vs a tiny admin endpoint for the
  device flow. (Leaning: subcommands.)
- Profile selection when an account has multiple profiles (default to `owner`, or a
  `--owner-uuid`/config value).
- Whether `/v1/session` caches a just-minted pair briefly or always mints fresh
  (fresh is simplest; sessions are cheap and self-refresh after).
- Binary target matrix for releases.
- Optional bearer-token hardening on `/v1/session` (off by default; network is the
  boundary).
