# NixOS VM test: install the reference host on an empty disk with the
# "ext4" layout and boot it under UEFI.
#
# disko's test library drives the installation: an installer VM formats
# and mounts the empty disk with the layout, installs the system and its
# boot loader, then a second VM boots from that disk alone (with OVMF
# firmware). The installed system shares the host's Nix store over 9p, so
# copying the closure onto the disk is not covered here; the installer
# test (M1 slice 4) covers it.
{
  pkgs,
  disko,
  mkReferenceHost,
}:
let
  inherit (pkgs.stdenv.hostPlatform) system;
in
disko.lib.testLib.makeDiskoTest {
  inherit pkgs;
  name = "strata-install-ext4";

  # The device is replaced with the test VM's disk.
  disko-config = {
    disko.devices = import ../../../installer/partitioning/ext4.nix { device = "/dev/vda"; };
  };

  inherit (mkReferenceHost system) extendModules;

  extraSystemConfig = {
    # The test VM's NIC name is not predictable; the management network is
    # covered by the nixos-host test.
    systemd.network.wait-online.enable = false;
  };

  extraTestScript = ''
    machine.wait_for_unit("multi-user.target")

    with subtest("filesystems follow the ext4 layout"):
        root = machine.succeed("findmnt -no FSTYPE,SOURCE /").split()
        assert root == ["ext4", "/dev/vda2"], root
        boot = machine.succeed("findmnt -no FSTYPE,SOURCE,OPTIONS /boot").split()
        assert boot[:2] == ["vfat", "/dev/vda1"], boot
        assert "fmask=0077" in boot[2] and "dmask=0077" in boot[2], boot
        # Mounted by partition label, not by device name.
        machine.succeed("grep -E '^/dev/disk/by-partlabel/disk-system-root +/ ' /etc/fstab")
        machine.succeed("test \"$(stat -c %a /boot)\" = 700")

    with subtest("the system booted with systemd-boot"):
        machine.succeed("bootctl is-installed")
        machine.succeed("test -f /boot/EFI/systemd/systemd-bootx64.efi")
        out = machine.succeed("bootctl status --no-pager 2>&1 || true")
        assert "systemd-boot" in out, out

    with subtest("strata host services run on the installed system"):
        for unit in ["strata-agent.service", "sshd.service", "incus.service",
                     "strata-incus-setup.service", "prometheus-node-exporter.service"]:
            machine.wait_for_unit(unit)
        machine.succeed("incus info > /dev/null")
        # The agent reaches Incus with its restricted identity.
        machine.wait_until_succeeds("grep -Eq '\"trusted\": ?true' /var/lib/strata/backend.json", timeout=60)
        machine.wait_until_succeeds("test -s /var/lib/strata/hardware.json")
        machine.succeed("grep -q '\"name\": \"vda\"' /var/lib/strata/hardware.json")
        machine.succeed("grep -Eq '\"id\": ?\"hv-01\"' /etc/strata/node.json")

    with subtest("the installed system boots again"):
        machine.succeed("touch /var/lib/strata/persisted")
        machine.shutdown()
        machine.start()
        machine.wait_for_unit("multi-user.target")
        machine.succeed("test -e /var/lib/strata/persisted")
  '';
}
