# Evaluates configurations that use the module and checks what they render,
# without booting anything - the part of the VM test that needs no KVM.
{ pkgs, self }:

let
  inherit (pkgs) lib;

  base = {
    imports = [ self.nixosModules.default ];
    boot.loader.grub.enable = false;
    fileSystems."/" = {
      device = "none";
      fsType = "tmpfs";
    };
    system.stateVersion = "25.11";
  };

  render =
    config:
    (pkgs.nixos {
      imports = [
        base
        config
      ];
    }).config;

  # The defaults: its own user and group, /var/lib/treesinger, the socket only.
  defaults = render { services.treesinger.enable = true; };

  # Dan's shape: an existing user, state outside /var/lib, TCP as well, a
  # bearer token and extra environment.
  custom = render {
    users.users.gamer.isNormalUser = true;
    services.treesinger = {
      enable = true;
      user = "gamer";
      stateDir = "/data/servers/treesinger";
      port = 8080;
      openFirewall = true;
      bearerTokenFile = "/etc/treesinger-bearer";
      environment.TREESINGER_SKEW_MS = "60000";
    };
  };

  # An IPv6 listener, bracketed as in a URL.
  ipv6 = render {
    services.treesinger = {
      enable = true;
      socket = null;
      port = 8080;
      host = "::1";
    };
  };

  # Whether a configuration fails to evaluate, or fails an assertion.
  refused =
    config:
    let
      evaluated = render config;
      attempt = builtins.tryEval (
        builtins.deepSeq (map (unit: unit.text) (lib.attrValues evaluated.systemd.units)) (
          lib.any (assertion: !assertion.assertion) evaluated.assertions
        )
      );
    in
    !attempt.success || attempt.value;

  treesinger = settings: {
    services.treesinger = {
      enable = true;
    }
    // settings;
  };

  # Each must be refused when evaluated, not later by the broker at run time.
  refusals = {
    "no socket and no port" = treesinger { socket = null; };
    "a relative socket" = treesinger { socket = "treesinger.sock"; };
    "openFirewall without a port" = treesinger { openFirewall = true; };
    "TREESINGER_PORT in environment" = treesinger { environment.TREESINGER_PORT = "8080"; };
    "a bearer token file in the store" = treesinger { bearerTokenFile = "bearer"; };
  };
  accepted = {
    "a port and no socket" = treesinger {
      socket = null;
      port = 8080;
    };
  };
  wrong =
    lib.attrNames (lib.filterAttrs (_: config: !refused config) refusals)
    ++ lib.attrNames (lib.filterAttrs (_: refused) accepted);

  unit = config: name: config.systemd.units.${name}.text;
in
assert lib.assertMsg (
  wrong == [ ]
) "module-check: wrongly accepted or refused: ${lib.concatStringsSep ", " wrong}";
pkgs.runCommand "treesinger-module-check"
  {
    socket = unit defaults "treesinger.socket";
    admin = unit defaults "treesinger-admin.socket";
    service = unit defaults "treesinger.service";
    customSocket = unit custom "treesinger.socket";
    customService = unit custom "treesinger.service";
    ipv6Socket = unit ipv6 "treesinger.socket";
    tcp = toString custom.networking.firewall.allowedTCPPorts;
    defaultTcp = toString defaults.networking.firewall.allowedTCPPorts;
    users = builtins.toJSON (lib.attrNames defaults.users.users);
    customUsers = builtins.toJSON (lib.attrNames custom.users.users);
  }
  ''
    fail() { echo "FAIL: $*"; exit 1; }

    grep -qx 'ListenStream=/run/treesinger/treesinger.sock' <<<"$socket" || fail "no client socket"
    grep -qx 'SocketGroup=treesinger' <<<"$socket" || fail "client socket not the group's"
    grep -qx 'SocketMode=0660' <<<"$socket" || fail "client socket mode"
    ! grep -q 'FreeBind' <<<"$socket" || fail "FreeBind without TCP"
    grep -qx 'ListenStream=/run/treesinger-admin.sock' <<<"$admin" || fail "no admin socket"
    grep -qx 'SocketMode=0600' <<<"$admin" || fail "admin socket mode"
    grep -qx 'Service=treesinger.service' <<<"$admin" || fail "admin socket not the broker's"

    grep -qx 'User=treesinger' <<<"$service" || fail "not its own user"
    grep -qx 'StateDirectory=treesinger' <<<"$service" || fail "no StateDirectory"
    grep -qx 'Environment="TREESINGER_STORE=/var/lib/treesinger/tokens.json"' <<<"$service" || fail "store path"
    grep -qx 'Type=notify' <<<"$service" || fail "not Type=notify"
    grep -qx 'RemoveIPC=true' <<<"$service" || fail "own user without RemoveIPC"
    ! grep -q 'LoadCredential' <<<"$service" || fail "a credential nobody asked for"
    [[ $users == *'"treesinger"'* ]] || fail "no treesinger user: $users"
    [[ -z $defaultTcp ]] || fail "opened ports nobody asked for: $defaultTcp"

    grep -qx 'ListenStream=/run/treesinger/treesinger.sock' <<<"$customSocket" || fail "custom: no client socket"
    grep -qx 'ListenStream=127.0.0.1:8080' <<<"$customSocket" || fail "custom: no TCP listener"
    grep -qx 'FreeBind=true' <<<"$customSocket" || fail "custom: TCP without FreeBind"
    grep -qx 'User=gamer' <<<"$customService" || fail "custom: not gamer"
    grep -qx 'ReadWritePaths=/data/servers/treesinger' <<<"$customService" || fail "custom: stateDir not writable"
    ! grep -q 'StateDirectory=' <<<"$customService" || fail "custom: a StateDirectory for someone else's directory"
    grep -qx 'Environment="TREESINGER_STORE=/data/servers/treesinger/tokens.json"' <<<"$customService" || fail "custom: store path"
    grep -qx 'Environment="TREESINGER_SKEW_MS=60000"' <<<"$customService" || fail "custom: environment"
    grep -qx 'LoadCredential=bearer-token:/etc/treesinger-bearer' <<<"$customService" || fail "custom: bearer credential"
    grep -qx 'RemoveIPC=false' <<<"$customService" || fail "custom: RemoveIPC for a shared user"
    [[ $customUsers != *'"treesinger"'* ]] || fail "custom: made a treesinger user: $customUsers"
    [[ " $tcp " == *" 8080 "* ]] || fail "custom: 8080 not opened: $tcp"

    grep -qx 'ListenStream=\[::1\]:8080' <<<"$ipv6Socket" || fail "ipv6: not bracketed"
    ! grep -q 'ListenStream=/' <<<"$ipv6Socket" || fail "ipv6: a socket after socket = null"

    touch $out
  ''
