# Running Treesinger on systemd

Natively, the broker is **socket-activated**. systemd holds two sockets and
starts `treesinger.service` on the first connection to either:

- **`treesinger.socket`**: `/run/treesinger/treesinger.sock`, mode `0660`,
  group `treesinger`. Being in that group is what lets a server mint a session.
  There is no TCP listener unless you add one to this unit.
- **`treesinger-admin.socket`**: `/run/treesinger-admin.sock`, mode `0600`,
  root only. `sudo treesinger login` logs the running broker in through it.
  It is deliberately outside `/run/treesinger`, which is bind-mounted into
  confined game servers.

Because systemd owns the sockets, they exist from early boot and outlive the
broker: a connection made while the broker starts, restarts or is being
upgraded waits for it rather than being refused, and the socket file a client
bound into its namespace is never replaced. The service is `Type=notify`, so
anything ordered `After=treesinger.service` starts once it is serving (and,
through it, once `network-online.target` is reached, so the mint at boot can
reach Hypixel).

There are two ways to install it: the [NixOS module](#nixos), which is the whole
setup in a few lines of configuration, or the [release binary and
units](#without-nix), which work on any other systemd distribution. The
variables and commands are in the [README](../README.md#commands). To run it in
a container instead, see [Running Treesinger in Docker](docker.md).

## NixOS

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

Then, once, `sudo treesinger login`: the module puts `treesinger` on the
system path, and the login runs inside the broker, through its admin socket,
so the token lands in the broker's store with its owner and mode. No restart
afterwards: the broker reads the store on every request.

| Option | Default | |
|---|---|---|
| `enable` | `false` | |
| `package` | this flake's | Also put on the system path. |
| `user` | `"treesinger"` | Created when left as `treesinger`; any other user must exist and own `stateDir`. |
| `group` | `"treesinger"` | The client socket's group; created when left as `treesinger`. |
| `stateDir` | `"/var/lib/treesinger"` | Holds `tokens.json`. Any other path must already exist; it is never created or chowned. |
| `socket` | `"/run/treesinger/treesinger.sock"` | The client socket. `null` for none, which needs `port`. |
| `port` | `null` | TCP as well, as another `ListenStream=` on `treesinger.socket`. |
| `host` | `"127.0.0.1"` | TCP bind address. |
| `openFirewall` | `false` | Open `port`. |
| `ownerUuid` | `null` | Profile to mint for. |
| `bearerTokenFile` | `null` | Shared secret for `/v1/session`, given to the broker as `LoadCredential=bearer-token:<path>`, so never in the store or its environment: an absolute path as a string, not `./file`. |
| `environment` | `{}` | Extra environment, e.g. `TREESINGER_SKEW_MS`. Not `TREESINGER_PORT`/`HOST`: use `port`/`host`. |
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
Hypixel. `treesinger exec` is described [below](#a-hytale-server-as-a-client):

```nix
systemd.services.hytale = {
  wants = [ "treesinger.service" ];
  after = [ "treesinger.service" ];
  serviceConfig = {
    SupplementaryGroups = [ config.services.treesinger.group ];
    BindReadOnlyPaths = [ "-/run/treesinger" ];
    ExecStart = "${lib.getExe config.services.treesinger.package} exec "
      + "--socket ${config.services.treesinger.socket} -- ${hytale}/bin/hytale-server";
  };
};
```

The flake also has `overlays.default` (`pkgs.treesinger`) and a VM test in
`checks.x86_64-linux.nixos`. [`docs/systemd-design.md`](systemd-design.md) has
the design and the reasons behind it.

## Without Nix

Every push to `trunk` is published as a
[GitHub release](https://github.com/danielbodart/treesinger/releases) with
everything a plain systemd host needs:

| Asset | |
| --- | --- |
| `treesinger-linux-x64` | The binary, for 64-bit Intel and AMD. |
| `treesinger-linux-arm64` | The binary, for 64-bit ARM. |
| `treesinger.service` | The service (hardened, see `systemd-analyze security treesinger`). |
| `treesinger.socket` | The client socket, `/run/treesinger/treesinger.sock`. |
| `treesinger-admin.socket` | The admin socket, `/run/treesinger-admin.sock`, for `sudo treesinger login`. |
| `treesinger.sysusers` | A sysusers file for the `treesinger` user and group. |
| `SHA256SUMS` | Checksums of the above. |

The same files are in [`contrib/systemd`](../contrib/systemd) for a checkout.
The binaries are static, so they run on any distribution, and have no
dependencies.

### Install

```sh
cd "$(mktemp -d)"
arch=$(uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/')
base=https://github.com/danielbodart/treesinger/releases/latest/download
for f in "treesinger-linux-$arch" treesinger.service treesinger.socket \
    treesinger-admin.socket treesinger.sysusers SHA256SUMS; do
  curl -fsSLO "$base/$f"
done
sha256sum -c --ignore-missing SHA256SUMS

sudo install -m 755 "treesinger-linux-$arch" /usr/local/bin/treesinger
sudo install -m 644 treesinger.sysusers /etc/sysusers.d/treesinger.conf
sudo install -m 644 treesinger.service treesinger.socket treesinger-admin.socket \
  /etc/systemd/system/
sudo systemd-sysusers
sudo systemctl daemon-reload
# The sockets are what is enabled; the service starts on the first connection.
sudo systemctl enable --now treesinger.socket treesinger-admin.socket

# The one-time login, through the broker's admin socket (which starts it).
# No restart after: the broker reads the store on every request.
sudo treesinger login
```

`treesinger login` uses the admin socket whenever `/run/treesinger-admin.sock`
exists, and fails rather than logging in somewhere else if it cannot use it
(not root, say). Only where there is no admin socket does it run the device
flow locally, into `TREESINGER_STORE`.

TCP as well, for clients that cannot reach the socket (containers, other
hosts on a LAN), is another listener on the same socket unit:

```sh
sudo systemctl edit treesinger.socket
```

```ini
[Socket]
ListenStream=127.0.0.1:8080
```

then `sudo systemctl restart treesinger.socket`. The broker serves every
listener systemd passes it, and the admin endpoints on none of them but the
admin socket, which must be a unix socket (a port has no mode to restrict
it, so the broker refuses `FileDescriptorName=admin` on one). Do not set `TREESINGER_PORT` or `TREESINGER_HOST` in the
service: when socket-activated the broker refuses them, since the listeners
come from the socket units.

A bearer token for `/v1/session`, kept out of the unit and out of the
broker's environment, is a credential. Put the secret in a file only root can
read, then add the credential (`sudo systemctl edit treesinger`):

```sh
sudo install -d -m 700 /etc/treesinger
sudo install -m 600 /dev/null /etc/treesinger/bearer-token
head -c 32 /dev/urandom | od -An -tx1 | tr -d " \n" |
  sudo tee /etc/treesinger/bearer-token >/dev/null
```

```ini
[Service]
LoadCredential=bearer-token:/etc/treesinger/bearer-token
```

then `sudo systemctl restart treesinger`. The broker reads the token once, at
startup, and a socket-activated broker does not exit when idle, so until that
restart `/v1/session` stays open.

### Upgrade

Download and verify every asset again, as in the first block of Install (the
units can change between releases), then, in the same directory:

```sh
sudo systemctl stop treesinger.service
# Once, coming from a release without socket units, whose service was enabled
# directly: drop its multi-user.target link (the sockets are what is enabled
# now). Before the new units are installed, since the new service's
# Also= would disable the sockets with it.
sudo systemctl disable treesinger.service
sudo install -m 755 "treesinger-linux-$arch" /usr/local/bin/treesinger
sudo install -m 644 treesinger.sysusers /etc/sysusers.d/treesinger.conf
sudo install -m 644 treesinger.service treesinger.socket treesinger-admin.socket \
  /etc/systemd/system/
sudo systemd-sysusers
sudo systemctl daemon-reload
sudo systemctl enable treesinger.socket treesinger-admin.socket
# Restarted, not just started, so changed socket units take effect; the
# broker starts again on the next connection.
sudo systemctl restart treesinger.socket treesinger-admin.socket
```

Replacing only the binary under the old unit does not work: that unit sets
`TREESINGER_SOCKET`, which this broker refuses at startup rather than run
without its socket units (and so on TCP, on every interface, with no unix
socket for its clients). A running server is not disturbed by an upgrade:
servers only ask the broker for a session when they start. One that starts
while the sockets are down, between the stop and the restart, boots
unauthenticated.

### A Hytale server as a client

Join the group, order after the broker (which waits for
`network-online.target`, so the mint at boot can reach Hypixel), and mint the
session as the server starts. `treesinger exec` does that by wrapping
`ExecStart`, so the tokens are never written to a file: it asks the broker for
a session, puts it in the variables the server reads itself
(`HYTALE_SERVER_SESSION_TOKEN` / `HYTALE_SERVER_IDENTITY_TOKEN`, never argv),
then `exec`s the server, which keeps its pid, stdin and notify socket. If the
broker cannot be reached or is not logged in, it warns and starts the server
unauthenticated.

With a bearer token, give the client the same secret. `exec` takes it from
`TREESINGER_BEARER_TOKEN` and removes that variable before it execs the
server, so a root-only `EnvironmentFile=` (a line
`TREESINGER_BEARER_TOKEN=...`, read by systemd, not by the server) keeps it
from the server. A `LoadCredential=bearer-token:...` works too but does not
hide it: the credential file stays readable by the unit's user, and so by the
server and anything it loads, for as long as the unit runs. Either way the
token only keeps out clients that are not given it; the server is trusted
with sessions already.

```ini
[Unit]
Wants=treesinger.service
After=treesinger.service

[Service]
SupplementaryGroups=treesinger
ExecStart=/usr/local/bin/treesinger exec --socket /run/treesinger/treesinger.sock -- \
    java -jar /opt/hytale/Server/HytaleServer.jar --assets /opt/hytale/Assets.zip
# A confined server: bind the broker's directory (or just the socket file;
# systemd keeps it across restarts of the broker, so either stays live). "-"
# boots it unauthenticated without one.
BindReadOnlyPaths=-/run/treesinger
```

`--socket /run/treesinger/treesinger.sock` is the default, spelled out here;
`--url http://host:8080` reaches a broker over TCP instead.

A client may use `PrivateUsers=yes`: the `treesinger` group is unmapped in its
user namespace, but the kernel checks the real group, so it still connects.
