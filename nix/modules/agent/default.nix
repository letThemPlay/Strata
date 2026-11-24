# Strata node agent service.
#
# The agent runs as an unprivileged dynamic user with no capabilities
# (ADR-0007). Privileges are granted explicitly when a feature needs them:
# on a compute host it may reach the Incus API over loopback (and nothing
# else), with its restricted identity passed as systemd credentials
# (ADR-0017). It never gets the Incus Unix socket.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.strata;
  incus = cfg.compute.enable;
  incusArgs = lib.optionalString incus (
    " --incus-address ${cfg.compute.incus.address}"
    + " --incus-project ${cfg.compute.incus.project}"
    + " --incus-credentials %d"
    + " --api-group strata-operators"
  );
in
{
  options.strata.agent = {
    enable = lib.mkEnableOption "the Strata node agent";
  };

  config = lib.mkIf cfg.agent.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.agent.enable requires strata.enable.";
      }
    ];

    # The local API (ADR-0020): systemd owns the socket, so the agent needs
    # no privilege to create it. Members of strata-operators may connect.
    users.groups.strata-operators = lib.mkIf incus { };
    systemd.sockets.strata-agent = lib.mkIf incus {
      description = "Strata agent local API";
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "/run/strata/agent.sock";
        SocketUser = "root";
        SocketGroup = "strata-operators";
        SocketMode = "0660";
        DirectoryMode = "0755";
        Service = "strata-agent.service";
      };
    };

    systemd.services.strata-agent = {
      description = "Strata node agent";
      wantedBy = [ "multi-user.target" ];
      after = [
        "local-fs.target"
      ]
      ++ lib.optionals incus [
        "strata-incus-setup.service"
        "strata-agent.socket"
      ];
      wants = lib.optional incus "strata-incus-setup.service";
      requires = lib.optional incus "strata-agent.socket";

      serviceConfig = {
        Type = "simple";
        ExecStart = "${lib.getExe' cfg.package "strata-agent"} run --state-dir /var/lib/strata --interval 1m${incusArgs}";
        ExecReload = "${pkgs.coreutils}/bin/kill -HUP $MAINPID";
        Restart = "on-failure";
        RestartSec = 5;

        DynamicUser = true;
        StateDirectory = "strata";
        StateDirectoryMode = "0755";
        UMask = "0022";

        # Hardening. /dev, /proc and /sys must stay readable for discovery,
        # so PrivateDevices= and ProcSubset=pid are intentionally not set.
        # PrivateNetwork= is not used either: it would hide the host's
        # interfaces from /sys/class/net. Network access is denied through
        # RestrictAddressFamilies= and IPAddressDeny= instead; on a compute
        # host only loopback is allowed (the Incus API).
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        NoNewPrivileges = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
        ]
        ++ lib.optionals incus [
          "AF_INET"
          "AF_INET6"
        ];
        Sockets = lib.mkIf incus [ "strata-agent.socket" ];
        IPAddressDeny = "any";
        IPAddressAllow = lib.mkIf incus "localhost";
        LoadCredential = lib.mkIf incus [
          "incus-client.crt:/var/lib/strata-incus/client.crt"
          "incus-client.key:/var/lib/strata-incus/client.key"
          "incus-server.crt:/var/lib/incus/server.crt"
        ];
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectProc = "invisible";
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
          "~@resources"
        ];
      };
    };
  };
}
