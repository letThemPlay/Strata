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
      # exporter are reachable only through it. Pin it below.
      # interface = "eno1";
      addressing = "dhcp";
      # For static addressing instead:
      # addressing = "static";
      # addresses = [ "192.0.2.10/24" ];
      # gateway4 = "192.0.2.1";
      # dns = [ "192.0.2.53" ];
    };

    # Pin the management interface (and guest uplinks) to its permanent MAC
    # address (`ethtool -P eno1`), so adding hardware cannot rename it.
    # networking.interfaces.eno1.macAddress = "84:47:09:a3:5d:dc";

    # Workload storage pools on dedicated disks (never the system disk).
    # Disks must be empty; Strata never erases them. See
    # docs/administration/storage.md.
    # Guest networks: bridged to a NIC other than the management
    # interface (untagged, or a VLAN), or isolated. The host has no address
    # on them. See docs/administration/networking.md.
    # networking.guest = {
    #   defaultNetwork = "lan";
    #   networks.lan.uplink = "enp3s0";
    # };

    # storage = {
    #   defaultPool = "fast";
    #   pools.fast = {
    #     driver = "zfs";
    #     devices = [ "/dev/disk/by-id/nvme-..." ];
    #   };
    # };

    # Configuration from Git (M3, experimental: plans only for now). See
    # docs/administration/gitops.md.
    # gitops = {
    #   enable = true;
    #   repository = "ssh://git@git.example.org/infra/hv-01.git";
    #   sshKeyFile = "/etc/strata/gitops-key";
    #   knownHosts = [ "git.example.org ssh-ed25519 AAAA..." ];
    #   trustedSigners = [ "ssh-ed25519 AAAA... alice" ];
    # };

    # Virtual machines (M3, experimental: declared and validated here, not
    # applied yet; use `strata vm` meanwhile). See
    # docs/administration/workloads.md.
    # workloads.vms.web-01 = {
    #   resources = { cpu = 2; memory = "2GiB"; };
    #   image = "debian-13";
    #   disks.root.size = "20GiB";
    #   networks.eth0.network = "lan";
    #   autostart = true;
    # };
  };
}
