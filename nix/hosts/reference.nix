# The values `templates/host` leaves for the user to fill in, set to
# test-only placeholders. Together with the template this is the
# reference host (`nixosConfigurations.reference`), which CI builds and
# installs in a VM, so the template users start from is always tested.
{
  strata = {
    # Public half of a throwaway test key; no private key exists anywhere.
    access.admin.authorizedKeys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl reference@strata"
    ];
    disk.device = "/dev/disk/by-id/virtio-strata-reference";
    networking.management.interface = "eth0";
  };
}
