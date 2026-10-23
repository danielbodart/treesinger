# services.treesinger: the broker as a hardened, socket-activated systemd
# service. systemd holds its sockets: a client socket whose group is the
# access boundary (and optionally TCP), and a root-only admin socket for
# `sudo treesinger login`. See docs/systemd.md and docs/systemd-design.md.
{ config, lib, pkgs, ... }:

let
  cfg = config.services.treesinger;
  inherit (lib) mkOption mkIf types;

  defaultStateDir = "/var/lib/treesinger";
  managedStateDir = cfg.stateDir == defaultStateDir;
  inHome = lib.hasPrefix "/home/" cfg.stateDir || lib.hasPrefix "/root/" cfg.stateDir;

  # An absolute path as a string: a path literal (./bearer) would be copied
  # into the world-readable store the moment it is interpolated.
  secretPath = types.strMatching "/.*";

  # An IPv6 address is bracketed in a ListenStream=, as in a URL.
  tcp = if lib.hasInfix ":" cfg.host
    then "[${cfg.host}]:${toString cfg.port}"
    else "${cfg.host}:${toString cfg.port}";
in
{
  options.services.treesinger = {
    enable = lib.mkEnableOption "Treesinger, the authentication broker for Hytale dedicated servers";

    package = mkOption {
      type = types.package;
      default = pkgs.treesinger;
      defaultText = lib.literalExpression "treesinger.packages.\${system}.default";
      description = "The Treesinger package. Also put on the system path, for `sudo treesinger login`.";
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
        The broker's primary group, and the group of its client socket:
        whoever is in it can mint sessions. Created when left as `treesinger`.
        A client unit joins it with `SupplementaryGroups`.
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
        The client socket, held by `treesinger.socket`: mode 0660 with
        `group`, its directory made by systemd. It exists from early boot and
        outlives restarts of the broker. null for none, which needs `port`.
      '';
    };

    port = mkOption {
      type = types.nullOr types.port;
      default = null;
      example = 8080;
      description = "TCP port for `treesinger.socket` to listen on as well, or null for none.";
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
      type = types.nullOr secretPath;
      default = null;
      example = "/run/secrets/treesinger-bearer";
      description = ''
        File holding a shared secret that `POST /v1/session` then requires as
        `Authorization: Bearer`. Read by systemd (`LoadCredential=`), so it
        need not be readable by `user`, never enters the Nix store, and is
        never in the broker's environment.
      '';
    };

    environment = mkOption {
      type = types.attrsOf types.str;
      default = { };
      example = { TREESINGER_SKEW_MS = "600000"; };
      description = "Extra environment for the broker.";
    };

    environmentFile = mkOption {
      type = types.nullOr secretPath;
      default = null;
      description = "`EnvironmentFile=` for the broker, for values kept out of the store.";
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.socket != null || cfg.port != null;
        message = "services.treesinger: set socket, port or both, or the broker listens on nothing.";
      }
      {
        assertion = cfg.socket == null || lib.hasPrefix "/" cfg.socket;
        message = "services.treesinger.socket must be an absolute path.";
      }
      {
        assertion = cfg.openFirewall -> cfg.port != null;
        message = "services.treesinger.openFirewall needs services.treesinger.port.";
      }
      {
        # The sockets are the listeners; the broker refuses to start with these.
        assertion = !(cfg.environment ? TREESINGER_PORT || cfg.environment ? TREESINGER_HOST);
        message = "services.treesinger: set port and host, not TREESINGER_PORT or TREESINGER_HOST in environment.";
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

    # `sudo treesinger login` logs the running broker in through its admin
    # socket; `treesinger exec` is a client unit's ExecStart.
    environment.systemPackages = [ cfg.package ];

    # What is enabled: the service starts on the first connection, and a
    # connection made while it starts or restarts waits rather than fails.
    systemd.sockets.treesinger = {
      description = "Treesinger: Hytale server authentication broker (client socket)";
      wantedBy = [ "sockets.target" ];
      listenStreams = lib.optional (cfg.socket != null) cfg.socket
        ++ lib.optional (cfg.port != null) tcp;
      socketConfig = {
        SocketGroup = cfg.group;
        SocketMode = "0660";
        # The socket's directory, made by systemd; it only ever holds the
        # socket, so a confined client can bind it or the socket file itself.
        DirectoryMode = "0755";
      } // lib.optionalAttrs (cfg.port != null) {
        # Sockets start early in boot, perhaps before `host` is configured.
        FreeBind = true;
      };
    };

    # Root only, and outside the client socket's directory on purpose: that is
    # bind-mounted into confined game servers.
    systemd.sockets.treesinger-admin = {
      description = "Treesinger: Hytale server authentication broker (admin socket)";
      wantedBy = [ "sockets.target" ];
      listenStreams = [ "/run/treesinger-admin.sock" ];
      socketConfig = {
        SocketMode = "0600";
        FileDescriptorName = "admin";
        Service = "treesinger.service";
      };
    };

    systemd.services.treesinger = {
      description = "Treesinger: Hytale server authentication broker";
      requires = [ "treesinger.socket" "treesinger-admin.socket" ];
      # Clients mint their sessions as soon as this is ready, once, at boot:
      # wait until Hypixel can be reached, and so make them wait too.
      wants = [ "network-online.target" ];
      after = [ "treesinger.socket" "treesinger-admin.socket" "network-online.target" ];
      environment = {
        TREESINGER_STORE = "${cfg.stateDir}/tokens.json";
      } // lib.optionalAttrs (cfg.ownerUuid != null) {
        TREESINGER_OWNER_UUID = cfg.ownerUuid;
      } // cfg.environment;

      serviceConfig = {
        # Ready once serving; the broker sends the notification itself.
        Type = "notify";
        NotifyAccess = "main";
        ExecStart = "${lib.getExe cfg.package} serve";
        Restart = "on-failure";
        RestartSec = "5s";

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
        # unix for the sockets and the readiness datagram; inet for HTTPS to
        # Hypixel (and the TCP listener, when there is one).
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        # Not ~@resources: the Go runtime raises its open-file limit
        # (setrlimit) as it starts, and a filtered call is SIGSYS.
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
      }) // lib.optionalAttrs (cfg.bearerTokenFile != null) {
        # The broker reads $CREDENTIALS_DIRECTORY/bearer-token itself.
        LoadCredential = "bearer-token:${cfg.bearerTokenFile}";
      } // lib.optionalAttrs (cfg.environmentFile != null) {
        EnvironmentFile = cfg.environmentFile;
      };
    };
  };
}
