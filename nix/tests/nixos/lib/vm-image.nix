# A NixOS virtual machine image for Incus (nixpkgs' image module), for VM
# tests that boot a guest. Includes the Incus guest agent, so tests can run
# commands in the guest. Built locally (not in the binary cache).
{ pkgs }:
let
  system = import (pkgs.path + "/nixos/lib/eval-config.nix") {
    system = pkgs.stdenv.hostPlatform.system;
    modules = [
      (pkgs.path + "/nixos/maintainers/scripts/incus/incus-virtual-machine-image.nix")
      {
        documentation.enable = false;
        # Addresses are set by the test; no DHCP wait at boot.
        networking.useDHCP = false;
        system.stateVersion = "26.05";
      }
    ];
  };
in
pkgs.runCommand "strata-incus-vm-image" { } ''
  mkdir $out
  ln -s ${system.config.system.build.repartImage}/nixos.qcow2 $out/root.qcow2
  ln -s $(echo ${system.config.system.build.metadata}/*/*.tar.xz) $out/metadata.tar.xz
''
