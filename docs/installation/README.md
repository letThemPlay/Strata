# Installing Strata

**Status:** experimental (M1 slice 4a). The installer is tested end to end
in a VM (`checks.x86_64-linux.nixos-installer`); installation on physical
hardware has not been validated yet (M1 slice 5). The interactive answer
file wizard (`strata-install init`) arrives in slice 4b.

## Requirements

- An x86_64 machine that boots in **UEFI** mode (no legacy BIOS).
- A system disk of at least 16 GiB. **It will be erased completely.**
- A network interface for management. No network access is needed during
  installation: everything is on the ISO.
- An SSH key pair for the administrator.

## 1. Build and write the ISO

```sh
nix build github:letThemPlay/Strata#iso
sudo dd if=result/iso/strata-installer-*.iso of=/dev/sdX bs=4M status=progress conv=fsync
```

## 2. Boot the installer

Boot the machine from the USB stick in UEFI mode. You are logged in on the
console as the `nixos` user, which has passwordless sudo.

Find the disk and the management interface:

```sh
ls -l /dev/disk/by-id/ | grep -v -- -part   # stable disk names
ip link                                      # interface names
```

## 3. Write the answer file

Create `answer.json` as described in [answer file](answer-file.md), for
example with `nano answer.json`. Copying your public key by hand is
error-prone; you can also fetch it from a USB stick or, if the network
works, with `curl`.

## 4. Check the plan

```sh
strata-install plan answer.json
```

This changes nothing. It validates the file, checks the disk and the
interface, and shows the disk's size, model and existing partitions.

## 5. Install

```sh
sudo strata-install apply answer.json
```

`apply`:

1. checks the answer file against the machine again;
2. generates the host configuration (the [host template](../../templates/host)
   with your values in `host.nix` and a detected
   `hardware-configuration.nix`);
3. builds the system offline;
4. asks you to **type the disk's device path** to confirm erasing it
   (`--confirm-wipe DEVICE` does this non-interactively; it must equal
   `disk.device` exactly);
5. erases and partitions the disk and installs the system and boot loader;
6. stores the configuration in `/etc/nixos` as a Git repository, together
   with the sources it needs (Strata, nixpkgs), so it can be evaluated on
   the host without network access;
7. unmounts the new system.

Nothing is written to the disk before step 4. If the configuration does
not evaluate or build, `apply` stops with the disk untouched.

Optional: `--extra-module FILE` adds a NixOS module to the configuration
(as `extra-module.nix`), e.g. for site-specific drivers.

## 6. First boot

Remove the USB stick and reboot. Then, from your workstation:

```sh
ssh strata-admin@<management address>
```

Push the host configuration to your own repository; it is the source of
truth for this host (and what Strata will synchronize from in M3):

```sh
cd /etc/nixos
sudo git remote add origin git@example.com:infra/hv-01.git
sudo git push -u origin main
```

The configuration pins the Strata and nixpkgs revisions of the ISO you
installed with. Update them with `nix flake update` in your repository.
