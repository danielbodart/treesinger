# Running Treesinger on systemd

Natively, the broker listens on a **unix socket** instead of a network port:
`/run/treesinger/treesinger.sock`, mode `0660`, group `treesinger`. Being in
that group is what lets a server mint a session. There is no TCP listener
unless you set `TREESINGER_PORT` as well. The unit is `Type=notify`, so anything
ordered `After=treesinger.service` finds the socket already there.

There is no `treesinger.socket` unit: Bun cannot serve on a socket handed
over by systemd, so the service makes its own, in its runtime directory.

There are two ways to install it: the [NixOS module](#nixos), which is the whole
setup in a few lines of configuration, or the [release binary and
unit](#without-nix), which works on any other systemd distribution. The
variables are in the [README](../README.md#configuration). To run it in a
container instead, see [Running Treesinger in Docker](docker.md).

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
Hypixel. `treesinger-env` is described [below](#a-hytale-server-as-a-client):

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
| `treesinger.service` | The unit (hardened, see `systemd-analyze security treesinger`). |
| `treesinger.sysusers` | A sysusers file for the `treesinger` user and group. |
| `treesinger-env` | The client helper, for a server's unit (needs `curl` and `jq`). |
| `SHA256SUMS` | Checksums of the above. |

The same files are in [`contrib/`](../contrib) for a checkout. The binaries are
built against glibc, which is every mainstream systemd distribution, and have
no other dependencies.

### Install

```sh
cd "$(mktemp -d)"
arch=$(uname -m | sed 's/x86_64/x64/; s/aarch64/arm64/')
base=https://github.com/danielbodart/treesinger/releases/latest/download
for f in "treesinger-linux-$arch" treesinger.service treesinger.sysusers treesinger-env SHA256SUMS; do
  curl -fsSLO "$base/$f"
done
sha256sum -c --ignore-missing SHA256SUMS

sudo install -m 755 "treesinger-linux-$arch" /usr/local/bin/treesinger
sudo install -m 755 treesinger-env /usr/local/bin/treesinger-env
sudo install -m 644 treesinger.sysusers /etc/sysusers.d/treesinger.conf
sudo install -m 644 treesinger.service /etc/systemd/system/
sudo systemd-sysusers
sudo systemctl daemon-reload
sudo systemctl enable --now treesinger

# The one-time login, as the broker's user, into its store. No restart after:
# the broker reads the store on every request.
sudo -u treesinger TREESINGER_STORE=/var/lib/treesinger/tokens.json treesinger login
```

To upgrade, replace `/usr/local/bin/treesinger` the same way and
`sudo systemctl restart treesinger`. A running server is not disturbed: servers
only ask the broker for a session when they start.

### A Hytale server as a client

Join the group, order after the broker (which waits for
`network-online.target`, so the mint at boot can reach Hypixel), and mint the
session as the server starts.
`treesinger-env` ([`contrib/treesinger-env`](../contrib/treesinger-env), needs
`curl` and `jq`) does that by wrapping `ExecStart`, so the tokens are never
written to a file: it asks the broker for a session, puts it in the variables
the server reads itself (`HYTALE_SERVER_SESSION_TOKEN` /
`HYTALE_SERVER_IDENTITY_TOKEN`, never argv), then `exec`s the server. If the
broker is down or not logged in, it warns and starts the server
unauthenticated.

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
