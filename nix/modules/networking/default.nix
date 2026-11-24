# Management network (systemd-networkd) and host firewall (nftables).
#
# Management services (SSH, node exporter) are reachable only through the
# management interface. No port is open on any other interface.
{ config, lib, ... }:
let
  cfg = config.strata;
  ncfg = cfg.networking;
  mgmt = ncfg.management;
  inherit (lib)
    mkEnableOption
    mkIf
    mkOption
    types
    ;

  ipv4 = "([0-9]{1,3}\\.){3}[0-9]{1,3}";
  ipv6 = "[0-9a-fA-F:]*:[0-9a-fA-F:.]*";
  ipAddress = types.strMatching "(${ipv4}|${ipv6})" // {
    description = "IPv4 or IPv6 address";
  };
  cidr = types.strMatching "(${ipv4}/[0-9]{1,2}|${ipv6}/[0-9]{1,3})" // {
    description = "IP address with prefix length (CIDR)";
  };
  ifName = types.strMatching "[a-zA-Z0-9_.-]{1,15}" // {
    description = "network interface name (1-15 characters)";
  };

  isStatic = mgmt.addressing == "static";

  fw = config.networking.firewall;
  globalOpen =
    map (p: "tcp/${toString p}") fw.allowedTCPPorts
    ++ map (p: "udp/${toString p}") fw.allowedUDPPorts
    ++ map (r: "tcp/${toString r.from}-${toString r.to}") fw.allowedTCPPortRanges
    ++ map (r: "udp/${toString r.from}-${toString r.to}") fw.allowedUDPPortRanges;

  managementPorts =
    lib.optionals cfg.access.enable config.services.openssh.ports
    ++ lib.optional cfg.monitoring.enable cfg.monitoring.nodeExporter.port;
in
{
  imports = [ ./guest.nix ];

  options.strata.networking = {
    enable = mkEnableOption "Strata management networking and host firewall";

    management = {
      interface = mkOption {
        type = types.nullOr ifName;
        default = null;
        example = "eno1";
        description = "Interface connected to the management network. Required.";
      };

      addressing = mkOption {
        type = types.enum [
          "static"
          "dhcp"
        ];
        default = "static";
        description = "How the management interface is addressed.";
      };

      addresses = mkOption {
        type = types.listOf cidr;
        default = [ ];
        example = [ "192.0.2.10/24" ];
        description = "Static addresses (CIDR). Required with static addressing; not allowed with DHCP.";
      };

      gateway4 = mkOption {
        type = types.nullOr ipAddress;
        default = null;
        example = "192.0.2.1";
        description = "IPv4 default gateway (static addressing only).";
      };

      gateway6 = mkOption {
        type = types.nullOr ipAddress;
        default = null;
        description = "IPv6 default gateway (static addressing only).";
      };

      dns = mkOption {
        type = types.listOf ipAddress;
        default = [ ];
        description = "DNS servers (static addressing only).";
      };
    };
  };

  config = mkIf ncfg.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.networking.enable requires strata.enable.";
      }
      {
        assertion = mgmt.interface != null;
        message = "strata.networking.management.interface must be set.";
      }
      {
        assertion = isStatic -> mgmt.addresses != [ ];
        message = "strata.networking.management.addresses must not be empty with static addressing.";
      }
      {
        assertion = globalOpen == [ ];
        message = "strata.networking: ports must not be opened on all interfaces (management isolation). Open them per interface via networking.firewall.interfaces instead. Globally open: ${lib.concatStringsSep ", " globalOpen}";
      }
      {
        assertion =
          !isStatic
          -> (mgmt.addresses == [ ] && mgmt.gateway4 == null && mgmt.gateway6 == null && mgmt.dns == [ ]);
        message = "strata.networking.management: addresses, gateways and dns are only allowed with static addressing.";
      }
    ];

    networking = {
      useNetworkd = true;
      useDHCP = false;

      nftables.enable = true;
      firewall = {
        enable = true;
        interfaces = mkIf (mgmt.interface != null) {
          ${mgmt.interface}.allowedTCPPorts = managementPorts;
        };
      };
    };

    systemd.network.networks."10-strata-management" = mkIf (mgmt.interface != null) {
      matchConfig.Name = mgmt.interface;
      networkConfig = {
        DHCP = if isStatic then "no" else "yes";
      }
      // lib.optionalAttrs (isStatic && mgmt.dns != [ ]) { DNS = mgmt.dns; };
      address = lib.optionals isStatic mgmt.addresses;
      gateway = lib.optionals isStatic (
        lib.filter (g: g != null) [
          mgmt.gateway4
          mgmt.gateway6
        ]
      );
      linkConfig.RequiredForOnline = "routable";
    };
  };
}
