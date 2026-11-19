# Sample Strata infrastructure configuration for a single standalone hypervisor.
#
# STATUS: non-normative example (milestone M0).
#
# This file illustrates the *intended* shape of a Strata configuration as
# described in CLAUDE.md sections 6 and 7. It is a plain Nix attribute set,
# NOT a NixOS module, and none of these options are implemented yet. The
# authoritative schema and its intermediate representation (IR) are defined
# in milestone M3. Until then, `nix flake check` only verifies that this file
# evaluates and has the expected overall structure.
{
  node = {
    id = "hv-01";
    role = "hypervisor";
  };

  storage.pools.fast-pool = {
    type = "zfs";
    pool = "tank/strata";
  };

  networks.vmbr0 = {
    type = "bridge";
    interfaces = [ "eno1" ];
    vlanAware = true;
  };

  workloads.web-01 = {
    type = "vm";

    resources = {
      cpu = 4;
      memory = "8GiB";
    };

    disks = [
      {
        name = "system";
        size = "80GiB";
        storage = "fast-pool";
      }
    ];

    networking = [
      {
        bridge = "vmbr0";
        vlan = 20;
      }
    ];

    lifecycle = {
      autostart = true;
      # Removing this workload from configuration must never delete its disks
      # implicitly (CLAUDE.md section 14).
      onRemove = "retain";
    };
  };
}
