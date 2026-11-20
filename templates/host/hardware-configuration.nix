# Generic hardware configuration.
#
# Replace this file with the output of
#   nixos-generate-config --no-filesystems --show-hardware-config
# run on the target machine. `--no-filesystems` matters: filesystems are
# declared by strata.disk, not here.
#
# Until then, these defaults cover common x86_64 server and virtual
# hardware.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  # Microcode for both vendors on x86; only the matching one is loaded.
  microcode = config.hardware.enableRedistributableFirmware && pkgs.stdenv.hostPlatform.isx86;
in
{
  boot.initrd.availableKernelModules = [
    "ahci"
    "nvme"
    "sd_mod"
    "usb_storage"
    "usbhid"
    "xhci_pci"
    "virtio_blk"
    "virtio_pci"
    "virtio_scsi"
  ];

  hardware = {
    enableRedistributableFirmware = lib.mkDefault true;
    cpu.intel.updateMicrocode = lib.mkDefault microcode;
    cpu.amd.updateMicrocode = lib.mkDefault microcode;
  };

  nixpkgs.hostPlatform = lib.mkDefault "x86_64-linux";
}
