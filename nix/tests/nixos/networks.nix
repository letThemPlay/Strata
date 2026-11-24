# NixOS VM test: guest networks (ADR-0019).
#
#   admin (VLAN 1)  ── management ──  hv01 eth1 (192.168.1.2)
#                                     hv01 eth2 ── untagged ──┐
#                                     hv01 eth3 ── trunk ─────┤ test VLAN 2
#   peer eth1 (192.168.2.3, untagged; eth1.20 10.20.0.1; eth1.30 10.30.0.1)
#
# Guest networks on hv01: lan (eth2, untagged), v20 (VLAN 20 on eth3),
# lab (isolated). The "moved" specialisation moves lan to VLAN 30 on eth3
# and removes lab.
{ self, pkgs }:
let
  testImage = import ./lib/test-image.nix { inherit pkgs; };
  baseNetworks = {
    lan.uplink = "eth2";
    v20 = {
      uplink = "eth3";
      vlan = 20;
    };
    lab = { };
  };
in
pkgs.testers.runNixOSTest {
  name = "strata-networks";

  nodes = {
    hv01 =
      { lib, pkgs, ... }:
      {
        imports = [ self.nixosModules.default ];
        environment.systemPackages = [ pkgs.jq ];

        virtualisation = {
          vlans = [
            1
            2
            2
          ];
          cores = 2;
          memorySize = 2048;
          diskSize = 4096;
          emptyDiskImages = [
            {
              size = 1024;
              driveConfig.deviceExtraOpts.serial = "pool-dir";
            }
          ];
          fileSystems."/srv/pool" = {
            device = "/dev/disk/by-id/virtio-pool-dir";
            fsType = "ext4";
            autoFormat = true;
          };
        };
        # The test framework addresses every interface; uplinks must have none.
        networking.interfaces = lib.genAttrs [ "eth2" "eth3" ] (_: {
          ipv4.addresses = lib.mkForce [ ];
          ipv6.addresses = lib.mkForce [ ];
        });

        strata = {
          enable = true;
          node.id = "hv01";
          agent.enable = true;
          compute.enable = true;
          storage = {
            defaultPool = "test";
            pools.test = {
              driver = "dir";
              path = "/srv/pool";
            };
          };
          networking = {
            enable = true;
            management = {
              interface = "eth1";
              addresses = [ "192.168.1.2/24" ];
            };
            guest = {
              defaultNetwork = "lan";
              networks = baseNetworks;
            };
          };
        };

        specialisation.moved.configuration.strata.networking.guest.networks = lib.mkForce {
          lan = {
            uplink = "eth3";
            vlan = 30;
          };
          inherit (baseNetworks) v20;
        };
      };

    peer = {
      virtualisation.vlans = [ 2 ];
      networking = {
        vlans = {
          "eth1.20" = {
            id = 20;
            interface = "eth1";
          };
          "eth1.30" = {
            id = 30;
            interface = "eth1";
          };
        };
        interfaces = {
          "eth1.20".ipv4.addresses = [
            {
              address = "10.20.0.1";
              prefixLength = 24;
            }
          ];
          "eth1.30".ipv4.addresses = [
            {
              address = "10.30.0.1";
              prefixLength = 24;
            }
          ];
        };
        firewall.enable = false;
      };
    };

    admin =
      { pkgs, ... }:
      {
        virtualisation.vlans = [ 1 ];
        environment.systemPackages = [ pkgs.netcat ];
      };
  };

  testScript = ''
    import json

    start_all()
    hv01.wait_for_unit("strata-incus-setup.service")
    hv01.wait_for_unit("strata-agent.service")
    peer.wait_for_unit("network.target")
    base = hv01.succeed("readlink -f /run/current-system").strip()

    hv01.succeed(
        "incus image import ${testImage}/metadata.tar.xz ${testImage}/rootfs.tar.xz --alias test --project strata",
        "mkdir -p /tmp/agent/servercerts",
        "cp /var/lib/strata-incus/client.crt /var/lib/strata-incus/client.key /tmp/agent/",
        "cp /var/lib/incus/server.crt /tmp/agent/servercerts/hv01.crt",
        "printf 'default-remote: hv01\\nremotes:\\n  hv01:\\n    addr: https://127.0.0.1:8443\\n"
        "    protocol: incus\\n    auth_type: tls\\n    project: strata\\n' > /tmp/agent/config.yml",
    )
    agent = "INCUS_CONF=/tmp/agent incus"

    def inside(name, command):
        return f"INCUS_CONF=/tmp/agent timeout 30 incus exec {name} --disable-stdin --force-interactive -- {command}"

    def launch(name, network, address):
        hv01.succeed(f"{agent} launch test {name} --network {network}")
        hv01.wait_until_succeeds(inside(name, "/bin/true"), timeout=60)
        hv01.succeed(inside(name, f"ip addr add {address} dev eth0"), inside(name, "ip link set eth0 up"))

    def reaches(name, target):
        return hv01.execute(inside(name, f"ping -c 2 -W 2 {target}"))[0] == 0

    def refused(command, message):
        out = hv01.fail(f"{agent} {command} 2>&1")
        assert message in out, f"{command}: expected {message!r}, got {out!r}"

    with subtest("declared networks are bridges without host addresses"):
        nets = {n["name"]: n for n in json.loads(hv01.succeed("incus query '/1.0/networks?recursion=1'"))}
        for name, ext in [("lan", "eth2"), ("v20", "eth3.20/eth3/20"), ("lab", None)]:
            n = nets[name]
            assert n["managed"] and n["type"] == "bridge", n
            assert n["config"]["ipv4.address"] == "none" and n["config"]["ipv6.address"] == "none", n
            assert n["config"].get("bridge.external_interfaces") == ext, n
        # No interface but loopback, the test driver's eth0 and management
        # has an address: not the uplinks, VLAN interfaces or bridges.
        addrs = hv01.succeed("ip -o addr show | grep -vE '^[0-9]+: (lo|eth0|eth1) ' || true").strip()
        assert addrs == "", f"host addresses on guest-facing interfaces:\n{addrs}"
        cfg = json.loads(hv01.succeed("incus query /1.0/projects/strata"))["config"]
        assert cfg["restricted.networks.access"] == "lab,lan,v20", cfg
        profile = json.loads(hv01.succeed("incus query '/1.0/profiles/default?project=strata'"))
        assert profile["devices"]["eth0"] == {"type": "nic", "name": "eth0", "network": "lan"}, profile

    with subtest("management is unaffected"):
        admin.wait_until_succeeds("ping -c 1 -W 2 192.168.1.2")
        hv01.succeed("ip -o addr show dev eth1 | grep -F 192.168.1.2/24")

    with subtest("guests reach their own network and nothing else"):
        launch("c-lan", "lan", "192.168.2.50/24")
        assert reaches("c-lan", "192.168.2.3"), "lan guest cannot reach peer"
        launch("c-v20", "v20", "10.20.0.50/24")
        assert reaches("c-v20", "10.20.0.1"), "v20 guest cannot reach peer on VLAN 20"
        hv01.succeed(inside("c-v20", "ip addr add 192.168.2.51/24 dev eth0"))
        assert not reaches("c-v20", "192.168.2.3"), "v20 guest reached the untagged network"
        launch("c-lab1", "lab", "10.99.0.1/24")
        launch("c-lab2", "lab", "10.99.0.2/24")
        assert reaches("c-lab1", "10.99.0.2"), "isolated guests cannot reach each other"
        hv01.succeed(inside("c-lab1", "ip addr add 192.168.2.52/24 dev eth0"))
        assert not reaches("c-lab1", "192.168.2.3"), "isolated guest reached the uplink"

    with subtest("guests have no path to the host"):
        # The host has no address on guest networks: nothing answers.
        for target in ["192.168.2.2", "192.168.1.2"]:
            assert not reaches("c-lan", target), f"guest reached {target}"

    with subtest("a guest cannot tag its way onto another VLAN"):
        hv01.succeed(f"{agent} init test c-hop --network lan")
        code, out = hv01.execute(f"{agent} config device set c-hop eth0 vlan=20 2>&1")
        if code == 0:
            hv01.succeed(f"{agent} start c-hop")
            hv01.wait_until_succeeds(inside("c-hop", "/bin/true"), timeout=60)
            hv01.succeed(inside("c-hop", "ip addr add 10.20.0.60/24 dev eth0"), inside("c-hop", "ip link set eth0 up"))
            assert not reaches("c-hop", "10.20.0.1"), "VLAN HOPPING: a lan guest with vlan=20 reached VLAN 20"
            # Positive control: without the tag, the same guest works.
            hv01.succeed(f"{agent} stop --force c-hop", f"{agent} config device unset c-hop eth0 vlan", f"{agent} start c-hop")
            hv01.wait_until_succeeds(inside("c-hop", "/bin/true"), timeout=60)
            hv01.succeed(inside("c-hop", "ip addr add 192.168.2.60/24 dev eth0"), inside("c-hop", "ip link set eth0 up"))
            assert reaches("c-hop", "192.168.2.3"), "positive control failed: c-hop has no connectivity at all"
            print("VLAN tag on an untagged network: allowed, but no hopping")
        else:
            print(f"VLAN tag on an untagged network refused: {out}")

    with subtest("the agent identity is refused undeclared networks and host interfaces"):
        hv01.succeed("incus network create manual ipv4.address=none ipv6.address=none")
        hv01.succeed("systemctl restart strata-incus-setup")
        code, out = hv01.execute(f"{agent} init test x1 --network manual 2>&1")
        assert code != 0 and ("Network not allowed in project" in out or 'not found' in out), out
        hv01.succeed("incus network show manual")  # it exists; the identity may not use it
        hv01.succeed(f"{agent} init test x2 --no-profiles -s test")
        refused("config device add x2 m nic nictype=macvlan parent=eth1", "Only managed network devices are allowed")
        # Networks live in the default project, which the identity cannot use.
        refused("network create mine", 'User does not have permission for project "default"')

    with subtest("networkd leaves Incus bridges alone"):
        hv01.succeed("networkctl reload")
        hv01.succeed("sleep 2")
        links = hv01.succeed("bridge -o link show")
        print(links)
        assert any(l.split(":")[1].strip().startswith("eth2") and "master lan" in l for l in links.splitlines()), links
        assert reaches("c-lan", "192.168.2.3")

    with subtest("a declared change is applied; a removed network stays while used"):
        code, out = hv01.execute(f"{base}/specialisation/moved/bin/switch-to-configuration test 2>&1")
        print(out)
        nets = {n["name"]: n for n in json.loads(hv01.succeed("incus query '/1.0/networks?recursion=1'"))}
        assert nets["lan"]["config"]["bridge.external_interfaces"] == "eth3.30/eth3/30", nets["lan"]
        hv01.succeed(inside("c-lan", "ip addr add 10.30.0.50/24 dev eth0"))
        assert reaches("c-lan", "10.30.0.1"), "lan guest cannot reach VLAN 30 after the move"
        cfg = json.loads(hv01.succeed("incus query /1.0/projects/strata"))["config"]
        assert cfg["restricted.networks.access"] == "lab,lan,v20", cfg  # lab still used
        hv01.succeed(f"{agent} delete --force c-lab1 c-lab2")
        hv01.succeed("systemctl restart strata-incus-setup")
        cfg = json.loads(hv01.succeed("incus query /1.0/projects/strata"))["config"]
        assert cfg["restricted.networks.access"] == "lan,v20", cfg
        hv01.succeed("incus network show lab")  # never deleted

    with subtest("networks and guest connectivity survive a reboot"):
        hv01.shutdown()
        hv01.start()
        hv01.wait_for_unit("strata-incus-setup.service")
        hv01.wait_until_succeeds(inside("c-lan", "/bin/true"), timeout=60)
        hv01.succeed(inside("c-lan", "ip addr add 192.168.2.50/24 dev eth0"), inside("c-lan", "ip link set eth0 up"))
        assert reaches("c-lan", "192.168.2.3"), "lan guest cannot reach peer after reboot"
        failed = hv01.succeed("systemctl list-units --failed --plain --no-legend").strip()
        assert failed == "", failed
  '';
}
