# Treesinger on systemd — design

> **Status:** built on the `systemd` branch: the listeners, readiness and
> `health` in the app, the flake (package, module, checks), `contrib/`, the
> README sections, and a VM test of the module and of the plain units (`nix
> flake check` green on x86_64-linux). Where building found reality different
> from the design, this document has been corrected.
>
> **Since then, the broker has been rewritten in Go**, which changes the parts
> of this design that were about Bun. They are kept below as the record of why
> it was built that way; what is true now:
>
> - **Socket activation is built.** systemd holds the sockets
>   (`treesinger.socket`, `treesinger-admin.socket`) and the broker serves the
>   fds it is passed (`LISTEN_FDS`, read by hand with `net.FileListener`, no
>   go-systemd). The broker no longer creates, chmods or unlinks a socket, and
>   `TREESINGER_SOCKET` is gone. See [socket activation](#socket-activation).
> - **Login goes through the broker**, over a root-only admin socket:
>   `sudo treesinger login` replaces the module's `treesinger-login` wrapper.
>   See [login](#login).
> - **`treesinger exec`** replaces the `treesinger-env` shell script: Go can
>   `execve`, which Bun could not. See [env injection](#hytale-servers-via-chandlery).
> - **The bearer token** may be a systemd credential
>   (`$CREDENTIALS_DIRECTORY/bearer-token`), so no wrapper script exports it.
>   There are no wrapper scripts left at all.
> - **Readiness** is a datagram the broker sends to `NOTIFY_SOCKET` itself, not
>   a `systemd-notify` child, so the unit has `NotifyAccess=main`, and nothing
>   needs `systemd-notify` on `PATH`.
> - **No JIT**, so the sandbox has `MemoryDenyWriteExecute=yes`.
>   `~@resources` stays out of the system call filter: the Go runtime raises
>   its open-file limit with `setrlimit` as it starts.
> - **The package** is `buildGoModule` with no vendored modules; there is no
>   `node_modules` derivation, bundle or wrapper, and no `.env`/`bunfig.toml`
>   for a binary to pick up. Its check phase is the tests.
> - **The code** is `main.go` and `internal/broker/` (`serve.go` for the
>   listeners, readiness and `health`), with the tests beside it; `./run`
>   replaces `run.ts`.
> - **CI** runs the VM test and `nix flake check` in a `nix` job (with KVM),
>   which does not gate publishing.

Treesinger today is a Docker image: one container, a volume holding
`tokens.json`, reached by name over a compose network on TCP `:8080`. This
branch makes it a first-class **systemd service** — useful on any systemd host,
with a Nix flake (package + NixOS module + VM test) on top — while the Docker
image and its TCP listener stay exactly as they are.

Nothing about what Treesinger *does* changes: one refresh token, one writer,
mint-on-request, `POST /v1/session` and `GET /health` unchanged.

## Interfaces

### Listeners

Where the broker listens depends on whether systemd passed it any:

- **Socket-activated** (`LISTEN_PID` is the broker's pid and `LISTEN_FDS` > 0):
  it serves **only** the passed fds, from fd 3, named by `LISTEN_FDNAMES`. An
  fd named `admin` serves the [admin handler](#login); every other fd, unix or
  TCP, serves the app (`POST /v1/session`, `GET /health`). The three variables
  are unset once read, so nothing the broker might start inherits them.
  `TREESINGER_PORT` or `TREESINGER_HOST` set as well is a startup error: the
  listeners come from the socket units then, and a silently ignored port is a
  misconfiguration nobody would notice.
- **Otherwise** (Docker, a shell): TCP only, on
  `TREESINGER_HOST:TREESINGER_PORT` (`0.0.0.0:8080` by default), byte-for-byte
  as before. The admin handler is **never** served on a listener the broker
  binds itself, nor on TCP.

| Variable                   | Default      | Meaning |
|----------------------------|--------------|---------|
| `TREESINGER_PORT`          | `8080`       | TCP port, when not socket-activated. |
| `TREESINGER_HOST`          | `0.0.0.0`    | TCP bind address, when not socket-activated. |
| `TREESINGER_BEARER_TOKEN`  | `$CREDENTIALS_DIRECTORY/bearer-token`, else unset | The shared secret for `/v1/session`, for `serve` and `exec`; the credential file's trailing newline is trimmed. |
| `TREESINGER_STORE`, `_SKEW_MS`, `_OWNER_UUID` | unchanged | unchanged |

`TREESINGER_SOCKET` is gone: the broker never creates a unix socket itself.
Set, it is refused at startup, so a unit from an earlier release cannot run
the new binary on TCP without its sockets (docs/systemd.md, Upgrade).
Under systemd, the socket's file permissions are the whole access boundary,
the native equivalent of compose network isolation; there is no TCP listener
unless the socket unit has one.

### Socket activation

Two socket units, both `WantedBy=sockets.target`, are what is enabled;
`treesinger.service` has no `WantedBy` of its own (its `[Install]` has
`Also=` the sockets, so `systemctl enable treesinger` enables them) and starts
on the first connection to either:

```ini
# treesinger.socket
[Socket]
ListenStream=/run/treesinger/treesinger.sock
SocketGroup=treesinger
SocketMode=0660
DirectoryMode=0755
#ListenStream=127.0.0.1:8080        # TCP: the module's port/host, or a drop-in

# treesinger-admin.socket
[Socket]
ListenStream=/run/treesinger-admin.sock
SocketMode=0600
FileDescriptorName=admin
Service=treesinger.service
```

The service `Requires=` and is `After=` both. What this buys over the broker
making its own socket (the Bun-era design below):

- **No window.** The socket exists from `sockets.target`, early in boot, and a
  connection made before the broker is up, or while it restarts, waits in the
  backlog instead of failing with `ENOENT`/`ECONNREFUSED`. The broker still
  sends `READY=1` once serving, so `After=treesinger.service` keeps meaning
  "serving" for clients that order on it.
- **A stable inode.** systemd keeps the socket across restarts of the broker,
  so a client may bind the socket file itself into its namespace, not just the
  directory. Chandlery binds `/run/treesinger` and expects `treesinger.sock` in
  it, which still holds: the directory now only ever holds that socket.
- **No socket code.** No stale-socket probe and unlink, no `chmod`, no group
  handling, no cleanup on exit, and no `RuntimeDirectory` (the broker writes
  nothing under `/run`). The owner is root, the group `treesinger`, the mode
  `0660`; the directory is `0755 root:root`, which only lets a client traverse
  it.
- **The admin socket is a separate unit**, root only (`0600`), outside
  `/run/treesinger` on purpose: that directory is bind-mounted into confined
  game servers, and nothing in one should be able to start a login. Its fd is
  named `admin`, which is how the broker tells it apart; the client socket's
  fds keep systemd's default name (the unit's).

The fds are parsed by hand (`os.NewFile` + `net.FileListener`); go-systemd
would be the module's first dependency for a dozen lines.

### Socket activation in Bun — investigated, not possible

(The record of why the Bun broker made its own socket.) Bun (1.3.13 / 1.3.14,
measured under `systemd-socket-activate`) **could not serve on an inherited
fd**: `Bun.serve` has no `fd` option, `Bun.listen({ fd: 3 })` throws `EINVAL`,
and `node:http` `server.listen({ fd: 3 })` silently ignored the fd and bound a
random TCP port on `::`. So the Bun broker created its socket under
`RuntimeDirectory=treesinger` with `RuntimeDirectoryPreserve=yes`, unlinked a
stale one on start (refusing a live one, or anything not a socket), `chmod`ed
it `0660` (Bun bound with the umask), and removed it on `SIGTERM`. Go serves
an inherited fd with `net.FileListener`, so all of that is gone.

### Readiness: `Type=notify`

Once serving, the broker sends `READY=1` to `NOTIFY_SOCKET` itself
(`NotifyAccess=main`). With socket activation a client no longer needs it to
find the socket, but ordering `After=treesinger.service` still means the
broker is serving and, through the broker's own `After=network-online.target`,
that Hypixel can be reached. Without `NOTIFY_SOCKET` (Docker, a shell) nothing
is sent. (The Bun broker had to spawn `systemd-notify --ready`, hence
`NotifyAccess=all` then.)

"Ready" means **serving**, not **logged in**: an unauthenticated broker is up
and answers `/health` with `503` and `/v1/session` with `401 not_logged_in`,
which clients already treat as "boot unauthenticated".

### `treesinger health`

`treesinger health [--socket PATH | --url URL]`: by default
`http://127.0.0.1:<TREESINGER_PORT or 8080>/health`, the Docker `HEALTHCHECK`,
unchanged; `--socket` probes over a unix socket. Exit `0` only on `200`.

### HTTP API

Unchanged on the client socket and TCP. `POST /v1/session` →
`{ sessionToken, identityToken, expiresAt }`; `GET /health` →
`200 {"status":"ok"}` / `503 {"status":"unauthenticated"}`. Over the socket a
client uses any host in the URL:

```sh
curl -fsS --unix-socket /run/treesinger/treesinger.sock -X POST http://treesinger/v1/session
```

### Login

The running service reads the store on every mint, so **logging in while the
service runs needs no restart**. The question is only who writes the store,
and as whom.

On the admin socket the broker serves `POST /v1/login`: it runs the OAuth
device flow itself, through the same `OAuthClient` that serves mints, so its
cached and refreshing token state stays coherent and the store is written the
way a refresh writes it (the broker's user, its `UMask`, its sandbox). The
response is newline-delimited JSON, flushed per line: first
`{"verification_uri_complete", "verification_uri", "user_code"}`, finally
`{"ok":true}` or `{"error":"..."}`. One login at a time: a second concurrent
`POST` is `409`.

`treesinger login [--socket PATH]` is its client, printing the same text as a
local login. Without `--socket` it uses `/run/treesinger-admin.sock` if that
exists, and otherwise runs the device flow locally against
`TREESINGER_STORE` (Docker); it says which. That local login is a second
writer beside a broker running on the same store, so it is only safe for a
first login or with the broker stopped (a refresh in flight could put the old
tokens back, and that broker would keep the old account's profiles):
[docker.md](docker.md#login) says to stop the container for any later login. A socket that exists (or was named) but cannot be used is an
error, never a fallback: a root login falling back to root's own store would
log in somewhere the broker never reads.

So `sudo treesinger login` is the whole login on a systemd host, module or
not. This replaces the module's `treesinger-login`, a `systemd-run` wrapper
that re-created the service's sandbox property by property to run a second,
local login as the broker's user.

## File layout

As it is now (the Bun broker's `src/`, `test/` and `nix/node-modules.nix` are
gone):

```
flake.nix  flake.lock
nix/
  package.nix        treesinger: buildGo127Module, the tests as its check phase
  module.nix         services.treesinger (nixosModules.default adds the flake's package)
  test.nix           NixOS VM test: the module, twice, and the contrib units
contrib/systemd/
  treesinger.service        plain-systemd service (no Nix)
  treesinger.socket         the client socket (and, by drop-in, TCP)
  treesinger-admin.socket   the root-only admin socket, for `sudo treesinger login`
  treesinger.sysusers       sysusers.d: the treesinger user + group
docs/systemd-design.md  this file
main.go              the CLI: serve | login | exec | health | version
internal/broker/     the broker; serve.go has the listeners, readiness and health
README.md            + "systemd" and "NixOS" sections
```

## Nix flake

Inputs: `nixpkgs` (`nixos-unstable`). Systems: `x86_64-linux`,
`aarch64-linux` (the VM test only on `x86_64-linux`).

| Output | What |
|---|---|
| `packages.<system>.default` / `.treesinger` | The broker: one static binary, `bin/treesinger`. |
| `overlays.default` | `treesinger` into `pkgs`. |
| `nixosModules.default` / `.treesinger` | `services.treesinger`, its `package` defaulting (`mkDefault`) to this flake's. `nix/module.nix` alone defaults to `pkgs.treesinger`, from the overlay. |
| `checks.<system>.package` | the package builds, which runs `go test`. |
| `checks.x86_64-linux.nixos` | the VM test. |
| `devShells.<system>.default` | Go (mise stays the primary dev tool). |

(When the broker was Bun, there were also `typecheck`, `test` and
`treesinger-env` checks and a `treesinger-env` package; `treesinger exec`
replaced the last.)

### How the package builds in the sandbox

(The Bun build, kept as the record. The Go package is `nix/package.nix`: a
static `buildGo127Module` with no modules to vendor, its check phase the
tests, and the version below.)

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
| `package` | package | the flake's (via `nixosModules.default`), else `pkgs.treesinger` | Also on `environment.systemPackages`, for `sudo treesinger login` and `treesinger exec`. |
| `user` | str | `"treesinger"` | Runs as. The module creates it (system user) only when it is `"treesinger"`. |
| `group` | str | `"treesinger"` | Primary group, **and the client socket's `SocketGroup=`**. The module always creates it when it is `"treesinger"`. Clients join this group. |
| `stateDir` | path (str) | `"/var/lib/treesinger"` | Holds `tokens.json` (`TREESINGER_STORE = "${stateDir}/tokens.json"`). Default is managed by `StateDirectory=treesinger` (mode 0700); any other path must already exist, owned by `user` — the module adds it to `ReadWritePaths` but does not create or chown it (Dan's `/data/servers/treesinger` is owned by dan, and a tmpfiles rule chowning a pre-existing data dir is a footgun). |
| `socket` | null or str | `"/run/treesinger/treesinger.sock"` | A `ListenStream=` of `treesinger.socket`; systemd makes its directory. `null` for no unix socket. Asserted: absolute, and `socket` or `port` (or both). |
| `port` | null or port | `null` | Another `ListenStream=host:port` of `treesinger.socket` (with `FreeBind=`, as sockets start before the network); `null` = no TCP listener. |
| `host` | str | `"127.0.0.1"` | The TCP listener's address (only used when `port` is set; an IPv6 one is bracketed). Loopback by default: on NixOS the socket is the interface; TCP is opt-in. |
| `openFirewall` | bool | `false` | Open `port` in the firewall. Asserts `port != null`. |
| `ownerUuid` | null or str | `null` | `TREESINGER_OWNER_UUID`. |
| `bearerTokenFile` | null or absolute path string | `null` | A string, not a path literal, which interpolation would copy into the store. `LoadCredential=bearer-token:<path>`, which the broker reads from `$CREDENTIALS_DIRECTORY` itself: no wrapper, and the secret is never in the Nix store, the unit file or the broker's environment. |
| `environment` | attrsOf str | `{}` | Extra env (e.g. `TREESINGER_SKEW_MS`), merged over the above. Asserted: no `TREESINGER_PORT`/`TREESINGER_HOST`, which the socket-activated broker refuses. |
| `environmentFile` | null or absolute path string | `null` | `EnvironmentFile=` for secrets the deployer manages. |

Example — Dan's server:

```nix
services.treesinger = {
  enable = true;
  user = "dan";                         # owns /data/servers/treesinger (1000:100)
  stateDir = "/data/servers/treesinger";  # tokens.json carries over from the container's /data
};
# group stays "treesinger"; the socket is /run/treesinger/treesinger.sock, 0660 root:treesinger
```

The container wrote `/data/tokens.json` on that volume, which is
`/data/servers/treesinger/tokens.json` on the host — exactly the module's
`TREESINGER_STORE` — so migration is "stop the container, switch". No re-login.

### The units the module renders

The two socket units are [above](#socket-activation) (`port` adds
`ListenStream=<host>:<port>` and `FreeBind=yes` to `treesinger.socket`); both
are `WantedBy=sockets.target`. The service:

```ini
[Unit]
Description=Treesinger: Hytale server authentication broker
Requires=treesinger.socket treesinger-admin.socket
After=treesinger.socket treesinger-admin.socket network-online.target
Wants=network-online.target

[Service]
Type=notify
NotifyAccess=main                  # all when Bun spawned systemd-notify
ExecStart=<package>/bin/treesinger serve
Environment=TREESINGER_STORE=<stateDir>/tokens.json …
User=<user>
Group=<group>
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
MemoryDenyWriteExecute=yes         # no JIT since the Go port (Bun needed no)
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged       # not @resources: Go setrlimit()s at start (and bun 1.3 sched_setscheduler()d)
CapabilityBoundingSet=
AmbientCapabilities=
LoadCredential=bearer-token:<bearerTokenFile>   # only when set
EnvironmentFile=<environmentFile>               # only when set
```

No `wantedBy`: the sockets start it. No `RuntimeDirectory`: systemd makes the
sockets' directory, and the broker writes nothing under `/run`. No wrapper in
`ExecStart`: the bearer token is a credential the broker reads itself.

`network-online.target` (`Wants=` and `After=`): the broker serves without the
network, but its clients mint their sessions the moment it is ready, once, at
boot, and a mint fails if Hypixel cannot be reached yet. Waiting here makes
every client ordered after the broker wait too. (A client that relies on the
socket alone, without ordering after the service, still waits: its connection
sits in the backlog until the broker is up.)

Every hardening line is verified in the VM test, not assumed: the broker
starts, serves, notifies, reads and writes its store, and reaches the network
stack with all of them, `ProcSubset=pid` and `PrivateUsers=yes` included.
`~@resources` is **not** there: on nixos-26.05's bun 1.3.13 the broker died
of `SIGSYS` at start — it calls `sched_setscheduler` (in `@resources`) on its
threads; unstable's 1.4.2 does not, so only the 26.05 run caught it.
`systemd-analyze security` scored the Bun broker **1.4** ("OK"); the test
asserts ≤ 1.5. What is left exposed is what it needs: `AF_INET` for Hypixel and
`AF_UNIX`. (The Bun broker also needed W+X memory for its JIT and
`NotifyAccess=all`.)

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

### Login on NixOS

The module puts the package on `environment.systemPackages`, and the login is
`sudo treesinger login`, through the admin socket ([login](#login)): the
running broker writes the store, as itself, in its own sandbox.

This replaced `treesinger-login`, a wrapper that ran a second, local
`treesinger login` under `systemd-run`, with every sandbox property of the
service rendered as a `-p` flag so the two could not drift. It worked, but it
was a script, it duplicated the service's sandbox, and it raced the running
broker for the store; through the admin socket there is one writer, the one
`OAuthClient`, and nothing to keep in step. The VM test runs the login
offline: as root it reaches the broker, whose device flow fails at the
network and writes nothing; as a group member it is refused at the socket,
with no fallback to a local login.

## Plain systemd (no Nix)

Shipped as `contrib/systemd/`, and attached to every release:

- `treesinger.sysusers` → `/etc/sysusers.d/treesinger.conf`:
  `u treesinger - "Treesinger authentication broker" /var/lib/treesinger`
  (creates the user and its same-named group, the client socket's group).
- `treesinger.socket` and `treesinger-admin.socket` → `/etc/systemd/system/`:
  the units [above](#socket-activation), TCP as a commented-out drop-in.
- `treesinger.service` → `/etc/systemd/system/`: the unit above with the
  defaults filled in, `ExecStart=/usr/local/bin/treesinger serve` (the release
  binary), `StateDirectory=treesinger`, a `LoadCredential=bearer-token:` as a
  commented-out line, and `[Install] Also=` the two sockets.

`systemctl enable --now treesinger.socket treesinger-admin.socket`, then
`sudo treesinger login`. A client's `ExecStart` is `treesinger exec`, the same
binary. There is no other file to install.

The VM test's third machine installs exactly these units (only `ExecStart`
substituted), runs `systemd-sysusers` on the sysusers file, starts the
sockets, and checks that the service waits for a connection, the sockets'
modes, and `/health`.

[`docs/systemd.md`](systemd.md) has the install steps, the login, a client
unit, and the NixOS equivalent.

## How each shared contract is met

### Treesinger's own (socket)

| Contract | Met by |
|---|---|
| `services.treesinger` listens on `/run/treesinger/treesinger.sock` | `socket` option default, a `ListenStream=` of `treesinger.socket`. |
| group `treesinger`, mode `0660` | `SocketGroup=<group>` (default `treesinger`), `SocketMode=0660`, made by systemd. Directory `0755 root:root` (`DirectoryMode=`), holding only that socket. |
| `/run/treesinger` holds exactly `treesinger.sock` (Chandlery's VM test) | The admin socket is `/run/treesinger-admin.sock`, outside it. |
| HTTP API unchanged | Unchanged on the client socket; `/v1/login` only on the admin socket. |

### Hytale servers (via Chandlery)

The contract is `services.chandlery.servers.<n>.treesinger.socket =
"/run/treesinger/treesinger.sock"`; Chandlery's module implements it. What it
needs from Treesinger, and what to watch for:

- **Ordering**: `wants`/`after = [ "treesinger.service" ]` when the socket is
  set. `Type=notify` means *after* = serving. With socket activation the
  socket is there from early boot regardless, and a connection made before
  the broker is up waits rather than fails.
- **Access**: `SupplementaryGroups = [ "treesinger" ]` (the static group).
  This holds under `PrivateUsers=yes` too (measured in the VM test): the group
  is unmapped inside the client's user namespace, shown as `nogroup`, but the
  socket's permission check is on the real gid, so `connect()` succeeds.
  Chandlery's open question on `PrivateUsers` and the treesinger group is
  answered: no `PrivateUsers=identity` needed for this.
- **Confinement**: bind the directory or the socket file, optionally:
  `BindReadOnlyPaths = [ "-/run/treesinger" ]`. systemd holds the socket, so
  its inode outlives restarts of the broker and either bind stays live. (When
  the broker made its own socket, a bind of the file pinned the old inode and
  went stale on a restart, so only the directory, kept by
  `RuntimeDirectoryPreserve=yes`, would do.) The `-` keeps the game starting
  (unauthenticated) if the broker is not installed. `connect()` to a
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

  To make (1) a one-liner for anyone on plain systemd, the binary has
  **`treesinger exec`** —

  ```sh
  treesinger exec [--socket PATH | --url URL] -- COMMAND [ARGS…]
  ```

  — which `POST`s `/v1/session` (10 s timeout; default socket
  `/run/treesinger/treesinger.sock`), puts the pair in the two variables on
  success and says so on stderr, warns to stderr and continues without them on
  any failure (exactly the image's degradation), then `execve`s `COMMAND`
  (looked up on `PATH`), which so keeps the pid, stdin and notify socket.
  Tokens never touch argv. The bearer token (`TREESINGER_BEARER_TOKEN`, or the
  `bearer-token` credential) is sent; the variable is removed from what
  `COMMAND` gets, but a credential cannot be: its file stays readable by the
  unit's user, so by `COMMAND`, for as long as the unit runs. A usage
  error (no `--`, no command) exits 2 and runs nothing.

  It replaced `contrib/treesinger-env`, a shell script needing `curl` and `jq`
  that did the same: a shell `exec` then because Bun cannot `execve`, and a
  Bun wrapper would have had to stay resident as the game's parent, breaking
  the notify wrapper's PID and the console FIFO's stdin. Go can, so it is one
  binary again.

  Chandlery's own design (its `docs/nix.md` on its `nix` branch) inlines the
  mint in its run script instead: its `images/hytale/run` learns its own
  `TREESINGER_SOCKET` variable (Chandlery's, not the broker's) and makes the
  same POST with `curl --unix-socket`, which is shape 1. Either works against
  this broker; `treesinger exec` is for servers that are not Chandlery's.

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

(The Bun broker's tests, kept as the record; the Go tests are beside the code
in `internal/broker/`.)

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

`nix flake check` runs the package (whose check phase is `go test`) and the
VM test; CI runs both in its `nix` job. `nix/test.nix` boots three machines,
offline:

- **`defaults`** — `services.treesinger.enable` from `nixosModules.default`:
  - at boot both sockets are listening and the service is **inactive**; the
    first connection starts it, and is answered;
  - client socket `srw-rw---- root treesinger`, `/run/treesinger` `755
    root:root` holding only `treesinger.sock`, `/var/lib/treesinger` `700`;
  - admin socket `srw------- root root`: a group member cannot connect, and
    `/v1/login` on the client socket is `404`;
  - a member gets `503 {"status":"unauthenticated"}` from `/health` and
    `401 {"error":"not_logged_in"}` from `/v1/session`; `treesinger health
    --socket` exits non-zero; a non-member cannot connect;
  - no TCP listener;
  - login: as root the admin socket streams an error (offline) and
    `treesinger login` fails having written nothing; as a member
    `treesinger login` fails with permission denied rather than logging in
    locally;
  - a confined client (user `bob`, `SupplementaryGroups=treesinger`,
    `TemporaryFileSystem=/run`, `BindReadOnlyPaths=-/run/treesinger`) running
    `treesinger exec` gets the broker's 401 (so the socket was reachable),
    warns and runs unauthenticated; its `/run` holds only `treesinger`;
  - a client with `PrivateUsers=yes` and the supplementary group connects;
  - a connection made as `systemctl restart --no-block` runs is answered;
    after `systemctl stop treesinger.service` the socket keeps its inode and
    the confined client (via `nsenter`) still gets an answer, which starts it
    again;
  - `treesinger exec` with no broker warns and runs the command without the
    pair; against a fake broker that wants a bearer token, it passes the pair
    on with `TREESINGER_BEARER_TOKEN` or a `bearer-token` credential, never
    passes the token itself on, and without one runs unauthenticated; a
    usage error runs nothing;
  - `systemd-analyze security`, asserted ≤ 1.5.
- **`custom`** — `user = "gamer"` (an existing normal user),
  `stateDir = "/data/servers/treesinger"` (made by tmpfiles, as a deployer
  would), `port = 8080`, `bearerTokenFile`, extra `environment`:
  - socket `srw-rw---- root treesinger`, the broker running as `gamer`;
  - with a seeded `tokens.json` holding a valid access token, `/health` is
    `200` over the socket and over `127.0.0.1:8080` (the same socket unit),
    and `treesinger health` with no arguments, the Docker probe, passes;
    nothing listens on 8080 beyond loopback, and `/v1/login` there is `404`;
  - `/v1/session` without the bearer is `401 unauthorized`, with it gets past
    the gate (`500`, Hypixel being absent); the secret is in neither the
    unit's environment nor the broker's, `TREESINGER_SKEW_MS` is.
- **`plain`** — no module: `contrib/systemd`'s units and sysusers file, see
  [plain systemd](#plain-systemd-no-nix).

(The Bun broker's run, against nix-config's nixos-26.05 pin with
`--override-input nixpkgs`, caught the `@resources` crash above.)

### Docker

- `./run image` still builds; the image's `HEALTHCHECK` still works (not
  socket-activated → TCP probe, unchanged), and `treesinger login` logs
  in locally, there being no admin socket (first login in the running
  container, later ones with it stopped).

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

1. ~~Does Treesinger ship `treesinger-env`?~~ It did, as `contrib/` and a
   flake package; now it is `treesinger exec`, in the one binary. Chandlery
   inlines its own in `images/hytale/run`.
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
