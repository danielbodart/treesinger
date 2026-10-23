# The module and the plain-systemd unit, booted. Offline, so no login and no
# real mint: what is tested is everything up to Hypixel — readiness, the
# socket's owner, group and mode, who can connect, the store path, the bearer
# credential, restarts, a confined client, treesinger-env, and the hardening.
{ pkgs, module, treesinger, treesinger-env }:

let
  # The flake's nixosModules.default, so its package comes from the module.
  common = {
    imports = [ module ];
    environment.systemPackages = [ pkgs.curl pkgs.jq treesinger-env ];
  };

  # A valid cached access token: /health is 200 without asking Hypixel, which
  # proves the broker found and could read the store.
  loggedIn = pkgs.writeText "tokens.json" (builtins.toJSON {
    refresh_token = "rt";
    access_token = "at";
    access_expires_at = 4102444800000; # 2100
  });

  # The contrib unit as a plain systemd host would install it, ExecStart aside.
  contrib = pkgs.runCommand "treesinger-contrib-units" { } ''
    mkdir -p $out/lib/systemd/system $out/lib/sysusers.d
    substitute ${../contrib/systemd/treesinger.service} $out/lib/systemd/system/treesinger.service \
      --replace-fail /usr/local/bin/treesinger ${pkgs.lib.getExe treesinger}
    cp ${../contrib/systemd/treesinger.sysusers} $out/lib/sysusers.d/treesinger.conf
  '';

  # Stands in for a logged-in broker, to see treesinger-env's success path.
  fakeBroker = pkgs.writers.writePython3 "fake-broker" { } ''
    import http.server
    import json
    import socketserver

    BODY = json.dumps({
        "sessionToken": "st",
        "identityToken": "it",
        "expiresAt": "2100-01-01T00:00:00Z",
    }).encode()


    class Handler(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length") or 0))
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(BODY)))
            self.end_headers()
            self.wfile.write(BODY)

        # A unix socket's peer has no address to log.
        def log_message(self, *args):
            pass


    socketserver.UnixStreamServer("/run/fake-broker.sock", Handler).serve_forever()
  '';

  curlSocket = "curl -sS --unix-socket /run/treesinger/treesinger.sock";
  # Inside a client's namespace /run/current-system is hidden: store paths.
  inClient = pid: "nsenter -t {${pid}} -m";
in
pkgs.testers.runNixOSTest {
  name = "treesinger";

  nodes = {
    # The defaults: its own user, /var/lib/treesinger, the socket only.
    defaults = {
      imports = [ common ];
      services.treesinger.enable = true;
      users.users.alice = { isNormalUser = true; extraGroups = [ "treesinger" ]; };
      users.users.bob.isNormalUser = true;

      # A game server's view: the broker's directory and nothing else of /run,
      # reached through the group, not through bob's own membership.
      systemd.services.confined-client = {
        after = [ "treesinger.service" ];
        wants = [ "treesinger.service" ];
        serviceConfig = {
          User = "bob";
          SupplementaryGroups = [ "treesinger" ];
          ProtectSystem = "strict";
          ProtectHome = true;
          TemporaryFileSystem = "/run";
          BindReadOnlyPaths = [ "-/run/treesinger" ];
          ExecStart = "${pkgs.lib.getExe treesinger-env} --socket /run/treesinger/treesinger.sock -- "
            + "${pkgs.runtimeShell} -c 'echo token=$${HYTALE_SERVER_SESSION_TOKEN:-none}; exec sleep infinity'";
        };
      };

      # In a user namespace the treesinger group is unmapped, shown as
      # nogroup, but the kernel checks the real gid: it still connects.
      systemd.services.private-users-client = {
        after = [ "treesinger.service" ];
        serviceConfig = {
          Type = "oneshot";
          User = "bob";
          SupplementaryGroups = [ "treesinger" ];
          PrivateUsers = true;
          ExecStart = "${pkgs.lib.getExe pkgs.curl} -sS -o /dev/null -w 'status=%{http_code}\\n' "
            + "--unix-socket /run/treesinger/treesinger.sock http://treesinger/health";
        };
      };
    };

    # Dan's shape: an existing user, state outside /var/lib, TCP as well, a
    # bearer token and extra environment.
    custom = {
      imports = [ common ];
      users.users.gamer.isNormalUser = true;
      systemd.tmpfiles.rules = [ "d /data/servers/treesinger 0700 gamer users -" ];
      environment.etc."treesinger-bearer" = { text = "sekrit\n"; mode = "0400"; };
      services.treesinger = {
        enable = true;
        user = "gamer";
        stateDir = "/data/servers/treesinger";
        port = 8080;
        bearerTokenFile = "/etc/treesinger-bearer";
        environment.TREESINGER_SKEW_MS = "60000";
      };
    };

    # No module: contrib/systemd's unit and sysusers file.
    plain = {
      environment.systemPackages = [ pkgs.curl ];
      systemd.packages = [ contrib ];
    };
  };

  testScript = ''
    start_all()

    with subtest("defaults: ready means listening"):
        defaults.wait_for_unit("treesinger.service")
        # Type=notify: active only once bound, so the socket is already there.
        defaults.succeed("test -S /run/treesinger/treesinger.sock")
        defaults.succeed("journalctl -u treesinger | grep -q 'serving on unix:/run/treesinger/treesinger.sock'")

    with subtest("defaults: the socket is the group's, and nobody else's"):
        assert defaults.succeed("stat -c '%A %U %G' /run/treesinger/treesinger.sock").strip() == "srw-rw---- treesinger treesinger"
        assert defaults.succeed("stat -c '%a %U %G' /run/treesinger").strip() == "750 treesinger treesinger"
        assert defaults.succeed("stat -c '%a %U' /var/lib/treesinger").strip() == "700 treesinger"

    with subtest("defaults: a member reaches the unauthenticated broker"):
        out = defaults.succeed("su alice -c '${curlSocket} -w \" %{http_code}\" http://treesinger/health'")
        assert out == '{"status":"unauthenticated"} 503', out
        out = defaults.succeed("su alice -c '${curlSocket} -w \" %{http_code}\" -X POST http://treesinger/v1/session'")
        assert out == '{"error":"not_logged_in"} 401', out
        defaults.fail("su alice -c 'TREESINGER_SOCKET=/run/treesinger/treesinger.sock ${pkgs.lib.getExe treesinger} health'")

    with subtest("defaults: a non-member cannot connect"):
        out = defaults.fail("su bob -c '${curlSocket} http://treesinger/health 2>&1'")
        assert "Permission denied" in out or "Could not connect" in out, out

    with subtest("defaults: no TCP unless asked"):
        defaults.fail("ss -Hltn | grep -q ':8080'")
        defaults.fail("curl -sS http://127.0.0.1:8080/health")

    with subtest("defaults: a confined client degrades to unauthenticated"):
        defaults.systemctl("start confined-client")
        defaults.wait_until_succeeds("journalctl -u confined-client | grep -q token=none")
        # 401 from the broker, not a refused connect: the socket was reachable.
        defaults.succeed("journalctl -u confined-client | grep -q 'returned error: 401'")
        pid = defaults.succeed("systemctl show -P MainPID confined-client").strip()
        assert defaults.succeed(f"${inClient "pid"} ${pkgs.coreutils}/bin/ls /run").split() == ["treesinger"]

    with subtest("defaults: a client with PrivateUsers still connects through the group"):
        defaults.systemctl("start private-users-client")
        defaults.succeed("journalctl -u private-users-client -o cat | grep -qx status=503")

    with subtest("defaults: a restart keeps the directory, so the client still sees the socket"):
        inode = defaults.succeed("stat -c %i /run/treesinger")
        defaults.systemctl("restart treesinger")
        defaults.wait_for_unit("treesinger.service")
        assert defaults.succeed("stat -c %i /run/treesinger") == inode
        out = defaults.succeed(f"${inClient "pid"} ${pkgs.lib.getExe pkgs.curl} -sS --unix-socket /run/treesinger/treesinger.sock http://treesinger/health")
        assert out == '{"status":"unauthenticated"}', out

    with subtest("defaults: stopping removes the socket"):
        defaults.systemctl("stop treesinger")
        defaults.fail("test -e /run/treesinger/treesinger.sock")
        out = defaults.succeed("treesinger-env --socket /run/treesinger/treesinger.sock -- sh -c 'echo token=''${HYTALE_SERVER_SESSION_TOKEN:-none}' 2>&1")
        assert "running unauthenticated" in out and "token=none" in out, out
        defaults.systemctl("start treesinger")
        defaults.wait_for_unit("treesinger.service")

    with subtest("defaults: treesinger-env passes a minted session on"):
        defaults.succeed("systemd-run --unit=fake-broker ${fakeBroker}")
        defaults.wait_for_file("/run/fake-broker.sock")
        out = defaults.succeed("treesinger-env --socket /run/fake-broker.sock -- sh -c 'echo $HYTALE_SERVER_SESSION_TOKEN $HYTALE_SERVER_IDENTITY_TOKEN'")
        assert out.strip() == "st it", out

    with subtest("defaults: treesinger-login runs as the broker, in its sandbox"):
        # Offline, so the device flow cannot start; but it gets as far as the
        # network, as the right user, and writes nothing.
        out = defaults.fail("treesinger-login </dev/null 2>&1")
        print(out)
        assert "EACCES" not in out and "denied" not in out, out
        defaults.fail("test -e /var/lib/treesinger/tokens.json")

    with subtest("defaults: hardening"):
        out = defaults.succeed("systemd-analyze security --no-pager treesinger.service")
        print(out)
        score = float(out.strip().splitlines()[-1].split(":")[1].split()[0])
        assert score <= 1.5, f"exposure {score}"

    with subtest("custom: an existing user, its own state directory"):
        custom.wait_for_unit("treesinger.service")
        assert custom.succeed("stat -c '%A %U %G' /run/treesinger/treesinger.sock").strip() == "srw-rw---- gamer treesinger"
        custom.succeed("install -m 600 -o gamer -g users ${loggedIn} /data/servers/treesinger/tokens.json")
        out = custom.succeed("${curlSocket} -w ' %{http_code}' http://treesinger/health")
        assert out == '{"status":"ok"} 200', out
        custom.succeed("TREESINGER_SOCKET=/run/treesinger/treesinger.sock ${pkgs.lib.getExe treesinger} health")

    with subtest("custom: TCP on loopback as well"):
        out = custom.succeed("curl -sS -w ' %{http_code}' http://127.0.0.1:8080/health")
        assert out == '{"status":"ok"} 200', out
        custom.fail("ss -Hltn | grep ':8080' | grep -v '127.0.0.1'")

    with subtest("custom: the bearer token comes from the credential"):
        out = custom.succeed("${curlSocket} -w ' %{http_code}' -X POST http://treesinger/v1/session")
        assert out == '{"error":"unauthorized"} 401', out
        # Past the gate, the mint itself needs Hypixel, which is not here.
        out = custom.succeed("${curlSocket} -w ' %{http_code}' -H 'Authorization: Bearer sekrit' -X POST http://treesinger/v1/session")
        assert out.endswith(" 500") and "unauthorized" not in out, out
        env = custom.succeed("systemctl show -P Environment treesinger")
        assert "sekrit" not in env and "TREESINGER_SKEW_MS=60000" in env, env

    with subtest("plain: contrib/systemd's unit and sysusers file"):
        plain.succeed("systemd-sysusers ${contrib}/lib/sysusers.d/treesinger.conf")
        plain.succeed("getent passwd treesinger")
        plain.systemctl("start treesinger")
        plain.wait_for_unit("treesinger.service")
        assert plain.succeed("stat -c '%A %U %G' /run/treesinger/treesinger.sock").strip() == "srw-rw---- treesinger treesinger"
        out = plain.succeed("${curlSocket} -w ' %{http_code}' http://treesinger/health")
        assert out == '{"status":"unauthenticated"} 503', out
        assert plain.succeed("stat -c '%a %U' /var/lib/treesinger").strip() == "700 treesinger"
        print(plain.succeed("systemd-analyze security --no-pager treesinger.service | tail -1"))
  '';
}
