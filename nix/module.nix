# services.treesinger: the broker as a hardened systemd service, listening on
# a unix socket whose group is the access boundary. See docs/systemd.md.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.treesinger;
  inherit (lib) mkOption mkIf types;

  defaultStateDir = "/var/lib/treesinger";
  managedStateDir = cfg.stateDir == defaultStateDir;
  inHome = lib.hasPrefix "/home/" cfg.stateDir || lib.hasPrefix "/root/" cfg.stateDir;

  environment = {
    TREESINGER_STORE = "${cfg.stateDir}/tokens.json";
  } // lib.optionalAttrs (cfg.ownerUuid != null) {
    TREESINGER_OWNER_UUID = cfg.ownerUuid;
  } // cfg.environment;

  listeners = lib.optionalAttrs (cfg.socket != null) {
    TREESINGER_SOCKET = cfg.socket;
  } // lib.optionalAttrs (cfg.port != null) {
    TREESINGER_PORT = toString cfg.port;
    TREESINGER_HOST = cfg.host;
  };

  # Who the broker runs as and what it can see: shared by the service and by
  # treesinger-login, so the login writes the store exactly as serve reads it.
  sandbox = {
    User = cfg.user;
    Group = cfg.group;
    UMask = "0077";
    NoNewPrivileges = true;
    ProtectSystem = "strict";
    ProtectHome = !inHome;
    PrivateTmp = true;
    PrivateDevices = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectControlGroups = true;
    ProtectClock = true;
    ProtectHostname = true;
    ProtectProc = "invisible";
    ProcSubset = "pid";
    # unix for the socket and systemd-notify; inet for HTTPS to Hypixel.
    RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
    RestrictNamespaces = true;
    RestrictRealtime = true;
    RestrictSUIDSGID = true;
    LockPersonality = true;
    # JavaScriptCore compiles JIT code into writable, executable memory.
    MemoryDenyWriteExecute = false;
    SystemCallArchitectures = "native";
    # Not ~@resources: bun 1.3 calls sched_setscheduler on its threads, and
    # dies of SIGSYS without it (1.4 does not).
    SystemCallFilter = [ "@system-service" "~@privileged" ];
    CapabilityBoundingSet = "";
    AmbientCapabilities = "";
    PrivateUsers = true;
    # Only for its own user: for a shared one (dan) it would remove that
    # user's IPC objects everywhere whenever the broker stops.
    RemoveIPC = cfg.user == "treesinger";
  } // (if managedStateDir then {
    StateDirectory = "treesinger";
    StateDirectoryMode = "0700";
  } else {
    ReadWritePaths = [ cfg.stateDir ];
  });

  # sandbox as systemd-run -p flags: a list is one property per element, as in
  # a unit file.
  property = name: value:
    let render = v: if lib.isBool v then lib.boolToString v else toString v;
    in map (v: "-p ${lib.escapeShellArg "${name}=${render v}"}") (lib.toList value);
  properties = lib.concatStringsSep " \\\n    " (lib.flatten (lib.mapAttrsToList property sandbox));

  login = pkgs.writeShellApplication {
    name = "treesinger-login";
    runtimeInputs = [ config.systemd.package ];
    text = ''
      # The one-time device login, as the broker's user inside the broker's
      # sandbox, so tokens.json lands where `treesinger serve` reads it, with
      # the right owner and mode. Needs root (sudo). The running broker reads
      # the store on every request: no restart afterwards.
      exec systemd-run --quiet --wait --collect --pty --pipe \
        --unit=treesinger-login \
        ${properties} \
        ${lib.optionalString (cfg.environmentFile != null) "-p EnvironmentFile=${lib.escapeShellArg cfg.environmentFile}"} \
        ${lib.concatMapStringsSep " " (n: "--setenv=${lib.escapeShellArg "${n}=${environment.${n}}"}") (lib.attrNames environment)} \
        ${lib.getExe cfg.package} login
    '';
  };

  # The bearer token comes in as a credential, so it is in neither the store
  # nor the unit; this puts it where the broker reads it, then becomes it.
  serve = pkgs.writeShellScript "treesinger-serve" ''
    ${lib.optionalString (cfg.bearerTokenFile != null) ''
      TREESINGER_BEARER_TOKEN=$(< "$CREDENTIALS_DIRECTORY/bearer-token")
      export TREESINGER_BEARER_TOKEN
    ''}
    exec ${lib.getExe cfg.package} serve
  '';
in
{
  options.services.treesinger = {
    enable = lib.mkEnableOption "Treesinger, the authentication broker for Hytale dedicated servers";

    package = mkOption {
      type = types.package;
      default = pkgs.treesinger;
      defaultText = lib.literalExpression "treesinger.packages.\${system}.default";
      description = "The Treesinger package.";
    };

    user = mkOption {
      type = types.str;
      default = "treesinger";
      description = ''
        User the broker runs as; it owns the token store. Created as a system
        user only when left as `treesinger`: any other user must already exist,
        and own `stateDir`.
      '';
    };

    group = mkOption {
      type = types.str;
      default = "treesinger";
      description = ''
        The broker's primary group, and the group of its socket: whoever is in
        it can mint sessions. Created when left as `treesinger`. A client unit
        joins it with `SupplementaryGroups`.
      '';
    };

    stateDir = mkOption {
      type = types.str;
      default = defaultStateDir;
      example = "/data/servers/treesinger";
      description = ''
        Directory holding `tokens.json`, the one long-lived credential. The
        default is created by systemd (`StateDirectory=`, mode 0700). Any other
        path must already exist, owned by `user`: it is made writable for the
        broker but never created or chowned.
      '';
    };

    socket = mkOption {
      type = types.nullOr types.str;
      default = "/run/treesinger/treesinger.sock";
      description = ''
        Unix socket to listen on, mode 0660 with `group`, or null for none. The
        default's directory is the broker's `RuntimeDirectory`, kept across
        restarts, so a client may bind `/run/treesinger` into its own
        namespace. Any other directory must exist and be writable by `user`.
      '';
    };

    port = mkOption {
      type = types.nullOr types.port;
      default = null;
      example = 8080;
      description = "TCP port to listen on as well, or null for none.";
    };

    host = mkOption {
      type = types.str;
      default = "127.0.0.1";
      description = "Address the TCP listener binds, when `port` is set.";
    };

    openFirewall = mkOption {
      type = types.bool;
      default = false;
      description = "Open `port` in the firewall.";
    };

    ownerUuid = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Profile to mint sessions for when the account has several (`TREESINGER_OWNER_UUID`); null for the first.";
    };

    bearerTokenFile = mkOption {
      type = types.nullOr types.path;
      default = null;
      example = "/run/secrets/treesinger-bearer";
      description = ''
        File holding a shared secret that `POST /v1/session` then requires as
        `Authorization: Bearer`. Read by systemd (`LoadCredential=`), so it
        need not be readable by `user` and never enters the Nix store.
      '';
    };

    environment = mkOption {
      type = types.attrsOf types.str;
      default = { };
      example = { TREESINGER_SKEW_MS = "600000"; };
      description = "Extra environment for the broker (and `treesinger-login`).";
    };

    environmentFile = mkOption {
      type = types.nullOr types.path;
      default = null;
      description = "`EnvironmentFile=` for the broker and `treesinger-login`, for values kept out of the store.";
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.socket != null || cfg.port != null;
        message = "services.treesinger: set socket, port or both, or the broker listens on nothing.";
      }
      {
        assertion = cfg.openFirewall -> cfg.port != null;
        message = "services.treesinger.openFirewall needs services.treesinger.port.";
      }
    ];

    users.users = lib.optionalAttrs (cfg.user == "treesinger") {
      treesinger = {
        isSystemUser = true;
        group = cfg.group;
        home = cfg.stateDir;
        description = "Treesinger authentication broker";
      };
    };
    users.groups = lib.optionalAttrs (cfg.group == "treesinger") {
      treesinger = { };
    };

    networking.firewall.allowedTCPPorts = lib.optional (cfg.openFirewall && cfg.port != null) cfg.port;

    environment.systemPackages = [ login ];

    systemd.services.treesinger = {
      description = "Treesinger: Hytale server authentication broker";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" ];
      # systemd-notify comes from the default path, which includes systemd.
      environment = environment // listeners;

      serviceConfig = sandbox // {
        # Ready once every listener is bound; the notification comes from a
        # child (systemd-notify), hence NotifyAccess=all.
        Type = "notify";
        NotifyAccess = "all";
        ExecStart = serve;
        Restart = "on-failure";
        RestartSec = "5s";
        # The directory outlives a restart, so a client that bound it into its
        # namespace sees the new socket the broker makes inside it.
        RuntimeDirectory = "treesinger";
        RuntimeDirectoryMode = "0750";
        RuntimeDirectoryPreserve = true;
      } // lib.optionalAttrs (cfg.socket != null && dirOf cfg.socket != "/run/treesinger") {
        ReadWritePaths = (sandbox.ReadWritePaths or [ ]) ++ [ (dirOf cfg.socket) ];
      } // lib.optionalAttrs (cfg.bearerTokenFile != null) {
        LoadCredential = "bearer-token:${cfg.bearerTokenFile}";
      } // lib.optionalAttrs (cfg.environmentFile != null) {
        EnvironmentFile = cfg.environmentFile;
      };
    };
  };
}
