# Workload storage pools on dedicated disks (ADR-0018).
#
# Pools are declared here and created by strata-incus-setup (strata-host)
# as Incus storage pools: only on disks that are verifiably empty, or by
# adopting an existing, empty zpool or volume group named like the pool.
# Strata never erases a disk and never deletes or modifies an existing
# pool; removing a pool from this configuration closes it to the Strata
# project and leaves it and its data in place.
#
# Host effects:
# - ZFS is enabled only if a zfs pool is declared, with networking.hostId
#   derived from the node ID.
# - LVM autoactivation is limited to the volume groups of declared lvm
#   pools, so leftover volume groups on other disks stay inactive.
{
  config,
  lib,
  ...
}:
let
  cfg = config.strata;
  st = cfg.storage;
  pools = lib.attrValues st.pools;
  poolsOf = driver: lib.filter (p: p.driver == driver) pools;
  lvmVGs = lib.concatMapStringsSep ", " (p: "\"${p.name}\"") (poolsOf "lvm");
  allDevices = lib.concatMap (p: p.devices) pools;
  duplicates = lib.unique (lib.filter (d: lib.count (x: x == d) allDevices > 1) allDevices);
  systemMounts = [
    "/"
    "/boot"
    "/nix"
    "/nix/store"
  ];
  byId = lib.types.strMatching "/dev/disk/by-id/[^/]+";
  # The kernel's ZFS module, if any (CLAUDE.md §6: ZFS is not available
  # with every kernel).
  zfsModule =
    let
      attr = config.boot.zfs.package.kernelModuleAttribute or null;
    in
    if attr == null then null else config.boot.kernelPackages.${attr} or null;

  poolModule =
    { name, ... }:
    {
      options = {
        name = lib.mkOption {
          type = lib.types.strMatching "[a-z][a-z0-9-]{0,30}";
          default = name;
          readOnly = true;
          description = "Pool name (the attribute name): lowercase letters, digits and hyphens.";
        };
        driver = lib.mkOption {
          type = lib.types.enum [
            "zfs"
            "lvm"
            "dir"
          ];
          description = "Incus storage driver.";
        };
        devices = lib.mkOption {
          type = lib.types.listOf byId;
          default = [ ];
          description = ''
            Whole disks for a zfs (one, or two with layout "mirror") or lvm
            (one) pool, by /dev/disk/by-id/ name. They must be empty unless
            the pool is adopted.
          '';
        };
        zfs.layout = lib.mkOption {
          type = lib.types.enum [
            "single"
            "mirror"
          ];
          default = "single";
          description = "ZFS layout. Other layouts (raidz): create the zpool by hand and adopt it.";
        };
        path = lib.mkOption {
          type = lib.types.nullOr (lib.types.strMatching "/.+");
          default = null;
          description = ''
            For a dir pool: a mount point declared in fileSystems, on its own
            disk. The pool lives in PATH/NAME.
          '';
        };
        adopt = lib.mkOption {
          type = lib.types.bool;
          default = false;
          description = ''
            Use an existing, empty zpool or volume group named like the pool
            on the given devices instead of creating one.
          '';
        };
        limit = lib.mkOption {
          type = lib.types.nullOr (lib.types.strMatching "[0-9]+(B|KiB|MiB|GiB|TiB|PiB)");
          default = null;
          description = "Most disk space the Strata project may use on this pool, e.g. \"1TiB\".";
        };
      };
    };
in
{
  options.strata.storage = {
    pools = lib.mkOption {
      type = lib.types.attrsOf (lib.types.submodule poolModule);
      default = { };
      description = "Workload storage pools (Incus), on dedicated disks.";
    };
    defaultPool = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Pool that holds the root disk of new instances (the project's default profile).";
    };
    rootSize = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "[0-9]+(B|KiB|MiB|GiB|TiB|PiB)");
      default = null;
      example = "20GiB";
      description = ''
        Root disk size of new instances (the default profile). Required when
        the default pool or the project has a disk limit: Incus counts
        limits by the sizes given to instances.
      '';
    };
    lvm.autoActivation = lib.mkOption {
      type = lib.types.enum [
        "declared"
        "all"
      ];
      default = if cfg.disk.enable then "declared" else "all";
      defaultText = lib.literalExpression ''if config.strata.disk.enable then "declared" else "all"'';
      description = ''
        Which LVM volume groups are activated automatically at boot.
        "declared": only those of declared lvm pools, so leftover volume
        groups on other disks stay inactive. "all": every volume group (the
        NixOS default), for hosts whose own filesystems use LVM. The
        default is "declared" when Strata manages the system disk
        (strata.disk, which uses no LVM).
      '';
    };
  };

  config = lib.mkMerge [
    (lib.mkIf (pools != [ ] || st.defaultPool != null) {
      assertions = [
        {
          assertion = cfg.compute.enable;
          message = "strata.storage pools require strata.compute.enable.";
        }
        {
          assertion = st.defaultPool == null || st.pools ? ${st.defaultPool};
          message = "strata.storage.defaultPool \"${toString st.defaultPool}\" is not a declared pool.";
        }
        {
          assertion = duplicates == [ ];
          message = "strata.storage: devices used by more than one pool: ${lib.concatStringsSep ", " duplicates}";
        }
        {
          assertion = !(cfg.disk.enable && lib.elem cfg.disk.device allDevices);
          message = "strata.storage: the system disk (strata.disk.device) cannot hold a storage pool.";
        }
        {
          assertion =
            st.rootSize != null
            || st.defaultPool == null
            || (cfg.compute.incus.limits.disk == null && (st.pools.${st.defaultPool}.limit or null) == null);
          message = "strata.storage.rootSize must be set when the default pool or the project has a disk limit (Incus needs a size to count against it).";
        }
        {
          assertion = !(st.pools ? strata-no-pools);
          message = "strata.storage: the pool name \"strata-no-pools\" is reserved.";
        }
      ]
      ++ map (p: {
        assertion =
          if p.driver == "dir" then
            p.devices == [ ]
            && p.path != null
            && config.fileSystems ? ${p.path}
            && !(lib.elem p.path systemMounts)
          else
            p.path == null
            && (
              if p.driver == "lvm" then
                lib.length p.devices == 1 && p.zfs.layout == "single"
              else if p.zfs.layout == "mirror" then
                lib.length p.devices == 2
              else
                lib.length p.devices == 1
            );
        message =
          "strata.storage.pools.${p.name}: "
          + {
            dir = "a dir pool needs `path`, a mount point declared in fileSystems (not /, /boot or /nix), and no devices.";
            lvm = "an lvm pool needs exactly one device and no path.";
            zfs = "a zfs pool needs one device (layout \"single\") or two (layout \"mirror\"), and no path.";
          }
          .${p.driver};
      }) pools;
    })

    (lib.mkIf (poolsOf "zfs" != [ ]) {
      assertions = [
        {
          assertion = zfsModule != null && !(zfsModule.meta.broken or false);
          message = "strata.storage: a zfs pool is declared, but ZFS is not available for this kernel (${config.boot.kernelPackages.kernel.version}).";
        }
      ];
      boot.supportedFilesystems.zfs = true;
      # Required by ZFS; stable per node.
      networking.hostId = lib.mkDefault (
        builtins.substring 0 8 (builtins.hashString "sha256" cfg.node.id)
      );
      # Incus imports its zpools itself.
      boot.zfs.forceImportRoot = false;
    })

    (lib.mkIf (poolsOf "lvm" != [ ]) {
      # Incus's LVM driver needs the device-mapper thin and snapshot
      # targets (thin pools; images cloned into instances), which nothing
      # loads on demand.
      boot.kernelModules = [
        "dm_thin_pool"
        "dm_snapshot"
      ];
    })

    (lib.mkIf (cfg.enable && st.lvm.autoActivation == "declared") {
      # Only declared lvm pools' volume groups activate at boot; Incus
      # activates what it needs. An empty list activates nothing.
      environment.etc."lvm/lvm.conf".text = ''
        activation {
          auto_activation_volume_list = [${lib.optionalString (lvmVGs != "") " ${lvmVGs}"} ]
        }
      '';
      # The initrd has its own lvm.conf and would otherwise activate every
      # volume group it finds (and fail on thin pools, whose kernel module
      # it lacks). In this mode no system filesystem uses LVM, so the
      # initrd activates none.
      boot.initrd.systemd.contents."/etc/lvm/lvm.conf".text = ''
        activation {
          auto_activation_volume_list = [ ]
        }
      '';
    })
  ];
}
