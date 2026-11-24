# NixOS VM test: interface names pinned to MAC addresses (ADR-0011).
#
# hv01 and hv02 have the same two NICs in opposite order, so the kernel
# names them the other way round (as adding a device did on real
# hardware). Both pin the management NIC (test VLAN 1) as mgmt0 and the
# guest uplink (test VLAN 2) as uplink0 by MAC address; each must end up
# with the right name on the right network.
#
#   admin (VLAN 1, 192.168.1.1) -- management -- hv01 mgmt0 (.2), hv02 mgmt0 (.3)
#
# Node numbers follow the sorted names: admin=1, hv01=2, hv02=3. The test
# framework's NIC MAC is 52:54:00:12:<vlan>:<node>.
{ self, pkgs }:
let
  keys = import (pkgs.path + "/nixos/tests/ssh-keys.nix") pkgs;
  host =
    { node, vlans }:
    _: {
      imports = [ self.nixosModules.default ];
      virtualisation = {
        inherit vlans;
        cores = 2;
        memorySize = 1536;
      };
      strata = {
        enable = true;
        node.id = "hv0${toString (node - 1)}";
        agent.enable = true;
        compute.enable = true;
        access = {
          enable = true;
          admin.authorizedKeys = [ keys.snakeOilEd25519PublicKey ];
        };
        monitoring.enable = true;
        networking = {
          enable = true;
          interfaces = {
            mgmt0.macAddress = "52:54:00:12:01:0${toString node}";
            uplink0.macAddress = "52:54:00:12:02:0${toString node}";
          };
          management = {
            interface = "mgmt0";
            addresses = [ "192.168.1.${toString node}/24" ];
          };
          guest.networks.lan.uplink = "uplink0";
        };
      };
    };
in
pkgs.testers.runNixOSTest {
  name = "strata-nic-names";

  nodes = {
    hv01 = host {
      node = 2;
      vlans = [
        1
        2
      ];
    };
    hv02 = host {
      node = 3;
      vlans = [
        2
        1
      ];
    };
    admin =
      { pkgs, ... }:
      {
        virtualisation.vlans = [ 1 ];
        environment.systemPackages = [ pkgs.curl ];
      };
  };

  testScript = ''
    start_all()
    admin.succeed("install -D -m 600 ${keys.snakeOilEd25519PrivateKey} /root/.ssh/id_ed25519")

    def slot(machine, name):
        """The PCI device path of an interface (its position on the bus)."""
        return machine.succeed(f"readlink -f /sys/class/net/{name}/device").strip()

    for m, addr, first in [(hv01, "192.168.1.2", "mgmt0"), (hv02, "192.168.1.3", "uplink0")]:
        with subtest(f"{m.name}: pinned names follow the MAC address, not the NIC order"):
            m.wait_for_unit("strata-incus-setup.service")
            m.wait_for_unit("sshd.service")
            names = m.succeed("ls /sys/class/net").split()
            assert "mgmt0" in names and "uplink0" in names, names
            assert "eth1" not in names and "eth2" not in names, names
            assert m.succeed("cat /sys/class/net/mgmt0/address").strip().startswith("52:54:00:12:01:")
            assert m.succeed("cat /sys/class/net/uplink0/address").strip().startswith("52:54:00:12:02:")
            # The kernel saw the NICs in opposite order on the two hosts.
            ordered = sorted(["mgmt0", "uplink0"], key=lambda n: slot(m, n))
            assert ordered[0] == first, (m.name, ordered)
            # Unpinned NICs keep their kernel name.
            assert "eth0" in names, names

        with subtest(f"{m.name}: management works on the pinned name"):
            status = m.succeed("networkctl status mgmt0")
            assert "10-strata-management.network" in status and addr in status, status
            assert 'iifname "mgmt0"' in m.succeed("nft list ruleset")
            admin.wait_until_succeeds(f"ping -c 1 -W 2 {addr}")
            out = admin.succeed(
                "timeout 60 ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes"
                f" strata-admin@{addr} 'sudo -n true && echo ok'"
            ).strip()
            assert out == "ok", out
            admin.succeed(f"curl -sf http://{addr}:9100/metrics > /dev/null")

        with subtest(f"{m.name}: the guest network uses the pinned uplink"):
            links = m.succeed("bridge -o link show")
            assert any(l.split(":")[1].strip().startswith("uplink0") and "master lan" in l for l in links.splitlines()), links
            assert m.succeed("ip -o addr show dev uplink0").strip() == "", "the host must have no address on the uplink"
            m.succeed("journalctl -u strata-incus-setup -b | grep -q 'incus setup complete'")
            failed = m.succeed("systemctl list-units --failed --plain --no-legend").strip()
            assert failed == "", failed
  '';
}
