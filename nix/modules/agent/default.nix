# Strata node agent service.
#
# The agent currently only performs hardware discovery, so it runs as an
# unprivileged dynamic user with no network access and no capabilities
# (ADR-0007). Privileges are granted explicitly when a feature needs them.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.strata;
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

    systemd.services.strata-agent = {
      description = "Strata node agent";
      wantedBy = [ "multi-user.target" ];
      after = [ "local-fs.target" ];

      serviceConfig = {
        Type = "simple";
        ExecStart = "${lib.getExe' cfg.package "strata-agent"} run --state-dir /var/lib/strata";
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
        # RestrictAddressFamilies= and IPAddressDeny= instead.
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        NoNewPrivileges = true;
        RestrictAddressFamilies = [ "AF_UNIX" ];
        IPAddressDeny = "any";
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
