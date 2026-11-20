# NixOS VM test: install a Strata host with the installer ISO, offline,
# and boot it.
#
# The installer VM boots the ISO image (a test variant with the test
# driver's instrumentation and fast compression) from a CD-ROM with UEFI
# firmware, and has an empty disk and a NIC with no outside network. It
# runs strata-install; then a second VM boots from that disk alone: no
# shared Nix store, so the installed system must be complete.
{
  pkgs,
  mkInstaller,
  mkReferenceHost,
}:
let
  inherit (pkgs) lib;
  inherit (pkgs.stdenv.hostPlatform) system;

  keys = import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs;
  qemu-common = import "${pkgs.path}/nixos/lib/qemu-common.nix" { inherit (pkgs) lib stdenv; };

  installedTestModule = ./installed-test-module.nix;

  testInstaller = mkInstaller system [
    (
      { modulesPath, ... }:
      {
        imports = [ (modulesPath + "/testing/test-instrumentation.nix") ];
        isoImage.squashfsCompression = "lz4";
        environment.etc."strata-test/extra-module.nix".source = installedTestModule;
        # The installed system additionally contains the test
        # instrumentation; ship its closure like the rest of a host.
        system.extraDependencies = [
          ((mkReferenceHost system).extendModules { modules = [ installedTestModule ]; })
          .config.system.build.toplevel
        ];
      }
    )
  ];
  iso = testInstaller.config.system.build.isoImage;

  answers = {
    apiVersion = "install.strata.dev/v1alpha1";
    node.id = "hv-07";
    admin.authorizedKeys = [ keys.snakeOilEd25519PublicKey ];
    disk.device = "/dev/disk/by-id/virtio-strata-target";
    network.addressing = "dhcp";
  };
in
pkgs.testers.runNixOSTest {
  name = "strata-installer";
  nodes = { };

  # Two VM boots and an offline system build.
  meta.timeout = 3600;

  testScript = ''
    import glob, json, os, shutil, subprocess, tempfile

    tmp = tempfile.mkdtemp()
    disk = os.path.join(tmp, "target.qcow2")
    subprocess.run(["${pkgs.qemu_test}/bin/qemu-img", "create", "-f", "qcow2", disk, "20G"], check=True)
    # Writable UEFI variables, shared by both VMs: the installer registers
    # the boot entry there, as on real hardware.
    efivars = os.path.join(tmp, "OVMF_VARS.fd")
    shutil.copy("${pkgs.OVMF.variables}", efivars)
    os.chmod(efivars, 0o644)
    iso = glob.glob("${iso}/iso/*.iso")[0]

    def qemu(memory, *extra):
        return " ".join([
            "${qemu-common.qemuBinary pkgs.qemu_test}", "-m", str(memory), "-smp", "2",
            "-drive", "if=pflash,format=raw,unit=0,readonly=on,file=${pkgs.OVMF.firmware}",
            "-drive", f"if=pflash,format=raw,unit=1,file={efivars}",
            "-drive", f"file={disk},if=none,id=target,format=qcow2",
            "-device", "virtio-blk-pci,drive=target,serial=strata-target",
            "-netdev", "user,id=n0,restrict=on", "-device", "virtio-net-pci,netdev=n0",
            *extra,
        ])

    def run(machine, cmd, timeout=900):
        """Run cmd, print its output, and fail with it if cmd fails."""
        status, out = machine.execute(f"set -o pipefail; {cmd} 2>&1", timeout=timeout)
        print(out)
        assert status == 0, f"`{cmd}` failed with exit code {status}"
        return out

    installer = create_machine(qemu(4096, "-cdrom", iso), name="installer")
    # Register for cleanup, so the driver stops the VM if a test fails.
    driver.machines_qemu.append(installer)
    installer.start()
    installer.wait_for_unit("multi-user.target")

    nic = installer.succeed("ls /sys/class/net | grep -vx lo").strip()
    answers = json.loads('${builtins.toJSON answers}')
    answers["network"]["interface"] = nic
    installer.succeed(f"cat > /tmp/answer.json <<'EOF'\n{json.dumps(answers, indent=2)}\nEOF")

    with subtest("plan describes the installation"):
        out = run(installer, "strata-install plan /tmp/answer.json")
        assert "virtio-strata-target -> /dev/vda, 20.0 GiB" in out, out
        assert "Partitions: none" in out, out

    with subtest("plan rejects an invalid answer file"):
        installer.succeed("sed 's/hv-07/HV_07/' /tmp/answer.json > /tmp/bad.json")
        out = installer.fail("strata-install plan /tmp/bad.json 2>&1")
        assert "node.id" in out, out

    with subtest("a mismatched confirmation builds the system but leaves the disk untouched"):
        status, out = installer.execute("strata-install apply --confirm-wipe /dev/vda /tmp/answer.json 2>&1", timeout=1800)
        print(out)
        if "the disk has not been touched" in out:
            # The offline build failed: show the generated configuration.
            print(installer.execute("for f in /tmp/strata-host-*/*.nix; do echo \"=== $f\"; cat \"$f\"; done")[1])
        assert status != 0, "apply succeeded with a mismatched confirmation"
        assert "nothing was changed" in out, out
        installer.succeed("test -z \"$(wipefs --no-act --noheadings /dev/vda)\"")

    with subtest("apply installs the host"):
        run(
            installer,
            "strata-install apply"
            " --confirm-wipe /dev/disk/by-id/virtio-strata-target"
            " --extra-module /etc/strata-test/extra-module.nix"
            " /tmp/answer.json",
            timeout=1800,
        )
        installer.succeed("! findmnt /mnt")

    installer.shutdown()

    installed = create_machine(qemu(3072), name="installed")
    driver.machines_qemu.append(installed)
    installed.start()
    installed.wait_for_unit("multi-user.target")

    with subtest("the installed host boots from its own disk"):
        installed.succeed("test \"$(hostname)\" = hv-07")
        root = installed.succeed("findmnt -no FSTYPE,SOURCE /").split()
        assert root == ["ext4", "/dev/vda2"], root
        # The Nix store is on the installed disk, not shared from the host.
        store = installed.succeed("findmnt -no SOURCE --target /nix/store").strip()
        # NixOS bind-mounts the store read-only: "/dev/vda2[/nix/store]".
        assert store.startswith("/dev/vda2"), store
        print(installed.execute("findmnt -t 9p,virtiofs")[1])
        installed.succeed("bootctl is-installed")

    with subtest("Strata host services run"):
        for unit in ["strata-agent.service", "sshd.service", "libvirtd.service",
                     "prometheus-node-exporter.service"]:
            installed.wait_for_unit(unit)
        installed.succeed("grep -q '${keys.snakeOilEd25519PublicKey}' /etc/ssh/authorized_keys.d/strata-admin")

    with subtest("/etc/nixos is a Git repository that reproduces the running system"):
        installed.succeed("test \"$(git -C /etc/nixos rev-list --count HEAD)\" = 1")
        installed.succeed("test -z \"$(git -C /etc/nixos status --porcelain)\"")
        built = run(
            installed,
            "nix eval --offline --raw /etc/nixos#nixosConfigurations.hv-07.config.system.build.toplevel.outPath",
            timeout=600,
        ).strip().splitlines()[-1]
        running = installed.succeed("readlink -f /run/current-system").strip()
        assert built == running, f"/etc/nixos evaluates to {built}, running {running}"
  '';
}
