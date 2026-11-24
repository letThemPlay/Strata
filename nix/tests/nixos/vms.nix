# NixOS VM test: VM lifecycle through the strata CLI (M2 slice 4,
# ADR-0020, ADR-0021): create, start/stop/restart, update with a plan,
# delete. Needs nested virtualization: hv01 runs a NixOS guest under Incus.
#
#   hv01 eth1 (management, VLAN 1); eth2 uplink of guest network "lan" (VLAN 2)
#   peer eth1 (192.168.2.2, VLAN 2; nodes are numbered hv01=1, peer=2)
#
# The CLI runs as strata-admin (a strata-operators member); mallory is an
# ordinary user and must be refused.
{ self, pkgs }:
let
  vmImage = import ./lib/vm-image.nix { inherit pkgs; };
  keys = import (pkgs.path + "/nixos/tests/ssh-keys.nix") pkgs;
  spec = builtins.toJSON {
    apiVersion = "vm.strata.dev/v1alpha1";
    name = "web-01";
    resources = {
      cpu = 1;
      memory = "1GiB";
    };
    image = "nixos";
    disks = [
      {
        name = "root";
        size = "8GiB";
        pool = "vmpool";
      }
      {
        name = "data";
        size = "1GiB";
        pool = "vmpool";
      }
    ];
    networks = [
      {
        name = "eth0";
        network = "lan";
      }
    ];
    autostart = true;
    cloudInit.sshAuthorizedKeys = [ keys.snakeOilEd25519PublicKey ];
  };
  emptySpec = builtins.toJSON {
    apiVersion = "vm.strata.dev/v1alpha1";
    name = "blank";
    resources = {
      cpu = 1;
      memory = "256MiB";
    };
    disks = [
      {
        name = "root";
        size = "1GiB";
        pool = "vmpool";
      }
    ];
  };
in
pkgs.testers.runNixOSTest {
  name = "strata-vms";

  nodes = {
    hv01 =
      { lib, pkgs, ... }:
      {
        imports = [ self.nixosModules.default ];
        environment.systemPackages = [
          pkgs.jq
          pkgs.curl
        ];
        users.users.mallory.isNormalUser = true;

        virtualisation = {
          vlans = [
            1
            2
          ];
          cores = 2;
          memorySize = 3072;
          diskSize = 4096;
          emptyDiskImages = [
            {
              size = 12288;
              driveConfig.deviceExtraOpts.serial = "vmpool";
            }
          ];
          fileSystems."/srv/vmpool" = {
            device = "/dev/disk/by-id/virtio-vmpool";
            fsType = "ext4";
            autoFormat = true;
          };
        };
        networking.interfaces.eth2 = {
          ipv4.addresses = lib.mkForce [ ];
          ipv6.addresses = lib.mkForce [ ];
        };

        strata = {
          enable = true;
          node.id = "hv01";
          agent.enable = true;
          compute = {
            enable = true;
            # Allowed, but unreachable in the test (no network): the
            # restriction is checked before the download.
            incus.imageServers = [ "images.example.invalid" ];
          };
          access = {
            enable = true;
            admin.authorizedKeys = [ keys.snakeOilEd25519PublicKey ];
          };
          storage = {
            defaultPool = "vmpool";
            pools.vmpool = {
              driver = "dir";
              path = "/srv/vmpool";
            };
          };
          networking = {
            enable = true;
            management = {
              interface = "eth1";
              addresses = [ "192.168.1.2/24" ];
            };
            guest.networks.lan.uplink = "eth2";
          };
        };
      };

    peer = {
      virtualisation.vlans = [ 2 ];
      networking.firewall.enable = false;
    };
  };

  testScript = ''
    import json

    start_all()
    hv01.wait_for_unit("strata-incus-setup.service")
    hv01.wait_for_unit("strata-agent.service")

    with subtest("nested virtualization is available"):
        # Booting a guest is the point of this test: fail clearly instead of
        # skipping (VM tests run locally, ADR-0016).
        code, _ = hv01.execute("test -c /dev/kvm")
        assert code == 0, "/dev/kvm is missing in the test VM: enable nested virtualization on the host"

    hv01.succeed(
        "incus image import ${vmImage}/metadata.tar.xz ${vmImage}/root.qcow2 --alias nixos --project strata",
        "install -m 644 ${pkgs.writeText "web.json" spec} /tmp/web.json",
        "install -m 644 ${pkgs.writeText "blank.json" emptySpec} /tmp/blank.json",
    )

    def admin(command):
        return f"su strata-admin -c 'strata vm {command}'"

    def inst(name):
        return json.loads(hv01.succeed(f"incus query '/1.0/instances/{name}?project=strata'"))

    def guest(command):
        return f"timeout 60 incus exec web-01 --project strata --disable-stdin --force-interactive -- {command}"

    def settle():
        """Wait until the guest has booted and is past logind's 30 s holdoff."""
        sw = "/run/current-system/sw/bin/"
        hv01.wait_until_succeeds(guest("true"), timeout=300)  # the guest agent answers
        state = hv01.succeed(guest(sw + "sh -c '" + sw + "systemctl is-system-running --wait || true'")).strip()
        print(f"guest state: {state}; failed units: " + hv01.succeed(guest(sw + "systemctl --failed --plain --no-legend")))
        assert state in ("running", "degraded"), state
        hv01.wait_until_succeeds(guest(sw + "sh -c 'test $(" + sw + "cut -d. -f1 /proc/uptime) -ge 35'"), timeout=120)

    with subtest("the API socket belongs to strata-operators"):
        assert hv01.succeed("stat -c '%a %U %G' /run/strata/agent.sock").strip() == "660 root strata-operators"
        out = hv01.fail("su mallory -c 'strata vm list' 2>&1")
        assert "permission denied (members of the strata-operators group" in out, out

    with subtest("create a VM from a spec"):
        out = hv01.succeed(admin("create -f /tmp/web.json"))
        assert out.startswith("create web-01: done (operation "), out
        i = inst("web-01")
        assert i["type"] == "virtual-machine" and i["status"] == "Stopped", i["status"]
        assert i["config"]["limits.cpu"] == "1" and i["config"]["limits.memory"] == "1GiB", i["config"]
        assert i["config"]["boot.autostart"] == "true" and i["config"]["security.nesting"] == "false", i["config"]
        assert i["devices"]["root"] == {"type": "disk", "path": "/", "pool": "vmpool", "size": "8GiB"}, i["devices"]
        assert i["devices"]["data"] == {"type": "disk", "pool": "vmpool", "source": "web-01-data"}, i["devices"]
        assert i["devices"]["eth0"] == {"type": "nic", "network": "lan", "name": "eth0"}, i["devices"]
        assert i["profiles"] == [], i["profiles"]
        hv01.succeed("incus storage volume show vmpool web-01-data --project strata")
        listing = hv01.succeed(admin("list"))
        assert "web-01" in listing and "Stopped" in listing, listing

    with subtest("start: the guest boots and reaches its network"):
        hv01.succeed(admin("start web-01"))
        assert inst("web-01")["status"] == "Running"
        # The guest agent answers once the guest OS is up.
        hv01.wait_until_succeeds(guest("true"), timeout=300)
        print("GUEST BOOTED: " + hv01.succeed(guest("uname -sr")).strip())
        # The data disk is attached (1 GiB).
        assert "1073741824" in hv01.succeed(guest("lsblk -bdno SIZE")), "data disk missing"
        hv01.succeed(
            # NixOS guest: tools live in the system profile.
            guest("/run/current-system/sw/bin/sh -c 'PATH=/run/current-system/sw/bin; "
                  "dev=$(ls /sys/class/net | grep -v lo | head -1); ip addr add 192.168.2.60/24 dev $dev; ip link set $dev up'")
        )
        hv01.wait_until_succeeds(guest("/run/current-system/sw/bin/ping -c 1 -W 2 192.168.2.2"), timeout=60)

    with subtest("stop, restart and force stop"):
        # A graceful stop is an ACPI power-button press. systemd-logind
        # ignores it until boot has finished and for 30 s after startup
        # (HoldoffTimeoutSec), so wait for that first.
        settle()
        hv01.succeed(admin("stop web-01"))
        assert inst("web-01")["status"] == "Stopped"
        hv01.succeed(admin("start web-01"))
        settle()
        hv01.succeed(admin("restart web-01"))
        assert inst("web-01")["status"] == "Running"
        hv01.succeed(admin("show web-01"))

    with subtest("an empty VM (no image) starts its firmware"):
        hv01.succeed(admin("create -f /tmp/blank.json"))
        hv01.succeed(admin("start blank"))
        assert inst("blank")["status"] == "Running"
        hv01.succeed(admin("stop blank --force --timeout 0"))
        assert inst("blank")["status"] == "Stopped"

    with subtest("bad requests are refused"):
        out = hv01.fail(admin("create -f /tmp/web.json") + " 2>&1")
        assert "already exists" in out, out
        hv01.succeed("jq '.name = \"other\" | .disks[0].pool = \"nopool\"' /tmp/web.json > /tmp/other.json && chmod 644 /tmp/other.json")
        out = hv01.fail(admin("create -f /tmp/other.json") + " 2>&1")
        assert "nopool" in out and "operation" in out, out
        assert "other" not in hv01.succeed(admin("list"))
        # Its data volume was created first and removed again.
        hv01.fail("incus storage volume show vmpool other-data --project strata")
        hv01.succeed("jq '.name = \"remote\" | .image = \"images:debian/13\"' /tmp/web.json > /tmp/remote.json && chmod 644 /tmp/remote.json")
        out = hv01.fail(admin("create -f /tmp/remote.json") + " 2>&1")
        assert "server:alias is not accepted" in out, out

    with subtest("images download only from declared servers"):
        cfg = json.loads(hv01.succeed("incus query /1.0/projects/strata"))["config"]
        assert cfg["restricted.images.servers"] == "images.example.invalid", cfg
        hv01.succeed(
            "jq '.name = \"r1\" | .imageServer = \"images.other.invalid\" | .image = \"debian/13\" | .disks = [.disks[0]]'"
            " /tmp/web.json > /tmp/r1.json",
            "jq '.name = \"r2\" | .imageServer = \"images.example.invalid\" | .image = \"debian/13\" | .disks = [.disks[0]]'"
            " /tmp/web.json > /tmp/r2.json",
            "chmod 644 /tmp/r1.json /tmp/r2.json",
        )
        out = hv01.fail(admin("create -f /tmp/r1.json") + " 2>&1")
        assert 'Image server "images.other.invalid" isn\'t allowed in this project' in out, out
        # The declared server passes the restriction; the download itself
        # fails (no network in the test).
        out = hv01.fail(admin("create -f /tmp/r2.json") + " 2>&1")
        assert "isn't allowed" not in out and "images.example.invalid" in out, out
        hv01.succeed(
            "mkdir -p /tmp/agent/servercerts",
            "cp /var/lib/strata-incus/client.crt /var/lib/strata-incus/client.key /tmp/agent/",
            "cp /var/lib/incus/server.crt /tmp/agent/servercerts/hv01.crt",
            "printf 'default-remote: hv01\\nremotes:\\n  hv01:\\n    addr: https://127.0.0.1:8443\\n"
            "    protocol: incus\\n    auth_type: tls\\n    project: strata\\n' > /tmp/agent/config.yml",
        )
        body = json.dumps({"name": "dl", "type": "virtual-machine", "source": {
            "type": "image", "server": "https://images.linuxcontainers.org", "protocol": "simplestreams", "alias": "debian/13"}})
        out = hv01.fail(f"INCUS_CONF=/tmp/agent incus query -X POST '/1.0/instances?project=strata' --data '{body}' 2>&1")
        assert "isn't allowed in this project" in out, out

    hv01.succeed(
        "jq '.resources = {\"cpu\": 2, \"memory\": \"2GiB\"} | .disks += [{\"name\": \"logs\", \"size\": \"1GiB\", \"pool\": \"vmpool\"}]'"
        " /tmp/web.json > /tmp/web2.json",
        "jq '.disks = [.disks[0], .disks[2]]' /tmp/web2.json > /tmp/web3.json",
        "jq '.disks[0].size = \"4GiB\"' /tmp/web2.json > /tmp/shrink.json",
        "jq '.image = \"other\"' /tmp/web2.json > /tmp/reimage.json",
        "chmod 644 /tmp/web2.json /tmp/web3.json /tmp/shrink.json /tmp/reimage.json",
    )

    with subtest("update: the plan is shown; declining changes nothing"):
        out = hv01.fail(admin("update -f /tmp/web2.json < /dev/null") + " 2>&1")
        for expected in ["Plan for web-01 (running)", "resources.cpu", "1 -> 2", "restart",
                         "new data volume web-01-logs", "stopped gracefully, changed and started again",
                         "Nothing was changed."]:
            assert expected in out, f"missing {expected!r} in:\n{out}"
        assert inst("web-01")["config"]["limits.cpu"] == "1"

    with subtest("update: a confirmed plan restarts the guest with the new resources"):
        settle()  # the restart begins with a graceful stop
        out = hv01.succeed("echo y | " + admin("update -f /tmp/web2.json"))
        assert "update web-01: done" in out, out
        i = inst("web-01")
        assert i["status"] == "Running" and i["config"]["limits.cpu"] == "2" and i["config"]["limits.memory"] == "2GiB", i["config"]
        hv01.succeed("incus storage volume show vmpool web-01-logs --project strata")
        settle()
        sw = "/run/current-system/sw/bin/"
        assert hv01.succeed(guest(sw + "nproc")).strip() == "2"
        mem_kib = int(hv01.succeed(guest(sw + "grep MemTotal /proc/meminfo")).split()[1])
        assert mem_kib > 1800 * 1024, mem_kib
        sizes = hv01.succeed(guest(sw + "lsblk -bdno SIZE")).split()
        assert sizes.count("1073741824") == 2, sizes  # data and logs

    with subtest("update: data-losing changes are refused"):
        out = hv01.fail(admin("update -f /tmp/shrink.json --yes") + " 2>&1")
        assert "disks cannot shrink" in out, out
        out = hv01.fail(admin("update -f /tmp/reimage.json --yes") + " 2>&1")
        assert "the image is only used when a VM is created" in out, out

    with subtest("update: a stale plan is refused"):
        plan = json.loads(hv01.succeed(
            "su strata-admin -c \"curl -sf --unix-socket /run/strata/agent.sock -X PUT"
            " 'http://agent/api/v1/vms/web-01?plan=true' --data @/tmp/web3.json\""))
        assert plan["changes"] and plan["restart"], plan
        hv01.succeed(admin("stop web-01 --force --timeout 0"))  # the VM changed since the plan
        op = json.loads(hv01.succeed(
            "su strata-admin -c \"curl -sf --unix-socket /run/strata/agent.sock -X PUT"
            f" 'http://agent/api/v1/vms/web-01?confirm={plan['id']}' --data @/tmp/web3.json\""))
        hv01.wait_until_succeeds(
            "su strata-admin -c \"curl -sf --unix-socket /run/strata/agent.sock"
            f" http://agent/api/v1/operations/{op['id']}\" | jq -e '.status == \"FAILED\" and (.error | contains(\"plan again\"))'",
            timeout=30,
        )
        assert inst("web-01")["devices"].get("data"), "a stale plan must not be applied"

    with subtest("update: removing a data disk detaches it and keeps the volume"):
        out = hv01.succeed(admin("update -f /tmp/web3.json --yes"))  # stopped: no restart
        assert "detached; volume web-01-data is kept" in out and "(stopped)" in out and "started again" not in out, out
        assert "data" not in inst("web-01")["devices"]
        hv01.succeed("incus storage volume show vmpool web-01-data --project strata")
        hv01.succeed(admin("start web-01"))

    with subtest("every request is audited with the caller's uid"):
        uid = hv01.succeed("id -u strata-admin").strip()
        audit = hv01.succeed("journalctl -u strata-agent -o cat | grep -F '\"msg\":\"audit\"'")
        assert f'"uid":{uid}' in audit and '"path":"/api/v1/vms"' in audit, audit
        mallory = hv01.succeed("id -u mallory").strip()
        assert f'"uid":{mallory}' not in audit, "mallory never reached the API"

    with subtest("an autostart VM runs again after a host reboot"):
        hv01.shutdown()
        hv01.start()
        hv01.wait_for_unit("strata-agent.service")
        hv01.wait_until_succeeds(
            "incus query '/1.0/instances/web-01?project=strata' | jq -e '.status == \"Running\"'", timeout=120
        )
        hv01.wait_until_succeeds(guest("true"), timeout=300)
        assert inst("blank")["status"] == "Stopped"  # not autostart
        failed = hv01.succeed("systemctl list-units --failed --plain --no-legend").strip()
        assert failed == "", failed

    with subtest("delete needs its name, and --stop for a running VM; data volumes are kept"):
        out = hv01.fail(admin("delete web-01") + " 2>&1")
        assert "confirm with --confirm web-01" in out, out
        out = hv01.fail(admin("delete web-01 --confirm web-02") + " 2>&1")
        assert "confirm with --confirm web-01" in out, out
        out = hv01.fail(admin("delete web-01 --confirm web-01") + " 2>&1")
        assert "it is Running" in out, out
        settle()
        out = hv01.succeed(admin("delete web-01 --confirm web-01 --stop"))
        assert "delete web-01: done" in out and "Data volumes kept (not deleted): vmpool/web-01-logs" in out, out
        hv01.fail("incus query '/1.0/instances/web-01?project=strata'")
        assert "web-01" not in hv01.succeed(admin("list"))
        volumes = hv01.succeed("incus storage volume list vmpool --project strata --format csv -c tn")
        assert "custom,web-01-data" in volumes and "custom,web-01-logs" in volumes, volumes
        assert "virtual-machine,web-01" not in volumes, volumes  # the root disk went with it
        # A new VM may not silently take over the kept volumes.
        out = hv01.fail(admin("create -f /tmp/web2.json") + " 2>&1")
        assert "never reused implicitly" in out, out
  '';
}
