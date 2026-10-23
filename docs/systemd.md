# Treesinger on systemd — design

> **Status:** design for the `systemd` branch. Nothing here is built yet except
> the mechanical refactor noted under [Refactors done](#refactors-done). Read
> this end to end before implementing.

Treesinger today is a Docker image: one container, a volume holding
`tokens.json`, reached by name over a compose network on TCP `:8080`. This
branch makes it a first-class **systemd service** — useful on any systemd host,
with a Nix flake (package + NixOS module + VM test) on top — while the Docker
image and its TCP listener stay exactly as they are.

Nothing about what Treesinger *does* changes: one refresh token, one writer,
mint-on-request, `POST /v1/session` and `GET /health` unchanged.

## Interfaces

### Listeners

The broker listens on **any combination** of a unix socket and a TCP port:

| Variable                   | Default                                    | Meaning |
|----------------------------|--------------------------------------------|---------|
| `TREESINGER_SOCKET`        | unset                                      | **New.** Listen on this unix socket path. |
| `TREESINGER_PORT`          | `8080` if no socket is set, else unset     | Listen on this TCP port. Setting it alongside a socket listens on both. |
| `TREESINGER_HOST`          | `0.0.0.0`                                  | **New.** Address the TCP listener binds (e.g. `127.0.0.1`). |
| `TREESINGER_STORE`, `_SKEW_MS`, `_OWNER_UUID`, `_BEARER_TOKEN` | unchanged | unchanged |

So: Docker (no socket set) behaves byte-for-byte as today, `:8080` on all
addresses. A systemd unit sets `TREESINGER_SOCKET` and, unless it also sets
`TREESINGER_PORT`, has no TCP listener at all — the socket's file permissions
are the whole access boundary, the native equivalent of compose network
isolation.

`Config` becomes:

```ts
interface Config {
    storePath: string;
    skewMs: number;
    /** TCP port, or undefined for no TCP listener. */
    port?: number;
    /** TCP bind address. */
    hostname: string;
    /** Unix socket path, or undefined for none. */
    socketPath?: string;
    ownerUuid?: string;
    bearerToken?: string;
}
```

`configFromEnv` resolves `port` as `TREESINGER_PORT` if set, else `8080` if no
`TREESINGER_SOCKET`, else `undefined`. (`test/support/harness.ts`'s
`baseConfig` gains `hostname`.)

### Socket semantics

`serve` (in `src/serve.ts`) does, for the socket:

1. If something already exists at the path **and it is a socket**, unlink it (a
   stale socket from a crash, or one left by `RuntimeDirectoryPreserve`). If it
   exists and is *not* a socket, refuse to start — never delete a user's file.
2. `Bun.serve({ unix: path, fetch: c.app })`.
3. `chmod 0660` the socket. Bun binds with the process umask (measured: `0755`
   under umask 022, which a non-owner cannot even `connect()` to), so the mode
   is set explicitly rather than left to umask. The socket's **group is the
   process's primary group** — that is the access-control group (see the
   module's `group`).

Then, once every listener is bound, readiness (below), then the log line
(`serving on unix:/run/treesinger/treesinger.sock` and/or `http://host:port`).

### Socket activation — investigated, not possible

Bun (1.3.13 / 1.3.14, measured under `systemd-socket-activate`) **cannot serve
on an inherited fd**:

- `Bun.serve` has no `fd` option.
- `Bun.listen({ fd: 3 })` throws `EINVAL: Bun does not support listening on a
  file descriptor`.
- `node:http` `server.listen({ fd: 3 })` is worse: it **silently ignores the fd
  and binds a random TCP port on `::`** — an open listener nobody asked for.

So there is no `treesinger.socket` unit and no `LISTEN_FDS` handling. The
service creates its socket itself under `RuntimeDirectory=treesinger`. (If Bun
gains fd listening, socket activation can be added without changing the
contract: same path, same group, same mode.) The code must **not** fall back
to `node:http` `listen({fd})` for the reason above.

### Readiness: `Type=notify`

With no socket unit, a client ordered `After=treesinger.service` could start
in the window between `exec` and `bind`, find no socket, and boot its game
unauthenticated. So the service is `Type=notify`: when `NOTIFY_SOCKET` is set,
after every listener is bound, `serve` runs `systemd-notify --ready` (Bun has
no `sd_notify` and cannot send to a unix datagram socket natively; spawning the
helper is the portable way). Because the notification comes from a child, the
unit needs `NotifyAccess=all`. Without `NOTIFY_SOCKET` (Docker, a shell)
nothing is spawned.

"Ready" means **listening**, not **logged in**: an unauthenticated broker is up
and answers `/health` with `503` and `/v1/session` with `401 not_logged_in`,
which clients already treat as "boot unauthenticated".

### `treesinger health`

Probes the socket if `TREESINGER_SOCKET` is set (Bun's `fetch(url, { unix })`),
else `http://127.0.0.1:<port>` (or `TREESINGER_HOST` when it is a specific
address). Exit `0` only on `200`, as today, so the Docker `HEALTHCHECK` is
unchanged.

### HTTP API

Unchanged. `POST /v1/session` → `{ sessionToken, identityToken, expiresAt }`;
`GET /health` → `200 {"status":"ok"}` / `503 {"status":"unauthenticated"}`.
Over the socket a client uses any host in the URL:

```sh
curl -fsS --unix-socket /run/treesinger/treesinger.sock -X POST http://treesinger/v1/session
```

### Login

`treesinger login` is unchanged: it writes `TREESINGER_STORE` atomically. The
running service reads the store on every mint (`ensureAccessToken` →
`store.read()`), so **logging in while the service runs needs no restart**.
The only requirement is that `login` runs as the service's user with the
service's `TREESINGER_STORE` — see the module's `treesinger-login` below.

## File layout

```
flake.nix  flake.lock
nix/
  node-modules.nix   fixed-output derivation: `bun install --frozen-lockfile` → node_modules
  package.nix        treesinger: bun build bundle + makeWrapper'd bin/treesinger
  module.nix         nixosModules.default (services.treesinger)
  test.nix           NixOS VM test
contrib/systemd/
  treesinger.service plain-systemd unit (no Nix)
  treesinger.sysusers sysusers.d: the treesinger user + group
docs/systemd.md      this file
src/serve.ts         serve + health (extracted from main.ts; gains listeners + notify)
src/config.ts        + socketPath, hostname; port optional
test/serve.test.ts   new: listeners, socket mode, stale-socket handling, health over socket
README.md            + "systemd" and "NixOS" sections
```

## Nix flake

Inputs: `nixpkgs` (`nixos-unstable`). Systems: `x86_64-linux`,
`aarch64-linux` (the VM test only on `x86_64-linux`).

| Output | What |
|---|---|
| `packages.<system>.default` / `.treesinger` | The broker. `bin/treesinger` runs the bundled JS with nixpkgs `bun`. |
| `packages.<system>.treesinger-env` | **Proposed** client helper, see [Hytale contract](#hytale-servers-via-chandlery). |
| `overlays.default` | `treesinger` (and `treesinger-env`) into `pkgs`. |
| `nixosModules.default` / `.treesinger` | `services.treesinger`. |
| `checks.<system>.package` | the package builds. |
| `checks.<system>.typecheck` | `tsc --noEmit` over the FOD `node_modules`. |
| `checks.<system>.test` | `bun test` (all in-memory; no network needed). |
| `checks.x86_64-linux.nixos` | the VM test. |
| `devShells.<system>.default` | `bun` (mise stays the primary dev tool). |

### How the package builds in the sandbox

- **`node_modules` as a fixed-output derivation**: `bun install
  --frozen-lockfile --ignore-scripts --no-progress` with network,
  `BUN_INSTALL_CACHE_DIR=$TMPDIR`, output pinned by `outputHash`
  (`sha256`, recursive). The deps (`@bodar/yadic` from npm.jsr.io via
  `.npmrc`, `typescript`, `@types/*`) are pure JS, so one hash serves every
  system. Bumping `bun.lock` means bumping the hash (`lib.fakeHash` →
  copy from the error), documented in the README.
- **Bundle, not `--compile`**: `bun build --target=bun --minify --define
  TREESINGER_VERSION="…" src/main.ts --outfile $out/lib/treesinger/treesinger.js`,
  then `makeWrapper ${bun}/bin/bun $out/bin/treesinger --add-flags
  $out/lib/treesinger/treesinger.js`. Rationale: `--compile` appends the JS to a
  copy of the bun executable, which `strip`/`patchelf` in the fixup phase can
  mangle; a bundle sidesteps that, needs no `node_modules` at runtime, and the
  runtime closure is the same nixpkgs `bun` either way. (Fallback if the bundle
  misbehaves: `--compile` with `dontStrip = true; dontPatchELF = true;`.)
- **Version**: `run.ts` derives `major.commit-count.build`. The flake does the
  same from what Nix knows: `"${major}.${toString self.revCount}.0"` for a
  clean git checkout, `"development"` for a dirty tree (no `revCount`), so the
  name never claims a release it is not. `major` is read from `package.json`.
- The **checks** reuse the FOD `node_modules` (copied, not symlinked, so bun's
  resolution sees a real tree) and run `bun run.ts check` / `bun run.ts test`.
  `run.ts check` now runs `tsc` under bun (`bun x --bun tsc`) so it needs no
  `node` — true in the sandbox, and on a mise host with no node pinned (it
  failed there before this branch).

## NixOS module: `services.treesinger`

| Option | Type | Default | Meaning |
|---|---|---|---|
| `enable` | bool | `false` | |
| `package` | package | `self.packages.${system}.default` | |
| `user` | str | `"treesinger"` | Runs as. The module creates it (system user) only when it is `"treesinger"`. |
| `group` | str | `"treesinger"` | Primary group, **and the group of the socket**. The module always creates it when it is `"treesinger"`. Clients join this group. |
| `stateDir` | path (str) | `"/var/lib/treesinger"` | Holds `tokens.json` (`TREESINGER_STORE = "${stateDir}/tokens.json"`). Default is managed by `StateDirectory=treesinger` (mode 0700); any other path must already exist, owned by `user` — the module adds it to `ReadWritePaths` but does not create or chown it (Dan's `/data/servers/treesinger` is owned by dan, and a tmpfiles rule chowning a pre-existing data dir is a footgun). |
| `socket` | null or str | `"/run/treesinger/treesinger.sock"` | `TREESINGER_SOCKET`; `null` for no socket. |
| `port` | null or port | `null` | `TREESINGER_PORT`; `null` = no TCP listener. |
| `host` | str | `"127.0.0.1"` | `TREESINGER_HOST` (only used when `port` is set). Loopback by default: on NixOS the socket is the interface; TCP is opt-in. |
| `openFirewall` | bool | `false` | Open `port` in the firewall. Asserts `port != null`. |
| `ownerUuid` | null or str | `null` | `TREESINGER_OWNER_UUID`. |
| `bearerTokenFile` | null or path | `null` | Read via `LoadCredential=` and exported as `TREESINGER_BEARER_TOKEN` by a tiny `ExecStart` wrapper, so the secret never lands in the Nix store or the unit file. |
| `environment` | attrsOf str | `{}` | Extra env (e.g. `TREESINGER_SKEW_MS`), merged over the above. |
| `environmentFile` | null or path | `null` | `EnvironmentFile=` for secrets the deployer manages. |

Example — Dan's server:

```nix
services.treesinger = {
  enable = true;
  user = "dan";                         # owns /data/servers/treesinger (1000:100)
  stateDir = "/data/servers/treesinger";  # tokens.json carries over from the container's /data
};
# group stays "treesinger"; the socket is /run/treesinger/treesinger.sock, 0660 dan:treesinger
```

The container wrote `/data/tokens.json` on that volume, which is
`/data/servers/treesinger/tokens.json` on the host — exactly the module's
`TREESINGER_STORE` — so migration is "stop the container, switch". No re-login.

### The unit the module renders

```ini
[Unit]
Description=Treesinger: Hytale server authentication broker
Wants=network-online.target
After=network-online.target

[Service]
Type=notify
NotifyAccess=all
ExecStart=<package>/bin/treesinger serve
Environment=TREESINGER_STORE=<stateDir>/tokens.json TREESINGER_SOCKET=<socket> …
User=<user>
Group=<group>
RuntimeDirectory=treesinger
RuntimeDirectoryMode=0750
RuntimeDirectoryPreserve=yes
StateDirectory=treesinger          # only when stateDir is the default
StateDirectoryMode=0700
ReadWritePaths=<stateDir>          # only when it is not
UMask=0077
Restart=on-failure
RestartSec=5s

# hardening
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes                    # mkDefault: a stateDir under /home needs it off
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6   # INET for HTTPS to Hypixel
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=no          # JavaScriptCore JITs
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
CapabilityBoundingSet=
AmbientCapabilities=
```

`RuntimeDirectoryPreserve=yes` keeps `/run/treesinger`'s **inode** across a
restart of the broker, so a client that bind-mounted the directory into its own
namespace keeps seeing the live socket (the stale one is unlinked and recreated
inside it — hence step 1 of [socket semantics](#socket-semantics)).

Every hardening line is verified in the VM test, not assumed: Bun reads
`/proc/self/*` and `/sys` CPU info at startup, so `ProcSubset=pid` and similar
are added only if the test passes with them.

### Why a fixed user, not `DynamicUser`

The brief prefers `DynamicUser` "unless stateDir ownership requires a fixed
user". Two things require one here:

1. **The socket group must be static** so client units can name it in
   `SupplementaryGroups=` (and Nix can add users to it) before Treesinger has
   ever run. systemd forbids a dynamic user whose name matches an existing
   static group ("if `User=` is specified and the static group with the name
   exists, then it is required that the static user with the name already
   exists" — `systemd.exec(5)`), so `DynamicUser` with `User=treesinger,
   Group=treesinger` is rejected outright.
2. **The refresh token is the one long-lived secret**, and a `stateDir`
   outside `/var/lib` (Dan's) cannot be a `StateDirectory=`, which is the only
   way a dynamic uid gets chowned onto persistent state.

So the module creates a static `treesinger` system user and group by default,
and confines it with the hardening above instead. (`DynamicUser` remains
possible for someone who names the dynamic user differently; it is not
offered as an option — see open questions.)

### `treesinger-login`

The module puts a `treesinger-login` command on `environment.systemPackages`.
It runs the one-time device login **as the service's user, with the service's
environment and filesystem view**, so the token lands where `serve` reads it
with the right owner and mode:

```sh
sudo treesinger-login
# → systemd-run --quiet --pty --wait --collect \
#     --uid=<user> --gid=<group> \
#     -p UMask=0077 -p ProtectSystem=strict -p ReadWritePaths=<stateDir> \
#     --setenv=TREESINGER_STORE=<stateDir>/tokens.json [--setenv=… extra env] \
#     <package>/bin/treesinger login
```

No restart afterwards (the broker reads the store per mint). The equivalent
without the helper, for a plain systemd host:

```sh
sudo -u treesinger TREESINGER_STORE=/var/lib/treesinger/tokens.json treesinger login
```

## Plain systemd (no Nix)

Shipped as `contrib/systemd/`:

- `treesinger.sysusers` → `/etc/sysusers.d/treesinger.conf`:
  `u treesinger - "Treesinger auth broker" /var/lib/treesinger` (creates the
  user and its same-named group).
- `treesinger.service` → `/etc/systemd/system/`: the unit above with the
  defaults filled in, `ExecStart=/usr/local/bin/treesinger serve` (the release
  binary), `StateDirectory=treesinger`.

There is no `treesinger.socket`, by necessity — see
[socket activation](#socket-activation--investigated-not-possible).

README gets a "Running under systemd" section: install the binary, the two
files, `systemd-sysusers`, `systemctl enable --now treesinger`,
`sudo -u treesinger … treesinger login`, and how a client joins the group.

## How each shared contract is met

### Treesinger's own (socket)

| Contract | Met by |
|---|---|
| `services.treesinger` listens on `/run/treesinger/treesinger.sock` | `socket` option default; `RuntimeDirectory=treesinger`. |
| group `treesinger`, mode `0660` | Socket created by the service with its primary group `group` (default `treesinger`), then `chmod 0660`. Directory `0750` `user:group`, so group members can traverse it. |
| HTTP API unchanged | No change to `src/app.ts`. |

### Hytale servers (via Chandlery)

The contract is `services.chandlery.servers.<n>.treesinger.socket =
"/run/treesinger/treesinger.sock"`; Chandlery's module implements it. What it
needs from Treesinger, and what to watch for:

- **Ordering**: `wants`/`after = [ "treesinger.service" ]` when the socket is
  set. `Type=notify` means *after* = socket bound.
- **Access**: `SupplementaryGroups = [ "treesinger" ]` (the static group).
- **Confinement**: bind the **directory**, not the socket file, and optional:
  `BindReadOnlyPaths = [ "-/run/treesinger" ]`. A bind of the socket file
  itself pins the old inode and goes stale the moment the broker restarts; the
  directory is stable thanks to `RuntimeDirectoryPreserve=yes`. The `-` keeps
  the game starting (unauthenticated) if the broker is absent. `connect()` to a
  socket works through a read-only bind (the read-only check does not apply to
  socket inodes).
- **Env injection — a caveat in the contract as written.** The contract says
  "an `ExecStartPre` mints the session into the env the server reads". An
  `ExecStartPre` **cannot** set environment for `ExecStart`: systemd reads
  `EnvironmentFile=` once, before the first command of the start job, so a file
  written by `ExecStartPre` is not seen by `ExecStart` of the same start. The
  working shapes are:
  1. **Mint inside `ExecStart`** — a wrapper that mints, exports
     `HYTALE_SERVER_SESSION_TOKEN` / `HYTALE_SERVER_IDENTITY_TOKEN`, then `exec`s
     the game (what `images/hytale/run` does today with `TREESINGER_URL`). This
     composes with Chandlery's notify wrapper (mint, then exec the notify
     wrapper). **Recommended.**
  2. A separate `chandlery-<n>-session.service` oneshot, `Before=` the game,
     writing `/run/chandlery/<n>/session.env` that the game reads with
     `EnvironmentFile=-…` (a different unit, so its file exists before the game
     unit's start job reads it).

  To make (1) a one-liner for Chandlery and for anyone on plain systemd,
  Treesinger's flake offers **`packages.treesinger-env`** (proposed): a
  `writeShellApplication` (`curl`, `jq`) —

  ```sh
  treesinger-env [--socket PATH | --url URL] -- COMMAND [ARGS…]
  ```

  — which `POST`s `/v1/session` (`-m 10`), exports the two variables on
  success, warns to stderr and continues without them on any failure (exactly
  the image's degradation), then `exec`s `COMMAND`. Tokens never touch argv. A
  shell `exec` rather than a Bun subcommand because Bun cannot `execve` — a Bun
  wrapper would have to stay resident as the game's parent, breaking the
  notify-wrapper's PID and the console FIFO's stdin. Chandlery is free to
  inline the same twenty lines instead of taking a flake input.

### Portical

Not applicable: Treesinger must never be forwarded to the internet. Nothing to
declare. (If someone does run it on TCP for remote Docker hosts, that is a LAN
port, still not a UPnP forward.)

### Tidewaiter

Not needed. Restarting Treesinger never disturbs a running game — servers
self-refresh their own sessions and only call the broker at boot — so the
ordinary `restartIfChanged` on `nixos-rebuild switch` is correct, and there are
no players to wait for. The module does not set `systemd.services.treesinger.
tidewaiter`, and the README says why.

## Test plan

### Bun (`bun test`, in-memory where possible)

- `test/config.test.ts` (new): `configFromEnv` — no vars → port 8080, host
  `0.0.0.0`, no socket; socket only → no port; socket + port → both; host
  honoured.
- `test/serve.test.ts` (new; real `Bun.serve` on a temp dir, no network):
  - socket listener answers `GET /health` (503 with an empty store) and
    `POST /v1/session` (401 `not_logged_in`);
  - socket mode is `0660` regardless of umask;
  - a stale socket at the path is replaced; a regular file at the path is
    refused and left intact;
  - socket + TCP both answer;
  - `health()` exit code over socket and over TCP;
  - `systemd-notify` is spawned only when `NOTIFY_SOCKET` is set (the spawn is
    an injected seam, so the test records the call rather than running it).
- Existing 26 tests stay green.

### Nix

- `nix build .#default`, `nix flake check` (package, typecheck, test, VM).
- `nix/test.nix` VM test, one node, `services.treesinger.enable` with the
  defaults plus a second variant with a fixed user and a custom `stateDir`:
  - `wait_for_unit("treesinger.service")` (proves `Type=notify` readiness).
  - Socket exists, `srw-rw----`, owner `treesinger`, group `treesinger`;
    `/run/treesinger` is `0750`.
  - As a user **in** `treesinger`: `curl --unix-socket … /health` → `503
    {"status":"unauthenticated"}`; `POST /v1/session` → `401
    {"error":"not_logged_in"}` (offline, so login cannot succeed — this is the
    unauthenticated error path).
  - As a user **not** in the group: connect fails (permission denied).
  - No TCP listener by default (`ss -ltn` shows nothing on 8080); with
    `port = 8080` it answers on `127.0.0.1:8080`.
  - Seed a `tokens.json` with an expired access token: `/health` → `503`
    (refresh fails offline → mapped error), proving the store path and
    permissions are wired.
  - `systemctl restart treesinger` → socket is back, same directory inode
    (`RuntimeDirectoryPreserve`), stale socket replaced.
  - `systemd-analyze security treesinger.service` exposure score logged (and
    asserted below a threshold, e.g. ≤ 2.5).
  - A confined client unit (`BindReadOnlyPaths=-/run/treesinger`,
    `SupplementaryGroups=treesinger`, `ProtectSystem=strict`) running
    `treesinger-env --socket … -- sh -c 'echo ${HYTALE_SERVER_SESSION_TOKEN:-none}'`
    gets `none` and a warning (degradation path), and does not fail.
  - `treesinger-login` exists on `PATH` (cannot be run offline).

### Docker

- `bun run.ts image` still builds; the image's `HEALTHCHECK` still works (no
  socket set → TCP probe, unchanged).

## Refactors done

Mechanical, behaviour unchanged, 26/26 tests green, typecheck clean:

- `serve` and `health` moved from `src/main.ts` to `src/serve.ts` (main.ts
  calls `process.exit` at top level, so nothing in it can be imported by a
  test; the listener logic that this branch adds needs tests).
- `run.ts check` runs `bun x --bun tsc --noEmit` instead of `bunx tsc` — `tsc`'s
  `#!/usr/bin/env node` shebang failed on a host whose mise pins no node, and
  there is no node in the Nix sandbox.
- `.gitignore`: `result`, `result-*`.

## Open questions

1. **The `ExecStartPre` env caveat** (above): the contract wording needs
   Chandlery to mint in `ExecStart`'s wrapper (or a session oneshot) instead.
   Whoever implements Chandlery's side should decide; `treesinger-env` is
   offered either way. Does Treesinger ship `treesinger-env`, or does
   Chandlery inline it?
2. **Package `pkgs`**: the module's default `package` is the flake's own
   (pinned nixpkgs `bun`, matching the FOD hash). Building against the
   consumer's `pkgs` via the overlay instead would follow their `bun` but risks
   the `node_modules` hash drifting with a bun version that writes the tree
   differently. Proposed: own pin by default, overlay available.
3. **`DynamicUser`**: not offered (see why). Worth an opt-in
   `dynamicUser = true` that names the dynamic user `treesinger-broker` and keeps
   the static `treesinger` group as a supplementary group + `chgrp` of the socket?
   Adds a code path (`chown` by gid) for little gain; proposed no.
4. **Upstream URL overrides for testing**: the Hytale endpoints are constants,
   so the VM test can only exercise unauthenticated paths. Adding
   `TREESINGER_OAUTH_BASE` etc. would let a VM test run a full mint against a
   fake upstream (the fake already exists in `test/support`). Out of scope
   unless wanted.
5. **Bun version skew**: mise pins 1.3.14; nixpkgs `nixos-unstable` ships
   1.4.x. `bun.lock` (lockfileVersion 1) reads under both, but the flake build
   and the Docker/mise build will run different bun minors.
6. **arm64**: the flake lists `aarch64-linux` for the package; unverified on
   real hardware (no builder here).
