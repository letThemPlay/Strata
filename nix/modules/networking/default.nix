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

  # Interfaces pinned to their permanent MAC address (ADR-0011): the name
  # stays the same whatever the PCI layout.
  pins = ncfg.interfaces;
  macs = lib.mapAttrsToList (_: p: p.macAddress) pins;
  duplicateMacs = lib.unique (lib.filter (m: lib.count (x: x == m) macs > 1) macs);
  # networkd units match a pinned interface by MAC address, so they work
  # under any name; otherwise by name.
  matchFor =
    name:
    if pins ? ${name} then { PermanentMACAddress = pins.${name}.macAddress; } else { Name = name; };
  pinLinks = lib.mapAttrs' (
    name: p:
    lib.nameValuePair "10-strata-${name}" {
      matchConfig.PermanentMACAddress = p.macAddress;
      linkConfig.Name = name;
    }
  ) pins;

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

    interfaces = mkOption {
      type = types.attrsOf (
        types.submodule {
          options.macAddress = mkOption {
            type = types.strMatching "([0-9a-f]{2}:){5}[0-9a-f]{2}";
            example = "84:47:09:a3:5d:dc";
            description = "Permanent MAC address of the NIC (lowercase; `ethtool -P IFACE`).";
          };
        }
      );
      default = { };
      example = {
        eno1.macAddress = "84:47:09:a3:5d:dc";
      };
      description = ''
        Interface names bound to the NIC with this permanent MAC address. The
        NIC gets this name at every boot, whatever the PCI layout, so adding
        or moving hardware cannot rename it. Pin the management interface
        and guest uplinks; strata-install pins the management interface.
        A name other than the NIC's current one takes effect after a reboot.
      '';
    };

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

  config = lib.mkMerge [
    (mkIf (cfg.enable && pins != { }) {
      assertions = [
        {
          assertion = duplicateMacs == [ ];
          message = "strata.networking.interfaces: MAC addresses pinned twice: ${lib.concatStringsSep ", " duplicateMacs}";
        }
      ];
      # Applied by udev at boot, in the initrd and in the system, while the
      # interfaces are still down.
      systemd.network.links = pinLinks;
      boot.initrd.systemd.network.links = pinLinks;
    })

    (mkIf ncfg.enable {
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

      warnings = lib.optional (mgmt.interface != null && !(pins ? ${mgmt.interface})) (
        "strata.networking.management.interface ${mgmt.interface} is not pinned to its MAC address: "
        + "adding or moving hardware may rename it and cut the host off its management network. "
        + "Add strata.networking.interfaces.${mgmt.interface}.macAddress (ethtool -P ${mgmt.interface})."
      );

      systemd.network.networks."10-strata-management" = mkIf (mgmt.interface != null) {
        matchConfig = matchFor mgmt.interface;
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
    })
  ];
}
