# Guest networks (ADR-0019).
#
# Each declared network becomes an Incus bridge, created by
# strata-incus-setup: bridged to an uplink NIC (untagged, or a VLAN on it)
# or isolated (no uplink). The host has no address on guest networks, and
# an uplink is never the management interface, so guests have no path to
# the host's management services.
#
# Host effects: uplinks are brought up by systemd-networkd without any
# address, and guest bridges are exempt from the reverse-path filter
# (bridged traffic passes the host's nftables hooks through br_netfilter).
{ config, lib, ... }:
let
  cfg = config.strata;
  g = cfg.networking.guest;
  mgmtIf = cfg.networking.management.interface;
  nets = lib.attrValues g.networks;
  ifName = lib.types.strMatching "[a-zA-Z0-9_-]{1,15}" // {
    description = "network interface name (1-15 characters, no dots)";
  };

  uplinks = lib.unique (lib.filter (u: u != null) (map (n: n.uplink) nets));
  onUplink = u: lib.filter (n: n.uplink == u) nets;
  untaggedOn = u: lib.filter (n: n.vlan == null) (onUplink u);
  vlansOn = u: map (n: n.vlan) (lib.filter (n: n.vlan != null) (onUplink u));

  vlanIfs = map (n: "${n.uplink}.${toString n.vlan}") (lib.filter (n: n.vlan != null) nets);
  noAddress = {
    DHCP = "no";
    LinkLocalAddressing = "no";
    IPv6AcceptRA = false;
    KeepMaster = true;
  };

  netModule =
    { name, ... }:
    {
      options = {
        name = lib.mkOption {
          type = ifName;
          default = name;
          readOnly = true;
          description = "Network name; also the bridge's interface name.";
        };
        uplink = lib.mkOption {
          type = lib.types.nullOr ifName;
          default = null;
          example = "enp3s0";
          description = ''
            Host NIC the network is bridged to. Never the management
            interface. null: an isolated network (guests on this host only).
          '';
        };
        vlan = lib.mkOption {
          type = lib.types.nullOr (lib.types.ints.between 1 4094);
          default = null;
          description = "VLAN ID on the uplink (tagged). null: untagged.";
        };
      };
    };
in
{
  options.strata.networking.guest = {
    networks = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule netModule);
      default = { };
      description = "Guest networks (Incus bridges).";
    };
    defaultNetwork = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Network for eth0 of new instances (the project's default profile).";
    };
  };

  config = lib.mkIf (nets != [ ] || g.defaultNetwork != null) {
    assertions = [
      {
        assertion = cfg.compute.enable && cfg.networking.enable;
        message = "strata.networking.guest requires strata.compute.enable and strata.networking.enable.";
      }
      {
        assertion = g.defaultNetwork == null || g.networks ? ${g.defaultNetwork};
        message = "strata.networking.guest.defaultNetwork \"${toString g.defaultNetwork}\" is not a declared network.";
      }
      {
        assertion = !(lib.elem mgmtIf uplinks);
        message = "strata.networking.guest: the management interface (${toString mgmtIf}) cannot be a guest uplink.";
      }
      {
        assertion = !(lib.elem mgmtIf (lib.attrNames g.networks));
        message = "strata.networking.guest: a network cannot be named like the management interface.";
      }
      {
        assertion = !(g.networks ? strata-none);
        message = "strata.networking.guest: the network name \"strata-none\" is reserved.";
      }
    ]
    ++ lib.concatMap (u: [
      {
        assertion = untaggedOn u == [ ] || (lib.length (onUplink u) == 1);
        message = "strata.networking.guest: uplink ${u} is used untagged by ${
          lib.concatMapStringsSep ", " (n: n.name) (untaggedOn u)
        }; an uplink is either untagged for one network or carries VLANs, not both.";
      }
      {
        assertion = lib.length (vlansOn u) == lib.length (lib.unique (vlansOn u));
        message = "strata.networking.guest: a VLAN ID is used twice on uplink ${u}.";
      }
    ]) uplinks
    ++ map (n: {
      assertion = n.vlan == null || lib.stringLength "${toString n.uplink}.${toString n.vlan}" <= 15;
      message = "strata.networking.guest.networks.${n.name}: ${toString n.uplink}.${toString n.vlan} is longer than 15 characters.";
    }) nets
    ++ map (n: {
      assertion = n.vlan == null || n.uplink != null;
      message = "strata.networking.guest.networks.${n.name}: a VLAN needs an uplink.";
    }) nets;

    # Uplinks are up, with no address of any kind: the host is not on guest
    # networks. Incus attaches them (or VLAN interfaces on them) to bridges;
    # KeepMaster stops networkd from detaching them.
    systemd.network.networks =
      lib.listToAttrs (
        map (
          u:
          lib.nameValuePair "20-strata-uplink-${u}" {
            matchConfig.Name = u;
            networkConfig = noAddress // {
              ConfigureWithoutCarrier = true;
            };
            linkConfig = {
              RequiredForOnline = "no";
              ActivationPolicy = "always-up";
            };
          }
        ) uplinks
      )
      # The VLAN interfaces Incus creates would otherwise get an IPv6
      # link-local address from the kernel. Their link state stays with
      # Incus.
      // lib.optionalAttrs (vlanIfs != [ ]) {
        "21-strata-guest-vlans" = {
          matchConfig.Name = lib.concatStringsSep " " vlanIfs;
          networkConfig = noAddress;
          linkConfig = {
            RequiredForOnline = "no";
            ActivationPolicy = "manual";
          };
        };
      };

    # Bridged guest traffic passes nftables prerouting (br_netfilter); the
    # strict reverse-path check would drop it.
    networking.firewall.extraReversePathFilterRules = lib.mkIf (nets != [ ]) ''
      iifname { ${lib.concatMapStringsSep ", " (n: "\"${n.name}\"") nets} } accept
    '';
  };
}
