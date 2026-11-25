# Declared workloads (M3, ADR-0023): `strata.workloads.vms.<name>`.
#
# These options are data. They do not change the system NixOS builds;
# the Strata controller evaluates `config.strata.ir`, the versioned
# intermediate representation (IR) of this host's desired workloads, and
# applies it through the agent. Nix checks structure and references to the
# host's declared pools, networks, image servers and project limits; Go
# validates the IR again and is the authority on the VM schema
# (internal/ir, internal/workload).
{ config, lib, ... }:
let
  inherit (lib) mkOption types;
  cfg = config.strata;
  incus = cfg.compute.incus;
  vms = cfg.workloads.vms;
  vmNames = lib.attrNames vms;

  sizeType = types.strMatching "[0-9]{1,15}(B|KiB|MiB|GiB|TiB)";
  units = {
    B = 1;
    KiB = 1024;
    MiB = 1024 * 1024;
    GiB = 1024 * 1024 * 1024;
    TiB = 1024 * 1024 * 1024 * 1024;
    PiB = 1024 * 1024 * 1024 * 1024 * 1024;
  };
  toBytes =
    s:
    let
      m = builtins.match "([0-9]+)(B|KiB|MiB|GiB|TiB|PiB)" s;
    in
    lib.toIntBase10 (builtins.head m) * units.${builtins.elemAt m 1};

  # Same patterns as internal/workload (Go is the authority; these give
  # early, readable errors).
  vmNameOk = n: builtins.match "[a-z][a-z0-9-]{0,62}" n != null;
  diskNameOk = n: builtins.match "[a-z][a-z0-9-]{0,30}" n != null;
  nicNameOk = n: builtins.match "[a-z][a-z0-9]{0,14}" n != null;

  diskModule = {
    options = {
      size = mkOption {
        type = sizeType;
        example = "20GiB";
        description = "Disk size (B, KiB, MiB, GiB or TiB). Disks can grow but not shrink.";
      };
      pool = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Storage pool (strata.storage.pools). null: strata.storage.defaultPool.";
      };
    };
  };

  nicModule = {
    options.network = mkOption {
      type = types.str;
      example = "lan";
      description = "Guest network (strata.networking.guest.networks).";
    };
  };

  vmModule = {
    options = {
      resources = {
        cpu = mkOption {
          type = types.ints.between 1 256;
          description = "Number of virtual CPUs.";
        };
        memory = mkOption {
          type = sizeType;
          example = "4GiB";
          description = "Memory (at least 128MiB).";
        };
      };
      image = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "debian-13";
        description = ''
          Image to create the VM from: a local image alias in the Strata
          project, or with imageServer an alias on that server
          ("debian/13/cloud"). null: an empty VM. Only used at creation.
        '';
      };
      imageServer = mkOption {
        type = types.nullOr types.str;
        default = null;
        example = "images.linuxcontainers.org";
        description = "Image server to download image from; must be in strata.compute.incus.imageServers.";
      };
      disks = mkOption {
        type = types.attrsOf (types.submodule diskModule);
        description = ''
          Disks by name. "root" (required) is the boot disk and belongs to
          the VM; the others are data volumes named VM-DISK, which are kept
          when the VM is deleted.
        '';
      };
      networks = mkOption {
        type = types.attrsOf (types.submodule nicModule);
        default = { };
        example = lib.literalExpression ''{ eth0.network = "lan"; }'';
        description = "Network interfaces by device name. Guests see them in name order.";
      };
      autostart = mkOption {
        type = types.bool;
        default = false;
        description = "Start the VM when the host boots.";
      };
      cloudInit.sshAuthorizedKeys = mkOption {
        type = types.listOf types.str;
        default = [ ];
        description = "SSH public keys for the image's default user (images with cloud-init). Only used at creation.";
      };
      lifecycle.protected = mkOption {
        type = types.bool;
        default = false;
        description = ''
          Refuse to delete or restart the VM, even after it is removed
          from the configuration, until this is set to false and applied.
        '';
      };
    };
  };

  # A VM as a vm.strata.dev/v1alpha1 spec, with defaults resolved. Optional
  # fields are left out when empty, as Go marshals them.
  poolOf = d: if d.pool == null then cfg.storage.defaultPool else d.pool;
  diskNames = vm: [ "root" ] ++ lib.filter (n: n != "root") (lib.attrNames vm.disks);
  specOf =
    name: vm:
    {
      apiVersion = "vm.strata.dev/v1alpha1";
      inherit name;
      resources = { inherit (vm.resources) cpu memory; };
      disks = map (d: {
        name = d;
        inherit (vm.disks.${d}) size;
        pool = poolOf vm.disks.${d};
      }) (lib.filter (d: vm.disks ? ${d}) (diskNames vm));
    }
    // lib.optionalAttrs (vm.image != null) { inherit (vm) image; }
    // lib.optionalAttrs (vm.imageServer != null) { inherit (vm) imageServer; }
    // lib.optionalAttrs (vm.networks != { }) {
      networks = lib.mapAttrsToList (n: nic: {
        name = n;
        inherit (nic) network;
      }) vm.networks;
    }
    // lib.optionalAttrs vm.autostart { autostart = true; }
    // lib.optionalAttrs (vm.cloudInit.sshAuthorizedKeys != [ ]) {
      cloudInit.sshAuthorizedKeys = vm.cloudInit.sshAuthorizedKeys;
    };
  dependsOn =
    vm:
    lib.unique (
      lib.sort (a: b: a < b) (
        map (d: "pool/${poolOf d}") (lib.attrValues vm.disks)
        ++ map (n: "network/${n.network}") (lib.attrValues vm.networks)
      )
    );

  ir = {
    apiVersion = "ir.strata.dev/v1alpha1";
    generator.strataVersion = cfg.package.version;
    node = cfg.node.id;
    host = {
      inherit (incus) project;
      imageServers = lib.unique (lib.sort (a: b: a < b) incus.imageServers);
      pools = lib.mapAttrsToList (n: p: {
        name = n;
        inherit (p) driver;
      }) cfg.storage.pools;
      networks = map (n: { name = n; }) (lib.attrNames cfg.networking.guest.networks);
      limits = lib.filterAttrs (_: v: v != null) { inherit (incus.limits) cpu memory instances; };
    }
    // lib.optionalAttrs (cfg.storage.defaultPool != null) { inherit (cfg.storage) defaultPool; };
    resources = lib.mapAttrsToList (name: vm: {
      id = "vm/${name}";
      type = "vm";
      dependsOn = dependsOn vm;
      lifecycle = { inherit (vm.lifecycle) protected; };
      spec = specOf name vm;
    }) vms;
  };

  # Checks, as [ { ok; msg; } ].
  perVM =
    name: vm:
    let
      at = "strata.workloads.vms.${name}";
      nicNames = lib.attrNames vm.networks;
    in
    [
      {
        ok = vmNameOk name;
        msg = "${at}: the name must be 1-63 lowercase letters, digits and hyphens, starting with a letter.";
      }
      {
        ok = vm.disks ? root;
        msg = "${at}: a disk named \"root\" (the boot disk) is required.";
      }
      {
        ok = lib.length (lib.attrNames vm.disks) <= 16;
        msg = "${at}: at most 16 disks.";
      }
      {
        ok = lib.length nicNames <= 8;
        msg = "${at}: at most 8 network interfaces.";
      }
      {
        ok = toBytes vm.resources.memory >= 128 * units.MiB;
        msg = "${at}.resources.memory: at least 128MiB.";
      }
      {
        ok = vm.imageServer == null || vm.image != null;
        msg = "${at}: imageServer needs image (an alias on the server).";
      }
      {
        ok = vm.imageServer == null || lib.elem vm.imageServer incus.imageServers;
        msg = "${at}.imageServer: \"${toString vm.imageServer}\" is not in strata.compute.incus.imageServers.";
      }
    ]
    ++ lib.mapAttrsToList (d: _: {
      ok =
        diskNameOk d
        && (d == "root" || lib.stringLength name + 1 + lib.stringLength d <= 63)
        && !(lib.elem d nicNames);
      msg = "${at}.disks.${d}: the name must be 1-31 lowercase letters, digits and hyphens, starting with a letter, not a network interface name, and \"${name}-${d}\" at most 63 characters.";
    }) vm.disks
    ++ lib.mapAttrsToList (d: disk: {
      ok = poolOf disk != null && cfg.storage.pools ? ${poolOf disk};
      msg =
        if poolOf disk == null then
          "${at}.disks.${d}.pool: not set and there is no strata.storage.defaultPool."
        else
          "${at}.disks.${d}.pool: \"${poolOf disk}\" is not declared in strata.storage.pools.";
    }) vm.disks
    ++ [
      {
        ok = !(vm.disks ? root) || toBytes vm.disks.root.size >= units.GiB;
        msg = "${at}.disks.root.size: the root disk needs at least 1GiB.";
      }
    ]
    ++ lib.mapAttrsToList (n: _: {
      ok = nicNameOk n;
      msg = "${at}.networks.${n}: the name must be 1-15 lowercase letters and digits, starting with a letter.";
    }) vm.networks
    ++ lib.mapAttrsToList (n: nic: {
      ok = cfg.networking.guest.networks ? ${nic.network};
      msg = "${at}.networks.${n}.network: \"${nic.network}\" is not declared in strata.networking.guest.networks.";
    }) vm.networks;

  sum = f: lib.foldl' (acc: vm: acc + f vm) 0 (lib.attrValues vms);
  totals = [
    {
      ok = cfg.compute.enable;
      msg = "strata.workloads.vms requires strata.compute.enable.";
    }
    {
      ok = incus.limits.instances == null || lib.length vmNames <= incus.limits.instances;
      msg = "strata.workloads.vms: ${toString (lib.length vmNames)} VMs exceed strata.compute.incus.limits.instances (${toString incus.limits.instances}).";
    }
    {
      ok = incus.limits.cpu == null || sum (vm: vm.resources.cpu) <= incus.limits.cpu;
      msg = "strata.workloads.vms: ${
        toString (sum (vm: vm.resources.cpu))
      } CPUs in total exceed strata.compute.incus.limits.cpu (${toString incus.limits.cpu}).";
    }
    {
      ok =
        incus.limits.memory == null || sum (vm: toBytes vm.resources.memory) <= toBytes incus.limits.memory;
      msg = "strata.workloads.vms: the VMs' memory in total exceeds strata.compute.incus.limits.memory (${toString incus.limits.memory}).";
    }
  ];
  checks = if vms == { } then [ ] else totals ++ lib.concatLists (lib.mapAttrsToList perVM vms);

  failedAssertions = map (a: a.message) (lib.filter (a: !a.assertion) config.assertions);
in
{
  options.strata = {
    workloads.vms = mkOption {
      type = types.attrsOf (types.submodule vmModule);
      default = { };
      description = ''
        Virtual machines on this host, by name. Applied by the Strata
        controller from Git (not by nixos-rebuild). See
        docs/administration/workloads.md.
      '';
    };

    ir = mkOption {
      type = types.attrs;
      readOnly = true;
      description = ''
        The intermediate representation (ir.strata.dev/v1alpha1) of this
        host's declared workloads, for the Strata controller:
        `nix eval --json .#nixosConfigurations.NODE.config.strata.ir`.
        Evaluating it fails if any assertion of the configuration fails.
      '';
      default =
        if failedAssertions != [ ] then
          throw "strata.ir: the configuration has failed assertions:\n- ${lib.concatStringsSep "\n- " failedAssertions}"
        else
          ir;
      defaultText = lib.literalMD "the IR of `strata.workloads`";
    };
  };

  config.assertions = map (c: {
    assertion = c.ok;
    message = c.msg;
  }) checks;
}
