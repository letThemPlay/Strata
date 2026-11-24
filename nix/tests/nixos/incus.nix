# NixOS VM test: Incus on a Strata compute host and the agent's restricted
# identity (ADR-0015, ADR-0017).
#
# The agent's identity (key and certificate generated on the host) is used
# by an `incus` client in the test. It must be able to run a container in
# the Strata project and be refused everything outside it: privileged or
# nested containers, host paths and devices, low-level settings, other
# projects, project, server and trust changes. Every refusal is checked
# against Incus's message, so that an unrelated error cannot pass.
#
#   admin (VLAN 1, 192.168.1.1) ── management ── hv01 eth1 (192.168.1.2)
{ self, pkgs }:
let
  testImage = import ./lib/test-image.nix { inherit pkgs; };
in
pkgs.testers.runNixOSTest {
  name = "strata-incus";

  nodes = {
    hv01 =
      { pkgs, ... }:
      {
        imports = [ self.nixosModules.default ];

        environment.systemPackages = [
          pkgs.curl
          pkgs.jq
        ];

        virtualisation = {
          vlans = [ 1 ];
          cores = 2;
          memorySize = 2048;
          diskSize = 4096;
          # A dedicated disk for the test's dir pool.
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
          };
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
    hv01.wait_for_unit("incus.service")
    hv01.wait_for_unit("strata-incus-setup.service")
    hv01.wait_for_unit("strata-agent.service")

    def trust_entries():
        return [
            c for c in json.loads(hv01.succeed("incus query '/1.0/certificates?recursion=1'"))
            if c["name"] == "strata-agent"
        ]

    with subtest("libvirtd is not installed"):
        hv01.fail("systemctl cat libvirtd.service")
        hv01.fail("command -v virsh")

    with subtest("the agent identity is trusted, restricted to the project"):
        entries = trust_entries()
        assert len(entries) == 1, entries
        e = entries[0]
        assert e["restricted"] is True and e["projects"] == ["strata"] and e["type"] == "client", e
        hv01.succeed("test $(stat -c %a /var/lib/strata-incus) = 700")
        hv01.succeed("test $(stat -c %a /var/lib/strata-incus/client.key) = 600")

    with subtest("the agent reaches Incus with its identity"):
        hv01.wait_until_succeeds(
            "jq -e '.backend == \"incus\" and .reachable and .trusted' /var/lib/strata/backend.json",
            timeout=60,
        )
        hv01.succeed("journalctl -u strata-agent -o cat | grep -F '\"msg\":\"workload backend available\"'")

    with subtest("the agent has no access to the Incus Unix socket"):
        pid = hv01.succeed("systemctl show -p MainPID --value strata-agent").strip()
        uid = int(hv01.succeed(f"ps -o uid= -p {pid}").strip())
        assert uid != 0, uid
        groups = hv01.succeed(f"grep ^Groups: /proc/{pid}/status").split()[1:]
        incus_admin = hv01.succeed("getent group incus-admin | cut -d: -f3").strip()
        assert incus_admin not in groups, groups
        # Connecting to a Unix socket needs write permission on it.
        as_agent = f"setpriv --reuid={uid} --regid={uid} --clear-groups"
        hv01.fail(f"{as_agent} test -w /var/lib/incus/unix.socket")
        hv01.fail(f"{as_agent} curl -sSf --unix-socket /var/lib/incus/unix.socket http://incus/1.0")
        # Positive control: the socket itself works.
        hv01.succeed("curl -sSf --unix-socket /var/lib/incus/unix.socket http://incus/1.0")

    with subtest("the Incus API listens on loopback only"):
        # Column 4 is the local address of each listening socket.
        listeners = hv01.succeed("ss -Hltn 'sport = :8443' | awk '{print $4}'").split()
        assert listeners == ["127.0.0.1:8443"], listeners
        admin.wait_until_succeeds("ping -c 1 -W 2 192.168.1.2")
        admin.fail("nc -z -w 3 192.168.1.2 8443")

    with subtest("the declared pool is the project's default"):
        pool = json.loads(hv01.succeed("incus query /1.0/storage-pools/test"))
        assert pool["driver"] == "dir" and pool["config"]["source"] == "/srv/pool/test", pool
        profile = json.loads(hv01.succeed("incus query '/1.0/profiles/default?project=strata'"))
        assert profile["devices"]["root"] == {"type": "disk", "path": "/", "pool": "test"}, profile

    # A test-only image, imported as root.
    hv01.succeed(
        "incus image import ${testImage}/metadata.tar.xz ${testImage}/rootfs.tar.xz --alias test --project strata",
    )

    # An `incus` client using the agent's identity over HTTPS.
    hv01.succeed(
        "mkdir -p /tmp/agent/servercerts",
        "cp /var/lib/strata-incus/client.crt /var/lib/strata-incus/client.key /tmp/agent/",
        "cp /var/lib/incus/server.crt /tmp/agent/servercerts/hv01.crt",
        "printf 'default-remote: hv01\\nremotes:\\n  hv01:\\n    addr: https://127.0.0.1:8443\\n"
        "    protocol: incus\\n    auth_type: tls\\n    project: strata\\n' > /tmp/agent/config.yml",
    )
    agent = "INCUS_CONF=/tmp/agent incus"

    def in_c1(command):
        # The test driver never closes stdin: without --disable-stdin, incus
        # exec waits for it forever (as in nixpkgs' Incus tests).
        return f"INCUS_CONF=/tmp/agent timeout 30 incus exec c1 --disable-stdin --force-interactive -- {command}"

    def refused(command, message):
        out = hv01.fail(f"{agent} {command} 2>&1")
        assert message in out, f"{command}: expected {message!r}, got {out!r}"

    with subtest("the agent identity manages containers in its project"):
        code, out = hv01.execute(f"{agent} launch test c1 2>&1")
        if code != 0:
            print(hv01.execute("cat /var/log/incus/strata_c1/lxc.log; ls -la /srv/pool /srv/pool/test; findmnt /srv/pool")[1])
            raise Exception(f"launch failed: {out}")
        hv01.wait_until_succeeds(in_c1("/bin/true"), timeout=60)
        assert hv01.succeed(in_c1("/bin/id -u")).strip() == "0"
        info = json.loads(hv01.succeed(f"{agent} query '/1.0/instances/c1?project=strata'"))
        assert info["status"] == "Running", info["status"]

    with subtest("the agent identity is refused privileged and nested containers"):
        refused("init test p1 -c security.privileged=true", "Privileged containers are forbidden")
        refused("init test p2 -c security.nesting=true", "Container nesting is forbidden")

    with subtest("the agent identity is refused low-level settings, host paths and devices"):
        refused("config set c1 raw.lxc=lxc.apparmor.profile=unconfined", 'Use of low-level config "raw.lxc"')
        refused("config device add c1 etc disk source=/etc path=/mnt/etc", "Attaching disks not backed by a pool is forbidden")
        refused("config device add c1 mem unix-char source=/dev/mem path=/dev/mem", "Unix character devices are forbidden")
        refused("config device add c1 kmsg unix-block source=/dev/vda path=/dev/vda", "Unix block devices are forbidden")
        refused("config device add c1 fwd proxy listen=tcp:0.0.0.0:2222 connect=tcp:127.0.0.1:22", "Proxy devices are forbidden")

    with subtest("the agent identity is confined to its project"):
        refused("init x1 --empty --project default", 'User does not have permission for project "default"')
        refused("project set strata restricted=false", "Certificate is restricted")
        refused("project create other", "Certificate is restricted")

    with subtest("the agent identity cannot change the server, trust or storage pools"):
        refused("config set core.https_address 0.0.0.0:8443", "Certificate is restricted")
        # Incus answers with its generic 403 here (the request falls through to
        # token-based enrollment, which a trusted client cannot use).
        refused("config trust add-certificate /tmp/agent/client.crt --name x", "not authorized")
        refused("storage create other dir", "Certificate is restricted")

    with subtest("setup enforces the project configuration and is idempotent"):
        hv01.succeed("incus project set strata restricted.containers.privilege=allow")
        hv01.succeed("incus project set strata restricted.devices.disk.paths=/etc")
        # The project is not empty now (c1): its features must not change.
        hv01.succeed("systemctl restart strata-incus-setup")
        cfg = json.loads(hv01.succeed("incus query /1.0/projects/strata"))["config"]
        assert cfg["restricted.containers.privilege"] == "unprivileged", cfg
        # Incus stores an empty value as "unset": the key is gone.
        assert cfg.get("restricted.devices.disk.paths", "") == "", cfg
        assert len(trust_entries()) == 1, trust_entries()
        refused("init test p3 -c security.privileged=true", "Privileged containers are forbidden")

    with subtest("setup replaces a trust entry with wider permissions"):
        fp = trust_entries()[0]["fingerprint"]
        hv01.succeed(f"incus config trust remove {fp}")
        hv01.succeed("incus config trust add-certificate /var/lib/strata-incus/client.crt --name strata-agent")
        assert trust_entries()[0]["restricted"] is False
        hv01.succeed("systemctl restart strata-incus-setup")
        entries = trust_entries()
        assert len(entries) == 1 and entries[0]["restricted"] is True and entries[0]["projects"] == ["strata"], entries

    with subtest("containers keep running when incusd restarts"):
        hv01.succeed("systemctl restart incus.service")
        hv01.wait_for_unit("incus.service")
        hv01.wait_until_succeeds(in_c1("/bin/true"), timeout=60)

    with subtest("the agent identity stops and deletes its container"):
        hv01.succeed(f"{agent} stop c1 --force", f"{agent} delete c1")
        assert hv01.succeed(f"{agent} list --format csv -c n").strip() == ""
  '';
}
