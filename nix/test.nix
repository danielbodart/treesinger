# The module and the plain-systemd units, booted. Offline, so no login and no
# real mint: what is tested is everything up to Hypixel — socket activation,
# the sockets' owners, groups and modes, who can connect, the admin socket,
# the store path, the bearer credential, restarts, a confined client,
# `treesinger exec`, and the hardening.
{ pkgs, module, treesinger }:

let
  inherit (pkgs) lib;
  exe = lib.getExe treesinger;

  # The flake's nixosModules.default, so its package comes from the module,
  # which also puts it on the path.
  common = {
    imports = [ module ];
    environment.systemPackages = [ pkgs.curl ];
  };

  # A valid cached access token: /health is 200 without asking Hypixel, which
  # proves the broker found and could read the store.
  loggedIn = pkgs.writeText "tokens.json" (builtins.toJSON {
    refresh_token = "rt";
    access_token = "at";
    access_expires_at = 4102444800000; # 2100
  });

  # The contrib units as a plain systemd host would install them, ExecStart
  # aside.
  contrib = pkgs.runCommand "treesinger-contrib-units" { } ''
    mkdir -p $out/lib/systemd/system $out/lib/sysusers.d
    substitute ${../contrib/systemd/treesinger.service} $out/lib/systemd/system/treesinger.service \
      --replace-fail /usr/local/bin/treesinger ${exe}
    # Named explicitly: a path interpolates as a store path, <hash>-name, and
    # cp into a directory would keep that as the unit's name.
    cp ${../contrib/systemd/treesinger.socket} $out/lib/systemd/system/treesinger.socket
    cp ${../contrib/systemd/treesinger-admin.socket} $out/lib/systemd/system/treesinger-admin.socket
    cp ${../contrib/systemd/treesinger.sysusers} $out/lib/sysusers.d/treesinger.conf
  '';

  # Stands in for a logged-in broker, to see `treesinger exec`'s success
  # path. It wants a bearer token, so a pair proves exec sent one.
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
            ok = self.headers.get("Authorization") == "Bearer sekrit"
            body = BODY if ok else b'{"error":"unauthorized"}'
            self.send_response(200 if ok else 401)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        # A unix socket's peer has no address to log.
        def log_message(self, *args):
            pass


    socketserver.UnixStreamServer("/run/fake-broker.sock", Handler).serve_forever()
  '';

  curlSocket = "curl -sS --unix-socket /run/treesinger/treesinger.sock";
  curlAdmin = "curl -sS --unix-socket /run/treesinger-admin.sock";
  showEnv = "sh -c 'echo \${HYTALE_SERVER_SESSION_TOKEN:-none} \${HYTALE_SERVER_IDENTITY_TOKEN:-none} \${TREESINGER_BEARER_TOKEN:-unset}'";
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
          ExecStart = "${exe} exec --socket /run/treesinger/treesinger.sock -- "
            + "${pkgs.runtimeShell} -c 'echo token=$${HYTALE_SERVER_SESSION_TOKEN:-none}; exec sleep infinity'";
        };
      };

      # In a user namespace the treesinger group is unmapped, shown as
      # nogroup, but the kernel checks the real gid: it still connects.
      systemd.services.private-users-client = {
        serviceConfig = {
          Type = "oneshot";
          User = "bob";
          SupplementaryGroups = [ "treesinger" ];
          PrivateUsers = true;
          ExecStart = "${lib.getExe pkgs.curl} -sS -o /dev/null -w 'status=%{http_code}\\n' "
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

    # No module: contrib/systemd's units and sysusers file.
    plain = {
      environment.systemPackages = [ pkgs.curl ];
      systemd.packages = [ contrib ];
    };
  };

  testScript = ''
    start_all()

    with subtest("defaults: the sockets are up at boot, the broker only once asked"):
        defaults.wait_for_unit("treesinger.socket")
        defaults.wait_for_unit("treesinger-admin.socket")
        defaults.wait_for_unit("multi-user.target")
        assert defaults.succeed("systemctl is-active treesinger.service || true").strip() == "inactive"
        out = defaults.succeed("su alice -c '${curlSocket} -w \" %{http_code}\" http://treesinger/health'")
        assert out == '{"status":"unauthenticated"} 503', out
        defaults.wait_for_unit("treesinger.service")

    with subtest("defaults: the socket is the group's, and nobody else's"):
        assert defaults.succeed("stat -c '%A %U %G' /run/treesinger/treesinger.sock").strip() == "srw-rw---- root treesinger"
        assert defaults.succeed("stat -c '%a %U %G' /run/treesinger").strip() == "755 root root"
        assert defaults.succeed("stat -c '%a %U' /var/lib/treesinger").strip() == "700 treesinger"

    with subtest("defaults: the admin socket is root's, and not in /run/treesinger"):
        assert defaults.succeed("stat -c '%A %U %G' /run/treesinger-admin.sock").strip() == "srw------- root root"
        assert defaults.succeed("ls -A /run/treesinger").split() == ["treesinger.sock"]
        out = defaults.fail("su alice -c '${curlAdmin} -X POST http://treesinger/v1/login 2>&1'")
        assert "Permission denied" in out or "Could not connect" in out, out
        # The admin endpoint is only on the admin socket.
        out = defaults.succeed("su alice -c '${curlSocket} -w \" %{http_code}\" -X POST http://treesinger/v1/login'")
        assert out.endswith(" 404"), out

    with subtest("defaults: a member reaches the unauthenticated broker"):
        out = defaults.succeed("su alice -c '${curlSocket} -w \" %{http_code}\" -X POST http://treesinger/v1/session'")
        assert out == '{"error":"not_logged_in"} 401', out
        defaults.fail("su alice -c 'treesinger health --socket /run/treesinger/treesinger.sock'")

    with subtest("defaults: a non-member cannot connect"):
        out = defaults.fail("su bob -c '${curlSocket} http://treesinger/health 2>&1'")
        assert "Permission denied" in out or "Could not connect" in out, out

    with subtest("defaults: no TCP unless asked"):
        defaults.fail("ss -Hltn | grep -q ':8080'")
        defaults.fail("curl -sS http://127.0.0.1:8080/health")

    with subtest("defaults: login runs through the broker, as root only"):
        # Offline, so the broker's device flow cannot start: the stream ends
        # in an error, and nothing is written.
        out = defaults.succeed("${curlAdmin} -N -X POST http://treesinger/v1/login")
        print(out)
        assert '"error"' in out.strip().splitlines()[-1], out
        out = defaults.fail("treesinger login </dev/null 2>&1")
        print(out)
        assert "through the broker at /run/treesinger-admin.sock" in out and "denied" not in out.lower(), out
        defaults.fail("test -e /var/lib/treesinger/tokens.json")
        # The socket is there but not alice's: an error, not a local login.
        out = defaults.fail("su alice -c 'treesinger login </dev/null 2>&1'")
        assert "permission denied" in out.lower(), out
        defaults.fail("test -e /home/alice/.config/treesinger")

    with subtest("defaults: a confined client boots unauthenticated, and says why"):
        defaults.systemctl("start confined-client")
        defaults.wait_until_succeeds("journalctl -u confined-client | grep -q token=none")
        # The broker's not_logged_in, not a refused connect: the socket was reachable.
        defaults.succeed("journalctl -u confined-client | grep -q 'not_logged_in); running unauthenticated'")
        pid = defaults.succeed("systemctl show -P MainPID confined-client").strip()
        assert defaults.succeed(f"${inClient "pid"} ${pkgs.coreutils}/bin/ls /run").split() == ["treesinger"]

    with subtest("defaults: a client with PrivateUsers still connects through the group"):
        defaults.systemctl("start private-users-client")
        defaults.succeed("journalctl -u private-users-client -o cat | grep -qx status=503")

    with subtest("defaults: a connection during a restart is answered, and so is one after a stop"):
        inode = defaults.succeed("stat -c %i /run/treesinger/treesinger.sock")
        defaults.succeed("systemctl restart --no-block treesinger.service")
        out = defaults.succeed("su alice -c '${curlSocket} -w \" %{http_code}\" http://treesinger/health'")
        assert out == '{"status":"unauthenticated"} 503', out
        defaults.wait_for_unit("treesinger.service")
        defaults.systemctl("stop treesinger.service")
        assert defaults.succeed("systemctl is-active treesinger.service || true").strip() == "inactive"
        # systemd holds the socket: the same one, still there, still answered.
        assert defaults.succeed("stat -c %i /run/treesinger/treesinger.sock") == inode
        out = defaults.succeed(f"${inClient "pid"} ${lib.getExe pkgs.curl} -sS --unix-socket /run/treesinger/treesinger.sock http://treesinger/health")
        assert out == '{"status":"unauthenticated"}', out
        defaults.wait_for_unit("treesinger.service")

    with subtest("defaults: treesinger exec boots unauthenticated without a broker"):
        out = defaults.succeed("treesinger exec --socket /run/nothing.sock -- ${showEnv} 2>&1")
        print(out)
        assert out.strip().splitlines()[-1] == "none none unset", out
        assert "running unauthenticated" in out, out

    with subtest("defaults: treesinger exec passes a minted session on, and not the bearer token"):
        defaults.succeed("systemd-run --unit=fake-broker ${fakeBroker}")
        defaults.wait_for_file("/run/fake-broker.sock")
        out = defaults.succeed("TREESINGER_BEARER_TOKEN=sekrit treesinger exec --socket /run/fake-broker.sock -- ${showEnv}")
        assert out.strip() == "st it unset", out
        # The same token as a systemd credential, its trailing newline trimmed.
        defaults.succeed("mkdir -p /tmp/creds && printf 'sekrit\\n' > /tmp/creds/bearer-token")
        out = defaults.succeed("CREDENTIALS_DIRECTORY=/tmp/creds treesinger exec --socket /run/fake-broker.sock -- ${showEnv}")
        assert out.strip() == "st it unset", out
        # Without it the fake refuses, and exec still runs the command.
        out = defaults.succeed("treesinger exec --socket /run/fake-broker.sock -- ${showEnv}")
        assert out.strip() == "none none unset", out
        # Usage errors run nothing.
        defaults.fail("treesinger exec --socket /run/fake-broker.sock touch /tmp/ran")
        defaults.fail("test -e /tmp/ran")

    with subtest("defaults: hardening"):
        out = defaults.succeed("systemd-analyze security --no-pager treesinger.service")
        print(out)
        score = float(out.strip().splitlines()[-1].split(":")[1].split()[0])
        assert score <= 1.5, f"exposure {score}"

    with subtest("custom: an existing user, its own state directory"):
        custom.wait_for_unit("treesinger.socket")
        assert custom.succeed("stat -c '%A %U %G' /run/treesinger/treesinger.sock").strip() == "srw-rw---- root treesinger"
        custom.succeed("install -m 600 -o gamer -g users ${loggedIn} /data/servers/treesinger/tokens.json")
        out = custom.succeed("${curlSocket} -w ' %{http_code}' http://treesinger/health")
        assert out == '{"status":"ok"} 200', out
        custom.succeed("treesinger health --socket /run/treesinger/treesinger.sock")
        assert custom.succeed("ps -o user= -p $(systemctl show -P MainPID treesinger)").strip() == "gamer"

    with subtest("custom: TCP on loopback as well, from the same socket unit"):
        out = custom.succeed("curl -sS -w ' %{http_code}' http://127.0.0.1:8080/health")
        assert out == '{"status":"ok"} 200', out
        custom.fail("ss -Hltn | grep ':8080' | grep -v '127.0.0.1'")
        # The default probe, as the Docker HEALTHCHECK makes it.
        custom.succeed("treesinger health")
        out = custom.succeed("curl -sS -w ' %{http_code}' -X POST http://127.0.0.1:8080/v1/login")
        assert out.endswith(" 404"), out

    with subtest("custom: the bearer token comes from the credential"):
        out = custom.succeed("${curlSocket} -w ' %{http_code}' -X POST http://treesinger/v1/session")
        assert out == '{"error":"unauthorized"} 401', out
        # Past the gate, the mint itself needs Hypixel, which is not here.
        out = custom.succeed("${curlSocket} -w ' %{http_code}' -H 'Authorization: Bearer sekrit' -X POST http://treesinger/v1/session")
        assert out.endswith(" 500") and "unauthorized" not in out, out
        env = custom.succeed("systemctl show -P Environment treesinger")
        assert "sekrit" not in env and "TREESINGER_SKEW_MS=60000" in env, env
        environ = custom.succeed("tr '\\0' '\\n' < /proc/$(systemctl show -P MainPID treesinger)/environ")
        assert "sekrit" not in environ and "TREESINGER_BEARER_TOKEN" not in environ, environ

    with subtest("plain: contrib/systemd's units and sysusers file"):
        plain.succeed("systemd-sysusers ${contrib}/lib/sysusers.d/treesinger.conf")
        plain.succeed("getent passwd treesinger")
        plain.succeed("systemctl start treesinger.socket treesinger-admin.socket")
        assert plain.succeed("systemctl is-active treesinger.service || true").strip() == "inactive"
        assert plain.succeed("stat -c '%A %U %G' /run/treesinger/treesinger.sock").strip() == "srw-rw---- root treesinger"
        assert plain.succeed("stat -c '%A %U %G' /run/treesinger-admin.sock").strip() == "srw------- root root"
        out = plain.succeed("${curlSocket} -w ' %{http_code}' http://treesinger/health")
        assert out == '{"status":"unauthenticated"} 503', out
        plain.wait_for_unit("treesinger.service")
        assert plain.succeed("stat -c '%a %U' /var/lib/treesinger").strip() == "700 treesinger"
        print(plain.succeed("systemd-analyze security --no-pager treesinger.service | tail -1"))
  '';
}
