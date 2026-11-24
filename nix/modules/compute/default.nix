# Virtualization host: Incus manages QEMU/KVM VMs and LXC containers
# (ADR-0015).
#
# - The Incus API listens on loopback only; it is never opened on a network
#   interface.
# - Strata works in one Incus project that is restricted (no privileged or
#   nested containers, no host devices or paths, no low-level settings).
#   strata-incus-setup (strata-host incus-setup, root) enforces the
#   project's configuration, including its limits, on every boot.
# - With the agent enabled, it also creates the agent's TLS identity and
#   has Incus trust it for that project only (ADR-0017). The agent never
#   gets the Incus Unix socket.
# - It creates the storage pools declared in strata.storage (ADR-0018).
#
# Guest networks are not configured yet (a later M2 slice).
#
# KVM kernel modules are not listed here: udev autoloads kvm-intel or
# kvm-amd from the CPU's feature flags, so no vendor is hardcoded.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.strata;
  inherit (cfg.compute.incus) project address;

  # Restrictions of the Strata project. restricted=true already implies
  # most of them; they are listed explicitly so that a change of Incus's
  # defaults cannot loosen them.
  projectConfig = {
    restricted = "true";
    "restricted.containers.privilege" = "unprivileged";
    "restricted.containers.nesting" = "block";
    "restricted.containers.lowlevel" = "block";
    "restricted.containers.interception" = "block";
    "restricted.virtual-machines.lowlevel" = "block";
    "restricted.virtual-machines.nesting" = "block";
    "restricted.devices.disk" = "managed";
    "restricted.devices.disk.paths" = "";
    "restricted.devices.nic" = "managed";
    "restricted.devices.gpu" = "block";
    "restricted.devices.pci" = "block";
    "restricted.devices.usb" = "block";
    "restricted.devices.unix-char" = "block";
    "restricted.devices.unix-block" = "block";
    "restricted.devices.unix-hotplug" = "block";
    "restricted.devices.infiniband" = "block";
    "restricted.devices.proxy" = "block";
    "restricted.idmap.uid" = "";
    "restricted.idmap.gid" = "";
    "restricted.cluster.target" = "block";
    # No remote image server: Incus would download on the project's behalf.
    # Only images imported into the project can be used (ADR-0021).
    "restricted.images.servers" = "strata-no-image-servers.invalid";
    # Snapshots stay inside the pool; backup export belongs to the backup
    # design (M7).
    "restricted.snapshots" = "allow";
    "restricted.backups" = "block";
    # Networks stay in the default project, where a restricted identity can
    # only view them; profiles, images and volumes are per project.
    "features.networks" = "false";
    "features.networks.zones" = "false";
    "features.profiles" = "true";
    "features.images" = "true";
    "features.storage.volumes" = "true";
    "features.storage.buckets" = "true";
  };
  limits = cfg.compute.incus.limits;
  limitConfig = lib.filterAttrs (_: v: v != null) {
    "limits.cpu" = limits.cpu;
    "limits.memory" = limits.memory;
    "limits.disk" = limits.disk;
    "limits.instances" = limits.instances;
  };

  # Desired state for strata-host incus-setup (internal/hostsetup).
  setupState = {
    inherit project;
    projectDescription = "Strata workloads (managed by NixOS; changes are reverted)";
    projectConfig = lib.mapAttrs (_: toString) (projectConfig // limitConfig);
    agent =
      if cfg.agent.enable then
        {
          dir = "/var/lib/strata-incus";
          name = "strata-agent";
          commonName = "strata-agent@${cfg.node.id}";
        }
      else
        null;
    pools = map (p: {
      inherit (p)
        name
        driver
        devices
        adopt
        ;
      layout = p.zfs.layout;
      path = if p.path == null then "" else p.path;
      limit = if p.limit == null then "" else p.limit;
    }) (lib.attrValues cfg.storage.pools);
    defaultPool = if cfg.storage.defaultPool == null then "" else cfg.storage.defaultPool;
    rootSize = if cfg.storage.rootSize == null then "" else cfg.storage.rootSize;
    networks = map (n: {
      inherit (n) name;
      uplink = if n.uplink == null then "" else n.uplink;
      vlan = if n.vlan == null then 0 else n.vlan;
    }) (lib.attrValues cfg.networking.guest.networks);
    defaultNetwork =
      if cfg.networking.guest.defaultNetwork == null then "" else cfg.networking.guest.defaultNetwork;
    managementInterface =
      if cfg.networking.management.interface == null then "" else cfg.networking.management.interface;
  };
  setupConfig = pkgs.writeText "strata-incus-setup.json" (
    builtins.toJSON cfg.compute.incus.setupState
  );
in
{
  options.strata.compute = {
    enable = lib.mkEnableOption "the Strata compute host role (Incus: QEMU/KVM VMs and LXC containers)";

    incus = {
      project = lib.mkOption {
        type = lib.types.strMatching "[a-z][a-z0-9-]{0,62}";
        default = "strata";
        description = ''
          Incus project that holds Strata's workloads. It is restricted, and
          its configuration is enforced on every boot. Must not be
          "default".
        '';
      };
      limits =
        let
          size = lib.types.nullOr (lib.types.strMatching "[0-9]+(B|KiB|MiB|GiB|TiB|PiB)");
        in
        {
          cpu = lib.mkOption {
            type = lib.types.nullOr lib.types.ints.positive;
            default = null;
            description = "Most CPUs the project's instances may use in total (null: no limit).";
          };
          memory = lib.mkOption {
            type = size;
            default = null;
            example = "64GiB";
            description = "Most memory the project's instances may use in total (null: no limit).";
          };
          disk = lib.mkOption {
            type = size;
            default = null;
            example = "2TiB";
            description = "Most disk space the project may use across all pools (null: no limit).";
          };
          instances = lib.mkOption {
            type = lib.types.nullOr lib.types.ints.positive;
            default = null;
            description = "Most instances (VMs and containers) in the project (null: no limit).";
          };
        };
      setupState = lib.mkOption {
        type = lib.types.attrs;
        internal = true;
        readOnly = true;
        default = setupState;
        description = "Desired state passed to strata-host incus-setup (for tests).";
      };
      address = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1:8443";
        readOnly = true;
        description = "Loopback address of the Incus API (not configurable).";
      };
    };
  };

  config = lib.mkIf cfg.compute.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.compute.enable requires strata.enable.";
      }
      {
        assertion = project != "default";
        message = "strata.compute.incus.project must not be \"default\" (it cannot be restricted).";
      }
    ];

    # Incus on NixOS requires the nftables firewall.
    networking.nftables.enable = true;

    virtualisation.incus = {
      enable = true;
      package = pkgs.incus-lts;
      ui.enable = false;
      # incusd starts at boot so that instances autostart.
      socketActivation = false;
      # Restarting incusd does not stop instances.
      softDaemonRestart = true;
      preseed.config."core.https_address" = address;
    };

    systemd.services.strata-incus-setup = {
      description = "Strata Incus project, agent identity and storage pools";
      wantedBy = [ "multi-user.target" ];
      requires = [ "incus.service" ];
      after = [
        "incus.service"
        "incus-preseed.service"
      ];
      before = lib.optional cfg.agent.enable "strata-agent.service";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${lib.getExe' cfg.package "strata-host"} incus-setup --config ${setupConfig}";
        StateDirectory = "strata-incus";
        StateDirectoryMode = "0700";
        UMask = "0077";
      };
      # dir pools live on their own mounts.
      unitConfig.RequiresMountsFor = lib.concatMap (p: lib.optional (p.path != null) p.path) (
        lib.attrValues cfg.storage.pools
      );
      # Re-run when the declared state changes.
      restartTriggers = [ setupConfig ];
      # pvs (adopting LVM pools).
      path = [ config.services.lvm.package ];
    };
  };
}
