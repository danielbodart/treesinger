# Treesinger on systemd — design

> **Status:** built on the `systemd` branch: the listeners, readiness and
> `health` in the app, the flake (package, `treesinger-env`, module, checks),
> `contrib/`, the README sections, and a VM test of the module and of the plain
> unit (`nix flake check` green on x86_64-linux). Where building found reality
> different from the design, this document has been corrected.

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

1. If something already exists at the path **and it is a socket nobody answers
   on**, unlink it (a stale socket from a crash or a SIGKILL — Bun's default
   SIGTERM exit leaves one too — kept by `RuntimeDirectoryPreserve`). Bun's
   bind does not do this itself: it fails `EADDRINUSE`. A socket that *does*
   answer (probed with a `connect()`) is another live broker, almost certainly
   on the same store, which the single-writer design forbids — refuse to start
   rather than steal it. Anything that is *not* a socket — refuse to start,
   never delete a user's file.
2. `Bun.serve({ unix: path, fetch: c.app })`.
3. `chmod 0660` the socket. Bun binds with the process umask (measured: `0755`
   under umask 022, which a non-owner cannot even `connect()` to), so the mode
   is set explicitly rather than left to umask. The socket's **group is the
   process's primary group** — that is the access-control group (see the
   module's `group`).

Then, once every listener is bound, readiness (below), then the log line
(`serving on unix:/run/treesinger/treesinger.sock` and/or `http://host:port`).
If a later listener or the notify fails, the ones already open are closed, so
a failed start leaves no socket behind.

On `SIGTERM`/`SIGINT` `serve` stops its listeners — `server.stop()` unlinks
the socket — and exits `0`. So a cleanly stopped broker leaves no socket, and
a client sees `ENOENT` rather than `ECONNREFUSED` (it treats both as "boot
unauthenticated").

In code: `listen(config, fetch)` opens the listeners, `start(c, notify, env)`
adds readiness and the log line (both tested directly), and `serve(c)` is
`start` plus the signal handlers and a promise that never resolves.

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
nothing is spawned. `systemd-notify` is looked up on `PATH` — the package does
not pull systemd into its closure; a NixOS unit's default `path` has it, and a
plain systemd host has it in `/usr/bin`. If it is missing or fails, `serve`
closes its listeners and exits non-zero rather than sit unready until
`TimeoutStartSec`.

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
  treesinger-env.nix contrib/treesinger-env with its curl and jq
  module.nix         services.treesinger (nixosModules.default adds the flake's package)
  test.nix           NixOS VM test: the module, twice, and the contrib unit
contrib/
  treesinger-env     client helper: mint a session, exec the game with it
  systemd/treesinger.service  plain-systemd unit (no Nix)
  systemd/treesinger.sysusers sysusers.d: the treesinger user + group
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
| `packages.<system>.treesinger-env` | The client helper, see [Hytale servers](#hytale-servers-via-chandlery). |
| `overlays.default` | `treesinger` (and `treesinger-env`) into `pkgs`. |
| `nixosModules.default` / `.treesinger` | `services.treesinger`, its `package` defaulting (`mkDefault`) to this flake's. `nix/module.nix` alone defaults to `pkgs.treesinger`, from the overlay. |
| `checks.<system>.package` | the package builds. |
| `checks.<system>.typecheck` | `tsc --noEmit` over the FOD `node_modules`. |
| `checks.<system>.test` | `bun test` (all in-memory; no network needed). |
| `checks.<system>.treesinger-env` | the helper builds (and passes `writeShellApplication`'s shellcheck). |
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
- **Bundle, not `--compile`**: `bun build --target=bun --minify
  --sourcemap=linked --define TREESINGER_VERSION="…" --entry-naming=treesinger.[ext]
  src/main.ts --outdir dist`, both files to `$out/lib/treesinger/`, then `makeWrapper ${bun}/bin/bun $out/bin/treesinger --add-flags
  "--no-env-file --config=/dev/null $out/lib/treesinger/treesinger.js"`. The two
  flags matter: `bun` otherwise loads a `.env` (measured: it set
  `TREESINGER_PORT`) and a `bunfig.toml` from whatever directory `treesinger`
  is run in. Rationale for the bundle: `--compile` appends the JS to a
  copy of the bun executable, which `strip`/`patchelf` in the fixup phase can
  mangle; a bundle sidesteps that, needs no `node_modules` at runtime, and the
  runtime closure is the same nixpkgs `bun` either way. (Fallback if the bundle
  misbehaves: `--compile` with `dontStrip = true; dontPatchELF = true;`.)
  The **sourcemap is needed**: without one, bun prints a logged error's source
  context from the bundle — a screenful of minified yadic in the journal for
  every failed mint (found in the VM test). `--outdir`, because with a
  sourcemap bun ignores `--outfile`'s directory and writes beside the entry,
  into `src/`. The released `--compile` binary (`run.ts build`) now also has
  `--no-compile-autoload-dotenv --no-compile-autoload-bunfig`: it too read a
  `.env` from its working directory (measured: it moved the port).
- **Version**: `run.ts` derives `major.commit-count.build`. The flake does the
  same from what Nix knows: `"${major}.${toString self.revCount}.0"` for a
  clean git checkout; a tarball fetch (`github:` input) has a revision but no
  count, so `"${major}.0.0-${shortRev}"`; a dirty tree has neither, so
  `"development"` — the name never claims a release it is not. `major` is read
  from `package.json`.
- The package's source is only what the bundle needs (`src/`, `package.json`,
  `bun.lock`, `bunfig.toml`, `tsconfig.json`), so editing a test does not
  rebuild it. The **checks** are the same derivation with `run.ts` and `test/`
  added, reusing the FOD `node_modules` (copied, not symlinked, so bun's
  resolution sees a real tree) and running `bun run.ts check` / `bun run.ts
  test` in place of the bundle.
  `run.ts check` runs `bun node_modules/typescript/bin/tsc --noEmit`: both
  `bunx tsc` and `bun x --bun tsc` exec tsc's `#!/usr/bin/env node` shebang,
  which needs a node (absent on a mise host pinning only bun) and
  `/usr/bin/env` (absent in the sandbox, where it failed with no output).
- **The `node_modules` hash does not depend on the bun version** — measured:
  `--rebuild` of the FOD with nixos-unstable's bun 1.4.2 and with
  nixos-26.05's 1.3.13 (via `--override-input nixpkgs`) both match, and the
  checks pass on both.

## NixOS module: `services.treesinger`

| Option | Type | Default | Meaning |
|---|---|---|---|
| `enable` | bool | `false` | |
| `package` | package | the flake's (via `nixosModules.default`), else `pkgs.treesinger` | |
| `user` | str | `"treesinger"` | Runs as. The module creates it (system user) only when it is `"treesinger"`. |
| `group` | str | `"treesinger"` | Primary group, **and the group of the socket**. The module always creates it when it is `"treesinger"`. Clients join this group. |
| `stateDir` | path (str) | `"/var/lib/treesinger"` | Holds `tokens.json` (`TREESINGER_STORE = "${stateDir}/tokens.json"`). Default is managed by `StateDirectory=treesinger` (mode 0700); any other path must already exist, owned by `user` — the module adds it to `ReadWritePaths` but does not create or chown it (Dan's `/data/servers/treesinger` is owned by dan, and a tmpfiles rule chowning a pre-existing data dir is a footgun). |
| `socket` | null or str | `"/run/treesinger/treesinger.sock"` | `TREESINGER_SOCKET`; `null` for no socket. Another directory must exist and be writable by `user`; it is added to `ReadWritePaths`. Asserted: `socket` or `port` (or both). |
| `port` | null or port | `null` | `TREESINGER_PORT`; `null` = no TCP listener. |
| `host` | str | `"127.0.0.1"` | `TREESINGER_HOST` (only used when `port` is set). Loopback by default: on NixOS the socket is the interface; TCP is opt-in. |
| `openFirewall` | bool | `false` | Open `port` in the firewall. Asserts `port != null`. |
| `ownerUuid` | null or str | `null` | `TREESINGER_OWNER_UUID`. |
| `bearerTokenFile` | null or absolute path string | `null` | A string, not a path literal, which interpolation would copy into the store. Read via `LoadCredential=` and exported as `TREESINGER_BEARER_TOKEN` by a tiny `ExecStart` wrapper, so the secret never lands in the Nix store or the unit file. |
| `environment` | attrsOf str | `{}` | Extra env (e.g. `TREESINGER_SKEW_MS`), merged over the above. |
| `environmentFile` | null or absolute path string | `null` | `EnvironmentFile=` for secrets the deployer manages. |

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
ExecStart=<treesinger-serve>          # exports the bearer credential if any, then execs treesinger serve
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
ProtectHome=yes                    # no when stateDir is under /home or /root
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
PrivateUsers=yes
RemoveIPC=yes                      # only for the module's own user: for dan it would remove dan's IPC
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6   # INET for HTTPS to Hypixel
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=no          # JavaScriptCore JITs
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged       # not @resources: bun 1.3 sched_setscheduler()s its threads
CapabilityBoundingSet=
AmbientCapabilities=
LoadCredential=bearer-token:<bearerTokenFile>   # only when set
EnvironmentFile=<environmentFile>               # only when set
```

`network-online.target` (`Wants=` and `After=`): the broker binds its socket
without the network, but its clients mint their sessions the moment it is
ready, once, at boot, and a mint fails if Hypixel cannot be reached yet.
Waiting here makes every client ordered after the broker wait too.

`RuntimeDirectoryPreserve=yes` keeps `/run/treesinger`'s **inode** across a
restart of the broker, so a client that bind-mounted the directory into its own
namespace keeps seeing the live socket (the stale one is unlinked and recreated
inside it — hence step 1 of [socket semantics](#socket-semantics)).

Every hardening line is verified in the VM test, not assumed: the broker
starts, binds, notifies, reads and writes its store, and reaches the network
stack with all of them, `ProcSubset=pid` and `PrivateUsers=yes` included.
`~@resources` is **not** there: on nixos-26.05's bun 1.3.13 the broker died
of `SIGSYS` at start — it calls `sched_setscheduler` (in `@resources`) on its
threads; unstable's 1.4.2 does not, so only the 26.05 run caught it.
`systemd-analyze security` scores it **1.4** ("OK"); the test asserts ≤ 1.5. What is left exposed is what it needs: `AF_INET` for
Hypixel, `AF_UNIX`, W+X memory for the JIT, `NotifyAccess=all`.

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
# → systemd-run --quiet --wait --collect --pty --pipe --unit=treesinger-login \
#     -p User=<user> -p Group=<group> -p UMask=0077 … every sandbox property of the service … \
#     [-p EnvironmentFile=…] --setenv=TREESINGER_STORE=<stateDir>/tokens.json [--setenv=… extra env] \
#     <package>/bin/treesinger login
```

The properties are rendered from the same attrset as the service's, so the
two cannot drift (`StateDirectory=` included, which creates the default
directory if the service has never run). `--pty --pipe` is a terminal when
run from one and plain pipes otherwise. The VM test runs it offline: it gets
as far as the network, as the broker's user, and writes nothing.

No restart afterwards (the broker reads the store per mint). The equivalent
without the helper, for a plain systemd host:

```sh
sudo -u treesinger TREESINGER_STORE=/var/lib/treesinger/tokens.json treesinger login
```

## Plain systemd (no Nix)

Shipped as `contrib/systemd/`:

- `treesinger.sysusers` → `/etc/sysusers.d/treesinger.conf`:
  `u treesinger - "Treesinger authentication broker" /var/lib/treesinger`
  (creates the user and its same-named group).
- `treesinger.service` → `/etc/systemd/system/`: the unit above with the
  defaults filled in, `ExecStart=/usr/local/bin/treesinger serve` (the release
  binary), `StateDirectory=treesinger`, TCP and an `EnvironmentFile` as
  commented-out lines.
- `treesinger-env` → `/usr/local/bin/` (needs `curl` and `jq`).

The VM test's third machine installs exactly these two unit files (only
`ExecStart` substituted), runs `systemd-sysusers` on the sysusers file, and
checks the socket and `/health`: it scores the same 1.4.

There is no `treesinger.socket`, by necessity — see
[socket activation](#socket-activation--investigated-not-possible).

The README's "systemd" section has the install steps, the login, a client
unit, and the NixOS equivalent.

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
  This holds under `PrivateUsers=yes` too (measured in the VM test): the group
  is unmapped inside the client's user namespace, shown as `nogroup`, but the
  socket's permission check is on the real gid, so `connect()` succeeds.
  Chandlery's open question on `PrivateUsers` and the treesinger group is
  answered: no `PrivateUsers=identity` needed for this.
- **Confinement**: bind the **directory**, not the socket file, and optional:
  `BindReadOnlyPaths = [ "-/run/treesinger" ]`. A bind of the socket file
  itself pins the old inode and goes stale the moment the broker restarts; the
  directory is stable thanks to `RuntimeDirectoryPreserve=yes`. The `-` keeps
  the game starting (unauthenticated) if the broker is absent. `connect()` to a
  socket works through a read-only bind (the read-only check does not apply to
  socket inodes).
- **Ordering on the network.** A client mints its session once, at boot, and
  nothing retries it: a mint that fails because Hypixel is not yet reachable
  leaves the server unauthenticated until its next restart. So the broker
  itself is `Wants=`/`After=network-online.target`, and a client ordered
  `After=treesinger.service` waits for the network through it (as the
  containers did, behind `docker.service`). A client that reaches the broker
  some other way (TCP) needs `network-online.target` itself.
- **Env injection.** The contract says "an `ExecStartPre` mints the session
  into the env the server reads". That works: systemd reads `EnvironmentFile=`
  again before each command, once the previous one has ended ("so you can
  generate these files in one unit state, and read it with this option in the
  next", systemd.exec(5); checked on systemd 260 with `systemd-run`). Two
  shapes, both against this broker:
  1. **Mint inside `ExecStart`** — a wrapper that mints, exports
     `HYTALE_SERVER_SESSION_TOKEN` / `HYTALE_SERVER_IDENTITY_TOKEN`, then `exec`s
     the game (what `images/hytale/run` does today with `TREESINGER_URL`). This
     composes with Chandlery's notify wrapper (mint, then exec the notify
     wrapper). **Recommended**, because the tokens only ever live in the
     game's environment, never in a file.
  2. **Mint in `ExecStartPre`** — `curl --unix-socket
     /run/treesinger/treesinger.sock -X POST http://treesinger/v1/session`,
     writing the two variables to a file in the unit's `RuntimeDirectory`
     (mode 0600, e.g. `/run/chandlery/<n>/session.env`), which the game reads
     with `EnvironmentFile=-/run/chandlery/<n>/session.env`. On a failed mint
     the script must remove (or truncate) the file, so a stale session from
     the last start is not reused, and exit 0, so the game still boots
     unauthenticated.

  To make (1) a one-liner for anyone on plain systemd, Treesinger ships
  **`contrib/treesinger-env`**, packaged as `packages.treesinger-env` (a
  `writeShellApplication` with `curl` and `jq`, so shellchecked at build) —

  ```sh
  treesinger-env [--socket PATH | --url URL] -- COMMAND [ARGS…]
  ```

  — which `POST`s `/v1/session` (`-m 10`), exports the two variables on
  success, warns to stderr and continues without them on any failure (exactly
  the image's degradation), then `exec`s `COMMAND`. Tokens never touch argv. A
  shell `exec` rather than a Bun subcommand because Bun cannot `execve` — a Bun
  wrapper would have to stay resident as the game's parent, breaking the
  notify-wrapper's PID and the console FIFO's stdin. A bearer token, if
  `TREESINGER_BEARER_TOKEN` is set, goes to curl on stdin (`-H @-`), not argv,
  and is unset before the `exec`.

  Chandlery's own design (its `docs/nix.md` on its `nix` branch) inlines the
  mint in its run script instead: its `images/hytale/run` learns
  `TREESINGER_SOCKET` and makes the same POST with `curl --unix-socket`, which
  is shape 1. Either works against this broker; `treesinger-env` is for
  servers that are not Chandlery's.

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
    `POST /v1/session` (401 `not_logged_in`); no TCP listener with a socket alone;
  - socket mode is `0660` regardless of umask;
  - a stale socket (left by a SIGKILLed child broker) is replaced; a live one
    is refused and keeps answering; a regular file at the path is refused and
    left intact; stopping removes the socket;
  - socket + TCP both answer; a TCP bind failure closes the socket already open;
  - `health()` exit code over socket and over TCP, and with nothing listening;
  - readiness is notified once, after the socket exists (an injected `Notify`);
    a failed notify closes the listeners; `systemdNotify` does nothing without
    `NOTIFY_SOCKET` and rejects when it cannot reach it.
- Existing 26 tests stay green: **48/48** now, locally and in the sandbox.
- Also run by hand: `systemd-run --user -p Type=notify -p NotifyAccess=all`
  with `TREESINGER_SOCKET` (the packaged binary and `bun src/main.ts`): the unit
  goes `active` only once listening, the socket is `srw-rw----`, `/health`
  503, `/v1/session` 401, and `systemctl stop` removes the socket.

### Nix (as built)

`nix flake check` runs the package, typecheck, test, `treesinger-env` and VM
checks. `nix/test.nix` boots three machines, offline:

- **`defaults`** — `services.treesinger.enable` from `nixosModules.default`:
  - `wait_for_unit` returns with the socket already there (`Type=notify`);
  - socket `srw-rw---- treesinger treesinger`, `/run/treesinger` `750`,
    `/var/lib/treesinger` `700`;
  - a member gets `503 {"status":"unauthenticated"}` from `/health` and
    `401 {"error":"not_logged_in"}` from `/v1/session`; `treesinger health`
    exits non-zero; a non-member cannot connect;
  - no TCP listener;
  - a confined client (user `bob`, `SupplementaryGroups=treesinger`,
    `TemporaryFileSystem=/run`, `BindReadOnlyPaths=-/run/treesinger`) running
    `treesinger-env` gets the broker's 401 (so the socket was reachable),
    warns and runs unauthenticated; its `/run` holds only `treesinger`;
  - `systemctl restart treesinger` keeps `/run/treesinger`'s inode, and the
    confined client (via `nsenter`) reaches the new socket;
  - a client with `PrivateUsers=yes` and the supplementary group connects;
  - stop removes the socket, and `treesinger-env` then degrades;
  - against a fake broker, `treesinger-env` passes the session on;
  - `treesinger-login` runs offline as far as the network, writes nothing;
  - `systemd-analyze security`: 1.4, asserted ≤ 1.5.
- **`custom`** — `user = "gamer"` (an existing normal user),
  `stateDir = "/data/servers/treesinger"` (made by tmpfiles, as a deployer
  would), `port = 8080`, `bearerTokenFile`, extra `environment`:
  - socket `srw-rw---- gamer treesinger`;
  - with a seeded `tokens.json` holding a valid access token, `/health` is
    `200` over the socket and over `127.0.0.1:8080` — the store path and its
    permissions are wired — and nothing listens on 8080 beyond loopback;
  - `/v1/session` without the bearer is `401 unauthorized`, with it gets past
    the gate (`500`, Hypixel being absent); the secret is not in the unit's
    environment, `TREESINGER_SKEW_MS` is.
- **`plain`** — no module: `contrib/systemd`'s unit and sysusers file, see
  [plain systemd](#plain-systemd-no-nix).

Also run against nix-config's nixos-26.05 pin with `--override-input nixpkgs`
(bun 1.3.13): green, after it caught the `@resources` crash above.

### Docker

- `bun run.ts image` still builds; the image's `HEALTHCHECK` still works (no
  socket set → TCP probe, unchanged).

## Refactors done

Mechanical, behaviour unchanged, 26/26 tests green, typecheck clean:

- `serve` and `health` moved from `src/main.ts` to `src/serve.ts` (main.ts
  calls `process.exit` at top level, so nothing in it can be imported by a
  test; the listener logic that this branch adds needs tests).
- `run.ts check` runs tsc's script with bun instead of `bunx tsc` — `tsc`'s
  `#!/usr/bin/env node` shebang failed on a host whose mise pins no node, and
  there is neither node nor `/usr/bin/env` in the Nix sandbox (see
  [the build](#how-the-package-builds-in-the-sandbox)).
- `.gitignore`: `result`, `result-*`.

Later, found while building:

- The Nix bundle carries a linked sourcemap (see
  [the build](#how-the-package-builds-in-the-sandbox)).
- `run.ts build` compiles with `--no-compile-autoload-dotenv
  --no-compile-autoload-bunfig`. The Dockerfile's own `bun build --compile`
  does not yet: inside the image the working directory is fixed, so it
  matters less, and the image was not rebuilt here.

## Open questions

1. ~~Does Treesinger ship `treesinger-env`?~~ **Yes**, as `contrib/` and a
   flake package; Chandlery inlines its own in `images/hytale/run`.
2. ~~Package `pkgs`~~: **`nixosModules.default` defaults (`mkDefault`) to the
   flake's own package**; `nix/module.nix` alone, or the overlay, uses the
   consumer's `pkgs`. Both pass the checks (1.3.13 and 1.4.2 give the same
   `node_modules` hash), so `inputs.nixpkgs.follows` is safe today.
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
   1.4.2, nixos-26.05 1.3.13. `bun.lock` (lockfileVersion 1) reads under all
   of them and the tests pass on 1.3.13, 1.3.14 and 1.4.2, but the flake build
   and the Docker/mise build will run different bun minors.
6. **arm64**: the flake lists `aarch64-linux` for the package; it evaluates
   (`nix flake check --all-systems --no-build`) but is unbuilt (no builder here).
