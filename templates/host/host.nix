# Values specific to this machine. `strata-install` writes this file from
# its answer file; when configuring by hand, fill in the values marked
# REQUIRED. Until then, evaluation fails with an assertion naming each
# missing option.
#
# This file must stay a plain attribute set (no function arguments):
# flake.nix reads strata.node.id from it to name the configuration.
{
  strata = {
    node.id = "hv-01";

    # REQUIRED: SSH public keys for the admin account. Anyone holding one
    # of these keys is effectively root on the host.
    access.admin.authorizedKeys = [
      # "ssh-ed25519 AAAA... admin@workstation"
    ];

    # REQUIRED: the disk to install on. Formatting destroys all data on
    # it. Use a /dev/disk/by-id/ path (`ls -l /dev/disk/by-id/`).
    # disk.device = "/dev/disk/by-id/nvme-...";

    networking.management = {
      # REQUIRED: the management interface (`ip link`). SSH and the node
      # exporter are reachable only through it.
      # interface = "eno1";
      addressing = "dhcp";
      # For static addressing instead:
      # addressing = "static";
      # addresses = [ "192.0.2.10/24" ];
      # gateway4 = "192.0.2.1";
      # dns = [ "192.0.2.53" ];
    };
  };
}
