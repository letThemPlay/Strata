# Virtualization host: libvirt with KVM-accelerated QEMU.
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
in
{
  options.strata.compute = {
    enable = lib.mkEnableOption "the Strata compute (libvirt/QEMU/KVM) host role";
  };

  config = lib.mkIf cfg.compute.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.compute.enable requires strata.enable.";
      }
    ];

    virtualisation.libvirtd = {
      enable = true;
      qemu = {
        # Emulator for the host architecture only.
        package = pkgs.qemu_kvm;
        # Guests run as the unprivileged qemu-libvirtd user.
        runAsRoot = false;
        swtpm.enable = true;
      };
      # Shut guests down cleanly on host shutdown instead of saving their
      # memory to disk.
      onShutdown = "shutdown";
    };
  };
}
