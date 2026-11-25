# Evaluation tests for the Strata NixOS modules: option validation,
# assertions and generated configuration. No VM is built.
{
  self,
  nixpkgs,
  pkgs,
  lib,
}:
let
  eval =
    modules:
    nixpkgs.lib.nixosSystem {
      modules = [
        self.nixosModules.default
        {
          nixpkgs.hostPlatform = pkgs.stdenv.hostPlatform.system;
          boot.loader.grub.enable = false;
          # mkDefault: strata.disk defines "/" in its own cases.
          fileSystems."/" = lib.mkDefault {
            device = "/dev/null";
            fsType = "ext4";
          };
          system.stateVersion = "26.05";
        }
      ]
      ++ modules;
    };

  failedAssertions = sys: map (a: a.message) (lib.filter (a: !a.assertion) sys.config.assertions);
  hasFailure = needle: sys: lib.any (lib.hasInfix needle) (failedAssertions sys);
  throws = v: !(builtins.tryEval (builtins.deepSeq v true)).success;

  valid = eval [
    {
      strata = {
        enable = true;
        node.id = "hv-01";
        agent.enable = true;
      };
    }
  ];
  disabled = eval [ ];
  withNode = id: eval [ { strata.node.id = id; } ];
  agentService = valid.config.systemd.services.strata-agent.serviceConfig;

  testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@strata";

  # A fully configured host; `extra` modules are merged on top.
  hostWith =
    extra:
    eval (
      [
        {
          strata = {
            enable = true;
            node.id = "hv-01";
            agent.enable = true;
            compute.enable = true;
            access = {
              enable = true;
              admin.authorizedKeys = [ testKey ];
            };
            networking = {
              enable = true;
              management.interface = "eno1";
            };
            monitoring.enable = true;
          };
        }
      ]
      ++ extra
    );
  static = {
    strata.networking.management = {
      addresses = [
        "192.0.2.10/24"
        "2001:db8::10/64"
      ];
      gateway4 = "192.0.2.1";
      dns = [ "192.0.2.53" ];
    };
  };
  host = hostWith [ static ];
  hc = host.config;
  dhcpHost = hostWith [ { strata.networking.management.addressing = "dhcp"; } ];
  mgmtUnit = hc.systemd.network.networks."10-strata-management";
  diskHost = hostWith [
    static
    {
      strata.disk = {
        enable = true;
        device = "/dev/disk/by-id/test-disk";
      };
    }
  ];
  pool = driver: extra: { inherit driver; } // extra;
  disk1 = "/dev/disk/by-id/ata-DISK1";
  disk2 = "/dev/disk/by-id/ata-DISK2";
  disk3 = "/dev/disk/by-id/ata-DISK3";
  storageWith =
    storage:
    hostWith [
      static
      {
        strata.storage = storage;
        fileSystems."/srv/scratch" = {
          device = "/dev/disk/by-id/ata-DIR";
          fsType = "ext4";
        };
      }
    ];
  storage = storageWith {
    defaultPool = "fast";
    rootSize = "20GiB";
    pools = {
      fast = pool "zfs" {
        devices = [
          disk1
          disk2
        ];
        zfs.layout = "mirror";
        limit = "1TiB";
      };
      bulk = pool "lvm" { devices = [ disk3 ]; };
      scratch = pool "dir" { path = "/srv/scratch"; };
    };
  };
  sc = storage.config;
  state = sc.strata.compute.incus.setupState;
  storageFails = msg: st: hasFailure msg (storageWith st);
  guestWith =
    guest:
    hostWith [
      static
      { strata.networking.guest = guest; }
    ];
  guest = guestWith {
    defaultNetwork = "lan";
    networks = {
      lan.uplink = "eno2";
      v20 = {
        uplink = "eno3";
        vlan = 20;
      };
      v30 = {
        uplink = "eno3";
        vlan = 30;
      };
      lab = { };
    };
  };
  gc = guest.config;
  guestFails = msg: g: hasFailure msg (guestWith g);
  pinned = hostWith [
    static
    {
      strata.networking = {
        interfaces = {
          eno1.macAddress = "84:47:09:a3:5d:dc";
          eno2.macAddress = "84:47:09:a3:5d:de";
        };
        guest.networks.lan.uplink = "eno2";
      };
    }
  ];
  pc = pinned.config;
  lvmConf = sys: sys.config.environment.etc."lvm/lvm.conf".text or "";
  templateModules = [
    ../../templates/host/hardware-configuration.nix
    ../../templates/host/configuration.nix
    ../../templates/host/host.nix
  ];
  # Option value evaluation throws on type errors.
  rejects = sys: path: throws (lib.attrByPath path null sys.config);

  # Each host module must refuse to work without strata.enable.
  requiresEnable =
    module: settings:
    hasFailure "strata.${module}.enable requires strata.enable" (eval [
      {
        strata.${module} = {
          enable = true;
        }
        // settings;
      }
    ]);

  # ---- workloads ----
  vmBase = {
    resources = {
      cpu = 2;
      memory = "2GiB";
    };
    disks.root.size = "10GiB";
  };
  workloadsWith =
    {
      vms,
      compute ? { },
      defaultPool ? "fast",
    }:
    hostWith [
      static
      {
        strata = {
          storage = {
            inherit defaultPool;
            pools = {
              fast = pool "zfs" { devices = [ disk1 ]; };
              bulk = pool "lvm" { devices = [ disk2 ]; };
            };
          };
          networking.guest.networks = {
            lan.uplink = "eno2";
            dmz = {
              uplink = "eno3";
              vlan = 20;
            };
          };
          compute.incus = compute;
          workloads.vms = vms;
        };
      }
    ];
  workloads = workloadsWith {
    compute.imageServers = [ "images.linuxcontainers.org" ];
    vms = {
      web = vmBase // {
        image = "debian/13/cloud";
        imageServer = "images.linuxcontainers.org";
        networks = {
          eth1.network = "lan";
          eth0.network = "dmz";
        };
        autostart = true;
      };
      db = vmBase // {
        disks = {
          root.size = "10GiB";
          logs = {
            size = "1GiB";
            pool = "bulk";
          };
          data = {
            size = "5GiB";
            pool = "bulk";
          };
        };
        lifecycle.protected = true;
      };
    };
  };
  wir = workloads.config.strata.ir;
  wres = lib.listToAttrs (map (r: lib.nameValuePair r.id r) wir.resources);
  workloadFails = msg: args: hasFailure msg (workloadsWith args);
  oneVM = vm: { vms.vm1 = lib.recursiveUpdate vmBase vm; };

  cases = {
    # ---- full host ----
    "full static host has no failed assertions" = failedAssertions host == [ ];
    "full DHCP host has no failed assertions" = failedAssertions dhcpHost == [ ];

    # ---- compute ----
    "compute enables Incus LTS and not libvirtd" =
      hc.virtualisation.incus.enable
      && hc.virtualisation.incus.package.outPath == pkgs.incus-lts.outPath
      && !hc.virtualisation.libvirtd.enable;
    "compute keeps the Incus API on loopback, without the web UI" =
      hc.virtualisation.incus.preseed.config."core.https_address" == "127.0.0.1:8443"
      && !hc.virtualisation.incus.ui.enable
      && !(lib.elem 8443 hc.networking.firewall.allowedTCPPorts);
    "compute starts Incus at boot and keeps instances on daemon restart" =
      !hc.virtualisation.incus.socketActivation && hc.virtualisation.incus.softDaemonRestart;
    "compute sets up the restricted project before the agent" =
      let
        s = hc.systemd.services.strata-incus-setup;
      in
      lib.elem "strata-agent.service" s.before
      && lib.elem "incus.service" s.requires
      && s.serviceConfig.StateDirectoryMode == "0700";
    "compute without strata.networking still uses nftables" =
      (eval [
        {
          strata = {
            enable = true;
            node.id = "hv-01";
            compute.enable = true;
          };
        }
      ]).config.networking.nftables.enable;
    "compute rejects the default project" = hasFailure "must not be \"default\"" (hostWith [
      static
      { strata.compute.incus.project = "default"; }
    ]);
    "compute rejects a malformed project name" =
      rejects (hostWith [ { strata.compute.incus.project = "Strata_1"; } ])
        [
          "strata"
          "compute"
          "incus"
          "project"
        ];
    "agent on a compute host reaches only loopback, with Incus credentials" =
      let
        a = hc.systemd.services.strata-agent;
        s = a.serviceConfig;
      in
      s.IPAddressDeny == "any"
      && s.IPAddressAllow == "localhost"
      && lib.elem "AF_INET" s.RestrictAddressFamilies
      && lib.any (lib.hasPrefix "incus-client.key:/var/lib/strata-incus/") s.LoadCredential
      && lib.hasInfix "--incus-address 127.0.0.1:8443 --incus-project strata --incus-credentials %d" s.ExecStart
      && !(lib.elem "incus-admin" (a.serviceConfig.SupplementaryGroups or [ ]));
    "agent without compute has no Incus access" =
      !(agentService ? LoadCredential)
      && !(agentService ? IPAddressAllow)
      && !(lib.hasInfix "--incus" agentService.ExecStart);
    "compute does not hardcode vendor KVM modules" =
      !(lib.elem "kvm-intel" hc.boot.kernelModules) && !(lib.elem "kvm-amd" hc.boot.kernelModules);
    "compute requires strata.enable" = requiresEnable "compute" { };

    # ---- workloads ----
    "workloads: a valid declaration has no failed assertions" = failedAssertions workloads == [ ];
    "workloads: IR header and host context" =
      wir.apiVersion == "ir.strata.dev/v1alpha1"
      && wir.node == "hv-01"
      && wir.generator.strataVersion == workloads.config.strata.package.version
      &&
        wir.host == {
          project = "strata";
          defaultPool = "fast";
          pools = [
            {
              name = "bulk";
              driver = "lvm";
            }
            {
              name = "fast";
              driver = "zfs";
            }
          ];
          networks = [
            { name = "dmz"; }
            { name = "lan"; }
          ];
          imageServers = [ "images.linuxcontainers.org" ];
          limits = { };
        };
    "workloads: resources sorted by ID, with type, dependencies and lifecycle" =
      map (r: r.id) wir.resources == [
        "vm/db"
        "vm/web"
      ]
      && wres."vm/db".type == "vm"
      &&
        wres."vm/db".dependsOn == [
          "pool/bulk"
          "pool/fast"
        ]
      &&
        wres."vm/web".dependsOn == [
          "network/dmz"
          "network/lan"
          "pool/fast"
        ]
      && wres."vm/db".lifecycle.protected
      && !wres."vm/web".lifecycle.protected;
    "workloads: spec is vm.strata.dev/v1alpha1 with root first, then disks and NICs by name" =
      wres."vm/db".spec == {
        apiVersion = "vm.strata.dev/v1alpha1";
        name = "db";
        resources = {
          cpu = 2;
          memory = "2GiB";
        };
        disks = [
          {
            name = "root";
            size = "10GiB";
            pool = "fast";
          }
          {
            name = "data";
            size = "5GiB";
            pool = "bulk";
          }
          {
            name = "logs";
            size = "1GiB";
            pool = "bulk";
          }
        ];
      }
      &&
        map (n: n.name) wres."vm/web".spec.networks == [
          "eth0"
          "eth1"
        ]
      && wres."vm/web".spec.autostart
      && wres."vm/web".spec.imageServer == "images.linuxcontainers.org";
    "workloads: no workloads gives an IR without resources" =
      (workloadsWith { vms = { }; }).config.strata.ir.resources == [ ]
      &&
        failedAssertions (workloadsWith {
          vms = { };
        }) == [ ];
    "workloads: the IR is the same on every evaluation" =
      builtins.toJSON wir == builtins.toJSON
        (workloadsWith {
          compute.imageServers = [ "images.linuxcontainers.org" ];
          inherit (workloads.config.strata.workloads) vms;
        }).config.strata.ir;
    "workloads: the IR does not change the system" =
      workloads.config.system.build.toplevel.drvPath == (workloadsWith {
        compute.imageServers = [ "images.linuxcontainers.org" ];
        vms = { };
      }).config.system.build.toplevel.drvPath;
    "workloads: evaluating the IR fails when an assertion fails" =
      throws
        (workloadsWith (oneVM {
          disks.root.pool = "nope";
        })).config.strata.ir;
    "workloads: image servers are sorted and unique in the IR" =
      (workloadsWith {
        compute.imageServers = [
          "images.linuxcontainers.org"
          "cloud-images.example.org"
          "images.linuxcontainers.org"
        ];
        vms = { };
      }).config.strata.ir.host.imageServers == [
        "cloud-images.example.org"
        "images.linuxcontainers.org"
      ];
    "workloads: limits are in the IR when set" =
      (workloadsWith (
        oneVM { }
        // {
          compute.limits = {
            cpu = 8;
            memory = "16GiB";
          };
        }
      )).config.strata.ir.host.limits == {
        cpu = 8;
        memory = "16GiB";
      };
    "workloads: undeclared pool fails" =
      workloadFails ''vm1.disks.root.pool: "nope" is not declared in strata.storage.pools''
        (oneVM {
          disks.root.pool = "nope";
        });
    "workloads: no pool and no default pool fails" =
      workloadFails "vm1.disks.root.pool: not set and there is no strata.storage.defaultPool"
        (oneVM { } // { defaultPool = null; });
    "workloads: undeclared network fails" =
      workloadFails ''vm1.networks.eth0.network: "wan" is not declared''
        (oneVM {
          networks.eth0.network = "wan";
        });
    "workloads: image server not allowed fails" =
      workloadFails ''vm1.imageServer: "images.example.org" is not in strata.compute.incus.imageServers''
        (oneVM {
          image = "x";
          imageServer = "images.example.org";
        });
    "workloads: image server without image fails" = workloadFails "vm1: imageServer needs image" (
      oneVM { imageServer = "images.linuxcontainers.org"; }
      // {
        compute.imageServers = [ "images.linuxcontainers.org" ];
      }
    );
    "workloads: missing root disk fails" = workloadFails ''vm1: a disk named "root"'' {
      vms.vm1 = vmBase // {
        disks.data.size = "1GiB";
      };
    };
    "workloads: small root disk and memory fail" =
      workloadFails "root disk needs at least 1GiB" (oneVM {
        disks.root.size = "512MiB";
      })
      && workloadFails "resources.memory: at least 128MiB" (oneVM {
        resources = {
          cpu = 1;
          memory = "64MiB";
        };
      });
    "workloads: invalid names fail" =
      workloadFails "strata.workloads.vms.Web_1: the name" { vms.Web_1 = vmBase; }
      && workloadFails "vm1.disks.Data: the name" (oneVM {
        disks = {
          root.size = "10GiB";
          Data.size = "1GiB";
        };
      })
      && workloadFails "vm1.networks.eth-0: the name" (oneVM {
        networks.eth-0.network = "lan";
      })
      && workloadFails "vm1.disks.eth0: the name" (oneVM {
        disks = {
          root.size = "10GiB";
          eth0.size = "1GiB";
        };
        networks.eth0.network = "lan";
      });
    "workloads: option types reject bad sizes and CPU counts" =
      throws
        (workloadsWith (oneVM {
          disks.root.size = "10G";
        })).config.strata.ir
      &&
        throws
          (workloadsWith (oneVM {
            resources = {
              cpu = 0;
              memory = "1GiB";
            };
          })).config.strata.ir;
    "workloads: project limits are checked" =
      workloadFails "3 VMs exceed strata.compute.incus.limits.instances (2)" {
        compute.limits.instances = 2;
        vms = {
          a = vmBase;
          b = vmBase;
          c = vmBase;
        };
      }
      && workloadFails "4 CPUs in total exceed strata.compute.incus.limits.cpu (3)" {
        compute.limits.cpu = 3;
        vms = {
          a = vmBase;
          b = vmBase;
        };
      }
      && workloadFails "memory in total exceeds strata.compute.incus.limits.memory (3GiB)" {
        compute.limits.memory = "3GiB";
        vms = {
          a = vmBase;
          b = vmBase;
        };
      }
      &&
        failedAssertions (workloadsWith {
          compute.limits = {
            instances = 2;
            cpu = 4;
            memory = "4GiB";
          };
          vms = {
            a = vmBase;
            b = vmBase;
          };
        }) == [ ];
    "workloads: require the compute role" =
      hasFailure "strata.workloads.vms requires strata.compute.enable"
        (hostWith [
          static
          {
            strata = {
              compute.enable = lib.mkForce false;
              workloads.vms.vm1 = vmBase;
              storage = {
                defaultPool = "fast";
                pools.fast = pool "zfs" { devices = [ disk1 ]; };
              };
            };
          }
        ]);

    # ---- storage ----
    "storage: valid pools have no failed assertions" = failedAssertions storage == [ ];
    "storage: setup state lists the pools as declared" =
      let
        byName = lib.listToAttrs (map (p: lib.nameValuePair p.name p) state.pools);
      in
      lib.length state.pools == 3
      && byName.fast.layout == "mirror"
      &&
        byName.fast.devices == [
          disk1
          disk2
        ]
      && byName.fast.limit == "1TiB"
      && byName.bulk.limit == ""
      && byName.scratch.path == "/srv/scratch"
      && state.defaultPool == "fast";
    "storage: snapshots allowed, backups blocked" =
      state.projectConfig."restricted.snapshots" == "allow"
      && state.projectConfig."restricted.backups" == "block";
    "storage: project limits only when set" =
      let
        limited = hostWith [
          static
          {
            strata.compute.incus.limits = {
              cpu = 16;
              memory = "64GiB";
              instances = 20;
            };
          }
        ];
        pc = limited.config.strata.compute.incus.setupState.projectConfig;
      in
      pc."limits.cpu" == "16"
      && pc."limits.memory" == "64GiB"
      && pc."limits.instances" == "20"
      && !(pc ? "limits.disk")
      && !(state.projectConfig ? "limits.cpu");
    "storage: ZFS only with a zfs pool, hostId from the node ID" =
      sc.boot.supportedFilesystems.zfs or false
      && sc.networking.hostId == builtins.substring 0 8 (builtins.hashString "sha256" "hv-01")
      && !(hc.boot.supportedFilesystems.zfs or false);
    "storage: LVM pools load the device-mapper thin and snapshot targets" =
      lib.all (m: lib.elem m sc.boot.kernelModules) [
        "dm_thin_pool"
        "dm_snapshot"
      ]
      && !(lib.elem "dm_thin_pool" hc.boot.kernelModules);
    "storage: dir pool mount is required by setup" =
      lib.elem "/srv/scratch" sc.systemd.services.strata-incus-setup.unitConfig.RequiresMountsFor;
    "storage: LVM autoactivation limited to declared pools on Strata disks" =
      lib.hasInfix "auto_activation_volume_list = [ ]" (lvmConf diskHost)
      && lib.hasInfix "auto_activation_volume_list = [ ]" (
        diskHost.config.boot.initrd.systemd.contents."/etc/lvm/lvm.conf".text or ""
      )
      && !(lib.hasInfix "auto_activation_volume_list" (lvmConf host))
      && lib.hasInfix ''auto_activation_volume_list = [ "bulk" ]'' (
        lvmConf (hostWith [
          static
          {
            strata.storage = {
              pools.bulk = pool "lvm" { devices = [ disk3 ]; };
              lvm.autoActivation = "declared";
            };
          }
        ])
      );
    "storage: no pools means no storage effects" =
      failedAssertions host == [ ] && hc.strata.compute.incus.setupState.pools == [ ];
    "storage: a limited default pool needs rootSize" =
      storageFails "strata.storage.rootSize must be set"
        {
          defaultPool = "a";
          pools.a = pool "zfs" {
            devices = [ disk1 ];
            limit = "1TiB";
          };
        };
    "storage: setup state carries rootSize" = state.rootSize == "20GiB";
    "storage: mirror needs two devices" = storageFails "a zfs pool needs one device" {
      pools.p = pool "zfs" {
        devices = [ disk1 ];
        zfs.layout = "mirror";
      };
    };
    "storage: lvm needs exactly one device" = storageFails "an lvm pool needs exactly one device" {
      pools.p = pool "lvm" {
        devices = [
          disk1
          disk2
        ];
      };
    };
    "storage: dir needs a declared mount point" = storageFails "a dir pool needs `path`" {
      pools.p = pool "dir" { path = "/srv/elsewhere"; };
    };
    "storage: dir may not be a system mount" = storageFails "a dir pool needs `path`" {
      pools.p = pool "dir" { path = "/boot"; };
    };
    "storage: a device in two pools fails" = storageFails "devices used by more than one pool" {
      pools.a = pool "zfs" { devices = [ disk1 ]; };
      pools.b = pool "lvm" { devices = [ disk1 ]; };
    };
    "storage: unknown default pool fails" = storageFails "is not a declared pool" {
      defaultPool = "nope";
      pools.a = pool "zfs" { devices = [ disk1 ]; };
    };
    "storage: reserved pool name fails" = storageFails "is reserved" {
      pools.strata-no-pools = pool "zfs" { devices = [ disk1 ]; };
    };
    "storage: the system disk cannot hold a pool" =
      hasFailure "the system disk (strata.disk.device) cannot hold a storage pool"
        (hostWith [
          static
          {
            strata.disk = {
              enable = true;
              device = disk1;
            };
            strata.storage.pools.p = pool "zfs" { devices = [ disk1 ]; };
          }
        ]);
    "storage: pools require compute" =
      hasFailure "strata.storage pools require strata.compute.enable"
        (eval [
          {
            strata = {
              enable = true;
              node.id = "hv-01";
              storage.pools.p = pool "zfs" { devices = [ disk1 ]; };
            };
          }
        ]);
    "storage: devices must be by-id paths" =
      rejects (storageWith { pools.p = pool "zfs" { devices = [ "/dev/sdb" ]; }; })
        [
          "strata"
          "storage"
          "pools"
          "p"
          "devices"
        ];
    "storage: limits must be sizes" =
      rejects
        (storageWith {
          pools.p = pool "zfs" {
            devices = [ disk1 ];
            limit = "lots";
          };
        })
        [
          "strata"
          "storage"
          "pools"
          "p"
          "limit"
        ];

    # ---- guest networks ----
    "guest: valid networks have no failed assertions" = failedAssertions guest == [ ];
    "guest: setup state lists networks and the management interface" =
      let
        s = gc.strata.compute.incus.setupState;
        byName = lib.listToAttrs (map (n: lib.nameValuePair n.name n) s.networks);
      in
      byName.lan.uplink == "eno2"
      && byName.lan.vlan == 0
      && byName.v20.vlan == 20
      && byName.lab.uplink == ""
      && s.defaultNetwork == "lan"
      && s.managementInterface == "eno1";
    "guest: uplinks are up without any address" =
      let
        u = gc.systemd.network.networks."20-strata-uplink-eno3";
      in
      u.matchConfig.Name == "eno3"
      && u.networkConfig.DHCP == "no"
      && u.networkConfig.LinkLocalAddressing == "no"
      && u.networkConfig.IPv6AcceptRA == false
      && (u.address or [ ]) == [ ]
      && u.linkConfig.RequiredForOnline == "no"
      && u.networkConfig.KeepMaster
      && gc.systemd.network.networks ? "20-strata-uplink-eno2";
    "guest: Incus VLAN interfaces get no link-local address" =
      let
        v = gc.systemd.network.networks."21-strata-guest-vlans";
      in
      v.matchConfig.Name == "eno3.20 eno3.30"
      && v.networkConfig.LinkLocalAddressing == "no"
      && v.linkConfig.ActivationPolicy == "manual";
    "guest: bridges are exempt from the reverse-path filter" =
      lib.hasInfix ''iifname { "lab", "lan", "v20", "v30" } accept'' gc.networking.firewall.extraReversePathFilterRules;
    "guest: no networks means no uplink units or exemptions" =
      !(lib.any (lib.hasPrefix "20-strata-uplink") (lib.attrNames hc.systemd.network.networks))
      && hc.networking.firewall.extraReversePathFilterRules == "";
    "guest: the management interface cannot be an uplink" =
      guestFails "the management interface (eno1) cannot be a guest uplink"
        { networks.bad.uplink = "eno1"; };
    "guest: an uplink is untagged for one network or carries VLANs" =
      guestFails "is either untagged for one network or carries VLANs"
        {
          networks.a.uplink = "eno2";
          networks.b = {
            uplink = "eno2";
            vlan = 5;
          };
        };
    "guest: a VLAN ID is used once per uplink" = guestFails "a VLAN ID is used twice" {
      networks.a = {
        uplink = "eno2";
        vlan = 5;
      };
      networks.b = {
        uplink = "eno2";
        vlan = 5;
      };
    };
    "guest: a VLAN needs an uplink" = guestFails "a VLAN needs an uplink" { networks.a.vlan = 5; };
    "guest: VLAN interface names fit 15 characters" = guestFails "is longer than 15 characters" {
      networks.a = {
        uplink = "enp129s0f1np1";
        vlan = 100;
      };
    };
    "guest: unknown default network fails" = guestFails "is not a declared network" {
      defaultNetwork = "nope";
      networks.a = { };
    };
    "guest: networks require compute" =
      hasFailure "strata.networking.guest requires strata.compute.enable"
        (eval [
          {
            strata = {
              enable = true;
              node.id = "hv-01";
              networking = {
                enable = true;
                management.interface = "eno1";
                management.addressing = "dhcp";
                guest.networks.a = { };
              };
            };
          }
        ]);
    "guest: network names are interface names" =
      rejects (guestWith { networks."lan.20" = { }; }) [
        "strata"
        "networking"
        "guest"
        "networks"
        "lan.20"
        "name"
      ]
      && rejects (guestWith { networks.averyveryverylongname = { }; }) [
        "strata"
        "networking"
        "guest"
        "networks"
        "averyveryverylongname"
        "name"
      ];
    "guest: VLAN IDs are 1-4094" = rejects (guestWith { networks.a.vlan = 4095; }) [
      "strata"
      "networking"
      "guest"
      "networks"
      "a"
      "vlan"
    ];

    # ---- local API (VMs) ----
    "api: the agent gets its socket from systemd, group strata-operators" =
      let
        sock = hc.systemd.sockets.strata-agent.socketConfig;
        svc = hc.systemd.services.strata-agent;
      in
      sock.ListenStream == "/run/strata/agent.sock"
      && sock.SocketGroup == "strata-operators"
      && sock.SocketMode == "0660"
      && svc.serviceConfig.Sockets == [ "strata-agent.socket" ]
      && lib.elem "strata-agent.socket" svc.requires
      && lib.hasInfix "--api-group strata-operators" svc.serviceConfig.ExecStart
      && hc.users.groups ? strata-operators;
    "api: the admin is a strata operator" =
      lib.elem "strata-operators" hc.users.users.strata-admin.extraGroups;
    "api: the agent sandbox still allows only loopback" =
      let
        s = hc.systemd.services.strata-agent.serviceConfig;
      in
      s.IPAddressDeny == "any" && s.IPAddressAllow == "localhost" && s.CapabilityBoundingSet == "";
    "api: no socket or group without compute" =
      !(valid.config.systemd.sockets ? strata-agent) && !(valid.config.users.groups ? strata-operators);
    "api: no remote image servers by default" =
      state.projectConfig."restricted.images.servers" == "strata-no-image-servers.invalid";
    "api: declared image servers are allowed" =
      (hostWith [
        static
        {
          strata.compute.incus.imageServers = [
            "images.linuxcontainers.org"
            "images.example.org"
          ];
        }
      ]).config.strata.compute.incus.setupState.projectConfig."restricted.images.servers"
      == "images.linuxcontainers.org,images.example.org";
    "api: image servers are host names, not URLs" =
      rejects (hostWith [ { strata.compute.incus.imageServers = [ "https://images.example.org" ]; } ])
        [
          "strata"
          "compute"
          "incus"
          "imageServers"
        ];

    # ---- stable interface names ----
    "nic: a pin becomes a .link file in the system and the initrd" =
      let
        l = pc.systemd.network.links."10-strata-eno1";
      in
      l.matchConfig.PermanentMACAddress == "84:47:09:a3:5d:dc"
      && l.linkConfig.Name == "eno1"
      && pc.boot.initrd.systemd.network.links ? "10-strata-eno2";
    "nic: pinned management and uplinks are matched by MAC address" =
      pc.systemd.network.networks."10-strata-management".matchConfig == {
        PermanentMACAddress = "84:47:09:a3:5d:dc";
      }
      &&
        pc.systemd.network.networks."20-strata-uplink-eno2".matchConfig == {
          PermanentMACAddress = "84:47:09:a3:5d:de";
        };
    "nic: the firewall keeps the (stable) name" = pc.networking.firewall.interfaces ? eno1;
    "nic: pinned interfaces give no warning" = !(lib.any (lib.hasInfix "not pinned") pc.warnings);
    "nic: an unpinned management interface or uplink gives a warning" =
      lib.any (lib.hasInfix "management.interface eno1 is not pinned") hc.warnings
      && lib.any (lib.hasInfix "uplink eno2 is not pinned") gc.warnings
      && hc.systemd.network.networks."10-strata-management".matchConfig == { Name = "eno1"; };
    "nic: a MAC address pinned twice fails" = hasFailure "MAC addresses pinned twice" (hostWith [
      static
      {
        strata.networking.interfaces = {
          a.macAddress = "84:47:09:a3:5d:dc";
          b.macAddress = "84:47:09:a3:5d:dc";
        };
      }
    ]);
    "nic: MAC addresses are lowercase colon-separated" =
      rejects (hostWith [ { strata.networking.interfaces.eno1.macAddress = "84-47-09-A3-5D-DC"; } ])
        [
          "strata"
          "networking"
          "interfaces"
          "eno1"
          "macAddress"
        ];
    "nic: no pins, no link files" = !(hc.systemd.network.links ? "10-strata-eno1");

    # ---- access ----
    "access hardens sshd" =
      let
        s = hc.services.openssh.settings;
      in
      hc.services.openssh.enable
      && !s.PasswordAuthentication
      && !s.KbdInteractiveAuthentication
      && s.PermitRootLogin == "no"
      && s.AllowUsers == [ "strata-admin" ];
    "access creates a locked wheel admin with keys" =
      let
        u = hc.users.users.strata-admin;
      in
      u.isNormalUser
      && lib.elem "wheel" u.extraGroups
      && u.hashedPassword == "!"
      && u.openssh.authorizedKeys.keys == [ testKey ];
    "access grants passwordless sudo to the admin only" = lib.any (
      r: r.users == [ "strata-admin" ] && lib.any (c: lib.elem "NOPASSWD" c.options) r.commands
    ) hc.security.sudo.extraRules;
    "access without keys fails an assertion" =
      hasFailure "authorizedKeys must contain at least one key"
        (eval [
          {
            strata = {
              enable = true;
              node.id = "hv-01";
              access.enable = true;
            };
          }
        ]);
    "access rejects root as admin" = rejects (eval [ { strata.access.admin.name = "root"; } ]) [
      "strata"
      "access"
      "admin"
      "name"
    ];
    "access accepts a custom admin name" =
      (hostWith [
        static
        { strata.access.admin.name = "ops"; }
      ]).config.users.users
        ? ops;
    "access rejects invalid admin names" =
      rejects
        (eval [
          { strata.access.admin.name = "Admin User"; }
        ])
        [
          "strata"
          "access"
          "admin"
          "name"
        ];
    "access rejects malformed keys" =
      rejects
        (eval [
          { strata.access.admin.authorizedKeys = [ "not-a-key" ]; }
        ])
        [
          "strata"
          "access"
          "admin"
          "authorizedKeys"
        ];
    "access requires strata.enable" = requiresEnable "access" { admin.authorizedKeys = [ testKey ]; };
    "access sets a console password from a file, SSH stays key-only" =
      let
        c =
          (hostWith [
            static
            { strata.access.admin.consolePasswordFile = "/etc/strata/admin-console-password"; }
          ]).config;
        u = c.users.users.strata-admin;
      in
      u.hashedPasswordFile == "/etc/strata/admin-console-password"
      && u.hashedPassword == null
      && !c.services.openssh.settings.PasswordAuthentication
      && !c.services.openssh.settings.KbdInteractiveAuthentication;
    "access rejects a relative console password file" =
      rejects
        (eval [
          { strata.access.admin.consolePasswordFile = "admin-console-password"; }
        ])
        [
          "strata"
          "access"
          "admin"
          "consolePasswordFile"
        ];

    # ---- networking ----
    "networking uses systemd-networkd and nftables" =
      hc.networking.useNetworkd && !hc.networking.useDHCP && hc.networking.nftables.enable;
    "networking generates a static management unit" =
      mgmtUnit.matchConfig.Name == "eno1"
      && mgmtUnit.networkConfig.DHCP == "no"
      &&
        mgmtUnit.address == [
          "192.0.2.10/24"
          "2001:db8::10/64"
        ]
      && mgmtUnit.gateway == [ "192.0.2.1" ]
      && mgmtUnit.networkConfig.DNS == [ "192.0.2.53" ];
    "networking generates a DHCP management unit" =
      let
        u = dhcpHost.config.systemd.network.networks."10-strata-management";
      in
      u.networkConfig.DHCP == "yes" && u.address == [ ];
    "networking opens SSH and exporter only on the management interface" =
      hc.networking.firewall.interfaces.eno1.allowedTCPPorts == [
        22
        9100
      ]
      && hc.networking.firewall.allowedTCPPorts == [ ];
    "networking without access/monitoring opens nothing" =
      (eval [
        {
          strata = {
            enable = true;
            node.id = "hv-01";
            networking = {
              enable = true;
              management.interface = "eno1";
              management.addressing = "dhcp";
            };
          };
        }
      ]).config.networking.firewall.interfaces.eno1.allowedTCPPorts == [ ];
    "networking rejects globally opened ports" = hasFailure "Globally open: tcp/8080" (hostWith [
      static
      { networking.firewall.allowedTCPPorts = [ 8080 ]; }
    ]);
    "networking requires a management interface" = hasFailure "management.interface must be set" (eval [
      {
        strata = {
          enable = true;
          node.id = "hv-01";
          networking.enable = true;
        };
      }
    ]);
    "networking static without addresses fails" = hasFailure "addresses must not be empty" (
      hostWith [ ]
    );
    "networking DHCP with static settings fails" =
      hasFailure "only allowed with static addressing"
        (hostWith [
          static
          { strata.networking.management.addressing = "dhcp"; }
        ]);
    "networking rejects invalid interface names" =
      rejects
        (eval [
          { strata.networking.management.interface = "this-name-is-too-long"; }
        ])
        [
          "strata"
          "networking"
          "management"
          "interface"
        ];
    "networking rejects addresses without prefix" =
      rejects
        (eval [
          { strata.networking.management.addresses = [ "192.0.2.10" ]; }
        ])
        [
          "strata"
          "networking"
          "management"
          "addresses"
        ];
    "networking requires strata.enable" = requiresEnable "networking" {
      management.interface = "eno1";
      management.addressing = "dhcp";
    };

    # ---- monitoring ----
    "monitoring enables node exporter without opening the firewall globally" =
      hc.services.prometheus.exporters.node.enable
      && hc.services.prometheus.exporters.node.port == 9100
      && !hc.services.prometheus.exporters.node.openFirewall;
    "monitoring requires networking" = hasFailure "requires strata.networking.enable" (eval [
      {
        strata = {
          enable = true;
          node.id = "hv-01";
          monitoring.enable = true;
        };
      }
    ]);
    "monitoring requires strata.enable" = requiresEnable "monitoring" { };

    # ---- disk ----
    "disk generates an ESP and a root partition filling the device" =
      let
        d = diskHost.config.disko.devices.disk.system;
        p = d.content.partitions;
      in
      d.device == "/dev/disk/by-id/test-disk"
      && d.content.type == "gpt"
      && p.ESP.type == "EF00"
      && p.ESP.size == "1G"
      && p.root.size == "100%";
    "disk mounts root and /boot by partition label" =
      let
        fs = diskHost.config.fileSystems;
      in
      fs."/".fsType == "ext4"
      && fs."/".device == "/dev/disk/by-partlabel/disk-system-root"
      && fs."/boot".fsType == "vfat"
      && fs."/boot".device == "/dev/disk/by-partlabel/disk-system-ESP"
      && lib.elem "umask=0077" fs."/boot".options;
    "disk boots with systemd-boot, not GRUB" =
      let
        l = diskHost.config.boot.loader;
      in
      l.systemd-boot.enable && l.systemd-boot.configurationLimit == 10 && !l.grub.enable;
    "full disk host has no failed assertions" = failedAssertions diskHost == [ ];
    "disk without a device fails an assertion" = hasFailure "strata.disk.device must be set" (hostWith [
      static
      { strata.disk.enable = true; }
    ]);
    "disk rejects device paths outside /dev" =
      rejects
        (eval [
          { strata.disk.device = "sda"; }
        ])
        [
          "strata"
          "disk"
          "device"
        ];
    "disk rejects unknown layouts" =
      rejects
        (eval [
          { strata.disk.layout = "zfs"; }
        ])
        [
          "strata"
          "disk"
          "layout"
        ];
    "disk requires strata.enable" = requiresEnable "disk" { device = "/dev/vda"; };
    "disabled disk defines no partitions" = hc.disko.devices.disk == { };

    # ---- host template ----
    "unfilled template fails on exactly the values left to the user" =
      let
        failures = failedAssertions (eval templateModules);
        mentions = needle: lib.any (lib.hasInfix needle) failures;
      in
      lib.length failures == 3
      && mentions "authorizedKeys"
      && mentions "strata.disk.device"
      && mentions "management.interface";
    "reference host has no failed assertions" =
      failedAssertions (eval (templateModules ++ [ ../hosts/reference.nix ])) == [ ];
    # strata-install output (Go golden files) is a valid host.nix.
    "installer-generated host.nix files evaluate" =
      lib.all
        (
          golden:
          failedAssertions (eval [
            ../../templates/host/hardware-configuration.nix
            ../../templates/host/configuration.nix
            golden
          ]) == [ ]
        )
        [
          ../../internal/install/testdata/host-static.nix
          ../../internal/install/testdata/host-dhcp.nix
        ];

    # ---- base and agent ----
    "valid config has no failed assertions" = failedAssertions valid == [ ];
    "strata enables flakes" = lib.all (f: lib.elem f valid.config.nix.settings.experimental-features) [
      "nix-command"
      "flakes"
    ];
    "node.id becomes the hostname" = valid.config.networking.hostName == "hv-01";
    "node.json contains identity" =
      builtins.fromJSON valid.config.environment.etc."strata/node.json".text == {
        apiVersion = "node.strata.dev/v1alpha1";
        id = "hv-01";
        role = "hypervisor";
      };
    "agent runs as a dynamic user without capabilities or network" =
      agentService.DynamicUser
      && agentService.CapabilityBoundingSet == ""
      && agentService.RestrictAddressFamilies == [ "AF_UNIX" ]
      && agentService.IPAddressDeny == "any";
    "disabled strata changes nothing" =
      !(disabled.config.systemd.services ? strata-agent)
      && !(disabled.config.environment.etc ? "strata/node.json");
    "missing node.id fails an assertion" = hasFailure "strata.node.id must be set" (eval [
      { strata.enable = true; }
    ]);
    "agent without strata.enable fails an assertion" =
      hasFailure "strata.agent.enable requires strata.enable"
        (eval [
          {
            strata = {
              node.id = "hv-01";
              agent.enable = true;
            };
          }
        ]);
    "node.id accepts a 63-character label" =
      !(throws (withNode (lib.strings.replicate 63 "a")).config.strata.node.id);
    "node.id rejects uppercase and underscores" = throws (withNode "Bad_ID").config.strata.node.id;
    "node.id rejects a leading hyphen" = throws (withNode "-hv").config.strata.node.id;
    "node.id rejects 64 characters" = throws (withNode (lib.strings.replicate 64 "a"))
      .config.strata.node.id;
    "node.role rejects unknown roles" =
      throws
        (eval [ { strata.node.role = "storage"; } ]).config.strata.node.role;
  };

  failed = lib.attrNames (lib.filterAttrs (_: ok: !ok) cases);
in
assert lib.assertMsg (failed == [ ]) (
  "Strata module evaluation tests failed:\n  - " + lib.concatStringsSep "\n  - " failed
);
pkgs.writeText "strata-module-eval-results" (
  lib.concatMapStringsSep "\n" (name: "PASS ${name}") (lib.attrNames cases) + "\n"
)
