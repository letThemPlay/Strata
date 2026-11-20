# host.nix of the reference host: the template's host.nix with test-only
# values for everything it leaves to the user. Together with the rest of
# the template this is `nixosConfigurations.reference`, which CI builds,
# installs in a VM and ships in the installer ISO, so the template users
# start from is always tested.
{
  strata = {
    node.id = "hv-01";

    # Public half of a throwaway test key; no private key exists anywhere.
    access.admin.authorizedKeys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl reference@strata"
    ];

    disk.device = "/dev/disk/by-id/virtio-strata-reference";

    networking.management = {
      interface = "eth0";
      addressing = "dhcp";
    };
  };
}
