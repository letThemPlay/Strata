# Example: a standalone Strata host with declared workloads.
#
# This is the `strata` part of a host's host.nix (see templates/host). The
# `examples` check evaluates it with the Strata modules, checks that the
# IR it produces is valid (`strata-controller validate-ir`) and that it is
# exactly internal/ir/testdata/standalone.json. The disks, interfaces and
# keys are placeholders.
{
  strata = {
    enable = true;
    node.id = "hv-01";
    agent.enable = true;
    monitoring.enable = true;

    access = {
      enable = true;
      admin.authorizedKeys = [
        "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl admin@example"
      ];
    };

    networking = {
      enable = true;
      management = {
        interface = "eno1";
        addressing = "dhcp";
      };
      interfaces = {
        eno1.macAddress = "84:47:09:a3:5d:dc";
        enp4s0.macAddress = "84:47:09:a3:5d:de";
      };
      guest = {
        defaultNetwork = "lan";
        networks = {
          lan = {
            uplink = "enp4s0";
            vlan = 10;
          };
          dmz = {
            uplink = "enp4s0";
            vlan = 20;
          };
        };
      };
    };

    storage = {
      defaultPool = "fast";
      pools = {
        fast = {
          driver = "zfs";
          devices = [
            "/dev/disk/by-id/nvme-EXAMPLE-1"
            "/dev/disk/by-id/nvme-EXAMPLE-2"
          ];
          zfs.layout = "mirror";
        };
        bulk = {
          driver = "lvm";
          devices = [ "/dev/disk/by-id/ata-EXAMPLE-3" ];
        };
      };
    };

    compute = {
      enable = true;
      incus = {
        imageServers = [ "images.linuxcontainers.org" ];
        limits = {
          cpu = 16;
          memory = "64GiB";
          instances = 20;
        };
      };
    };

    workloads.vms = {
      # A web server in the DMZ, from an image downloaded from a server.
      web-01 = {
        resources = {
          cpu = 2;
          memory = "2GiB";
        };
        image = "debian/13/cloud";
        imageServer = "images.linuxcontainers.org";
        disks.root.size = "20GiB"; # on the default pool, fast
        networks.eth0.network = "dmz";
        autostart = true;
        cloudInit.sshAuthorizedKeys = [
          "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl admin@example"
        ];
      };

      # A database with its data on the bulk pool; protected against
      # deletion and restarts.
      db-01 = {
        resources = {
          cpu = 4;
          memory = "8GiB";
        };
        image = "debian-13";
        disks = {
          root.size = "20GiB";
          data = {
            size = "200GiB";
            pool = "bulk";
          };
        };
        networks = {
          eth0.network = "lan";
          eth1.network = "dmz";
        };
        autostart = true;
        lifecycle.protected = true;
      };

      # An empty VM, to be installed later.
      scratch = {
        resources = {
          cpu = 1;
          memory = "1GiB";
        };
        disks.root.size = "10GiB";
      };
    };
  };
}
