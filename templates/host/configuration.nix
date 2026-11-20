# Strata host configuration.
#
# Three values must be filled in before this configuration evaluates; until
# then, evaluation fails with an assertion naming the missing option:
#
#   - strata.access.admin.authorizedKeys    your SSH public key(s)
#   - strata.disk.device                    the disk to install on
#   - strata.networking.management.interface the management NIC
#
# See docs/administration/options.md in the Strata repository for every
# option.
{
  strata = {
    enable = true;

    node.id = "hv-01";

    agent.enable = true;

    # libvirt with KVM-accelerated, unprivileged QEMU.
    compute.enable = true;

    # Key-only SSH for the admin account, which has passwordless sudo.
    # Anyone holding one of these keys is effectively root on the host.
    access = {
      enable = true;
      admin.authorizedKeys = [
        # "ssh-ed25519 AAAA... admin@workstation"
      ];
    };

    # Formatting this disk destroys all data on it. Use a /dev/disk/by-id/
    # path (`ls -l /dev/disk/by-id/`).
    disk = {
      enable = true;
      # device = "/dev/disk/by-id/nvme-...";
      layout = "ext4";
    };

    # SSH and the node exporter are reachable only through this interface.
    networking = {
      enable = true;
      management = {
        # interface = "eno1"; # see `ip link`
        addressing = "dhcp";
        # For static addressing instead:
        # addressing = "static";
        # addresses = [ "192.0.2.10/24" ];
        # gateway4 = "192.0.2.1";
        # dns = [ "192.0.2.53" ];
      };
    };

    monitoring.enable = true;
  };

  # The NixOS release this host was first installed with. Do not change it
  # on upgrades; see `man configuration.nix` (system.stateVersion).
  system.stateVersion = "26.05";
}
