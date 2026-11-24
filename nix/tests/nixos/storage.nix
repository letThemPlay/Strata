# NixOS VM test: workload storage pools on dedicated disks (ADR-0018).
#
# Disks (by-id names virtio-<serial>):
#   z1, z2  ZFS mirror "fast" (the default pool, limited to 200 MiB)
#   l1      LVM-thin "bulk"
#   d1      ext4 on /srv/scratch, dir pool "scratch"
#   dirty   gets an ext4 filesystem: must be refused
#   adopt   gets a hand-made zpool "tank": adopted on request
#   foreign gets a hand-made volume group "pve": must stay inactive
#
# Specialisations switch the declared pools without rebooting:
#   refusals  adds bad pools (dirty disk, system disk, changed driver)
#   second    removes "bulk" and adopts "tank"
{ self, pkgs }:
let
  testImage = import ./lib/test-image.nix { inherit pkgs; };
  disk = serial: "/dev/disk/by-id/virtio-${serial}";
  basePools = {
    fast = {
      driver = "zfs";
      devices = [
        (disk "z1")
        (disk "z2")
      ];
      zfs.layout = "mirror";
      limit = "200MiB";
    };
    bulk = {
      driver = "lvm";
      devices = [ (disk "l1") ];
    };
    scratch = {
      driver = "dir";
      path = "/srv/scratch";
    };
  };
in
pkgs.testers.runNixOSTest {
  name = "strata-storage";

  nodes.hv01 =
    { lib, pkgs, ... }:
    {
      imports = [ self.nixosModules.default ];

      environment.systemPackages = [
        pkgs.jq
        pkgs.e2fsprogs
      ];

      virtualisation = {
        cores = 2;
        memorySize = 3072;
        diskSize = 4096;
        emptyDiskImages =
          map
            (serial: {
              size = 1024;
              driveConfig.deviceExtraOpts.serial = serial;
            })
            [
              "z1"
              "z2"
              "l1"
              "d1"
              "dirty"
              "adopt"
              "foreign"
            ];
        fileSystems."/srv/scratch" = {
          device = disk "d1";
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
          defaultPool = "fast";
          # fast has a limit: Incus needs a size to count against it.
          rootSize = "64MiB";
          pools = basePools;
          # Test VMs do not use strata.disk, where this is the default.
          lvm.autoActivation = "declared";
        };
      };

      specialisation = {
        refusals.configuration.strata.storage.pools = lib.mkForce (
          basePools
          // {
            # A changed driver for an existing pool.
            bulk = {
              driver = "zfs";
              devices = [ (disk "l1") ];
            };
            dirty = {
              driver = "lvm";
              devices = [ (disk "dirty") ];
            };
            system = {
              driver = "zfs";
              devices = [ (disk "root") ];
            };
          }
        );
        second.configuration.strata.storage.pools = lib.mkForce (
          removeAttrs basePools [ "bulk" ]
          // {
            tank = {
              driver = "zfs";
              devices = [ (disk "adopt") ];
              adopt = true;
            };
          }
        );
      };
    };

  testScript = ''
    import json

    hv01.start()
    hv01.wait_for_unit("strata-incus-setup.service")
    hv01.wait_for_unit("strata-agent.service")
    # Specialisations are reached from the base system, not from whichever
    # one is current.
    base = hv01.succeed("readlink -f /run/current-system").strip()

    def pools():
        return {p["name"]: p for p in json.loads(hv01.succeed("incus query '/1.0/storage-pools?recursion=1'"))}

    def project_config():
        return json.loads(hv01.succeed("incus query /1.0/projects/strata"))["config"]

    def setup_log():
        return hv01.succeed("journalctl -u strata-incus-setup -o cat --no-pager")

    def switch(spec):
        # Returns the exit code: non-zero when strata-incus-setup fails.
        return hv01.execute(f"{base}/specialisation/{spec}/bin/switch-to-configuration test 2>&1")

    with subtest("declared pools are created on their disks"):
        p = pools()
        assert p["fast"]["driver"] == "zfs", p["fast"]
        assert p["fast"]["config"]["volatile.initial_source"] == "mirror=${disk "z1"},${disk "z2"}", p["fast"]
        assert "source.wipe" not in p["fast"]["config"], p["fast"]
        status = hv01.succeed("zpool status fast")
        assert "mirror-0" in status and "virtio-z1" in status and "virtio-z2" in status, status
        assert p["bulk"]["driver"] == "lvm" and p["bulk"]["config"]["lvm.vg_name"] == "bulk", p["bulk"]
        hv01.succeed("lvs bulk/IncusThinPool")
        assert p["scratch"]["config"]["source"] == "/srv/scratch/scratch", p["scratch"]

    with subtest("the project may use exactly the declared pools, within limits"):
        cfg = project_config()
        assert cfg["restricted.storage-pools.access"] == "bulk,fast,scratch", cfg
        assert cfg["limits.disk.pool.fast"] == "200MiB", cfg
        assert cfg["restricted.snapshots"] == "allow" and cfg["restricted.backups"] == "block", cfg
        profile = json.loads(hv01.succeed("incus query '/1.0/profiles/default?project=strata'"))
        assert profile["devices"]["root"] == {"type": "disk", "path": "/", "pool": "fast", "size": "64MiB"}, profile

    # The agent's identity, as in nixos-incus.
    hv01.succeed(
        "incus image import ${testImage}/metadata.tar.xz ${testImage}/rootfs.tar.xz --alias test --project strata",
        "mkdir -p /tmp/agent/servercerts",
        "cp /var/lib/strata-incus/client.crt /var/lib/strata-incus/client.key /tmp/agent/",
        "cp /var/lib/incus/server.crt /tmp/agent/servercerts/hv01.crt",
        "printf 'default-remote: hv01\\nremotes:\\n  hv01:\\n    addr: https://127.0.0.1:8443\\n"
        "    protocol: incus\\n    auth_type: tls\\n    project: strata\\n' > /tmp/agent/config.yml",
    )
    agent = "INCUS_CONF=/tmp/agent incus"

    def refused(command, message):
        out = hv01.fail(f"{agent} {command} 2>&1")
        assert message in out, f"{command}: expected {message!r}, got {out!r}"

    with subtest("the agent identity uses each declared pool and snapshots"):
        hv01.succeed(f"{agent} launch test c-fast")
        hv01.succeed(f"{agent} launch test c-bulk -s bulk")
        hv01.succeed(f"{agent} launch test c-scratch -s scratch")
        hv01.succeed(f"{agent} snapshot create c-fast snap0")
        for name, pool in [("c-fast", "fast"), ("c-bulk", "bulk"), ("c-scratch", "scratch")]:
            inst = json.loads(hv01.succeed(f"{agent} query '/1.0/instances/{name}?project=strata'"))
            assert inst["expanded_devices"]["root"]["pool"] == pool, inst["expanded_devices"]
            assert inst["status"] == "Running", inst["status"]

    with subtest("the agent identity is refused undeclared pools, the pool limit and backups"):
        hv01.succeed("incus storage create manual dir")
        hv01.succeed("systemctl restart strata-incus-setup")
        assert project_config()["limits.disk.pool.manual"] == "0"
        # Incus hides pools the project may not use: the identity gets "not
        # found" for a pool that exists (root sees it).
        hv01.succeed("incus storage show manual")
        refused("init test x1 -s manual", 'Failed loading storage pool "manual": not found')
        refused("init test x2 -d root,size=1GiB", "Reached maximum aggregate value")
        refused("export c-fast /tmp/c-fast.tar.gz", "doesn't allow for backup creation")

    with subtest("undeclared and changed pools, data disks and the system disk are refused"):
        hv01.succeed("mkfs.ext4 -q -L olddata ${disk "dirty"}", "udevadm settle")
        code, out = switch("refusals")
        assert code != 0, out
        log = setup_log()
        for expected in [
            'pool dirty: ${disk "dirty"}: not empty (/dev/',
            'ext4 \\"olddata\\"); Strata does not erase disks',
            "pool system: ${disk "root"}: /dev/vda holds the running system",
            "pool bulk: exists with driver lvm, declared zfs",
        ]:
            assert expected in log, f"missing {expected!r} in:\n{log}"
        assert hv01.succeed("blkid -s LABEL -o value ${disk "dirty"}").strip() == "olddata"
        p = pools()
        assert "dirty" not in p and "system" not in p and p["bulk"]["driver"] == "lvm", p.keys()
        # The valid pools are still in use.
        hv01.succeed(f"{agent} exec c-fast --disable-stdin --force-interactive -- /bin/true")

    with subtest("removing a pool keeps it and its data; an empty zpool is adopted"):
        hv01.succeed("zpool create -m none tank ${disk "adopt"}", "zpool export tank", "udevadm settle")
        code, out = switch("second")
        assert code == 0, out
        p = pools()
        assert p["tank"]["driver"] == "zfs" and p["tank"]["config"]["volatile.initial_source"] == "tank", p["tank"]
        hv01.succeed("zpool list tank")
        # bulk still exists with its container. Incus refuses to close a pool
        # under existing instances, and Strata never deletes them: it stays
        # open, with a warning, until the project no longer uses it.
        assert "bulk" in p, p.keys()
        cfg = project_config()
        assert cfg["restricted.storage-pools.access"] == "bulk,fast,scratch,tank", cfg
        assert "limits.disk.pool.bulk" not in cfg, cfg
        assert "still used by the project" in setup_log()
        hv01.succeed(f"{agent} info c-bulk")
        # Once its container is gone, the next run closes it; the pool and
        # its volume group remain.
        hv01.succeed(f"{agent} delete --force c-bulk")
        hv01.succeed("systemctl restart strata-incus-setup")
        cfg = project_config()
        assert cfg["restricted.storage-pools.access"] == "fast,scratch,tank", cfg
        assert cfg["limits.disk.pool.bulk"] == "0", cfg
        hv01.succeed("incus storage show bulk", "vgs bulk")
        refused("init test x3 -s bulk", 'Failed loading storage pool "bulk": not found')
        hv01.succeed(f"{agent} launch test c-tank -s tank")

    with subtest("leftover volume groups stay inactive across a reboot"):
        hv01.succeed(
            "pvcreate -q ${disk "foreign"}",
            "vgcreate -q pve ${disk "foreign"}",
            "lvcreate -q -L 64M -n data pve",
            "vgchange -q -an pve",
        )
        hv01.shutdown()
        hv01.start()
        hv01.wait_for_unit("strata-incus-setup.service")
        hv01.wait_for_unit("strata-agent.service")
        attrs = hv01.succeed("lvs --noheadings -o lv_attr pve/data").strip()
        assert attrs[4] != "a", f"pve/data was activated: {attrs}"
        failed = hv01.succeed("systemctl list-units --failed --plain --no-legend").strip()
        assert failed == "", failed

    with subtest("pools and containers survive the reboot"):
        p = pools()
        assert {"fast", "bulk", "scratch"} <= set(p), p.keys()
        hv01.succeed("zpool list fast")
        # Incus restores the instances' previous state: they come back running.
        for name in ["c-fast", "c-scratch", "c-tank"]:
            hv01.wait_until_succeeds(
                f"{agent} query '/1.0/instances/{name}?project=strata' | jq -e '.status == \"Running\"'", timeout=60
            )
            hv01.wait_until_succeeds(
                f"INCUS_CONF=/tmp/agent timeout 30 incus exec {name} --disable-stdin --force-interactive -- /bin/true", timeout=60
            )
        assert "snap0" in hv01.succeed(f"{agent} snapshot list c-fast --format csv")
  '';
}
