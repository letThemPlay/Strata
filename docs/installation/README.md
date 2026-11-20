# Installing Strata

**Status:** experimental (M1 slice 4). The installer is tested end to end
in a VM (`checks.x86_64-linux.nixos-installer`); installation on physical
hardware has not been validated yet. The procedure for that is in
[hardware validation](hardware-validation.md).

## Requirements

- An x86_64 machine that boots in **UEFI** mode (no legacy BIOS), with
  **Secure Boot disabled** (the installer and systemd-boot are not signed)
  and **Intel VT-x or AMD-V** enabled in the firmware settings.
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

## 3. Run the wizard

```sh
sudo strata-install init
```

The wizard asks for:

- the **node ID**, which is also the hostname;
- the **administrator account** (default `strata-admin`);
- the administrator's **SSH public keys**. Password login is disabled, so
  these keys are the only way in. You can:
  - paste a key;
  - read keys from a file, e.g. a `.pub` or `authorized_keys` file on a
    USB stick you mounted;
  - download keys over HTTPS, e.g. `https://github.com/USER.keys`. This is
    the only step that needs network access; the installer ISO uses DHCP.

  Each key is shown with its SHA256 fingerprint. Compare it with
  `ssh-keygen -lf ~/.ssh/id_ed25519.pub` on your workstation before you
  accept it.
- the **system disk**, chosen from the machine's whole disks, listed by
  their stable `/dev/disk/by-id/` names. Disks that are too small or in use
  (such as the installation USB stick) are shown as unavailable.
- the **management interface**, and DHCP or static addressing (addresses
  with prefix length, gateways, DNS servers).

It writes `answer.json` (`--output FILE` to choose another name; it does not
overwrite an existing file without `--force`), shows the plan (below) and
asks whether to install now. Answer `y` to continue with step 5 directly.
Without `sudo`, it stops after the plan.

Ctrl-C or the end of input aborts without writing anything.

## 4. Or write the answer file by hand

The wizard only writes an answer file; you can also write one yourself, as
described in [answer file](answer-file.md), e.g. to install many hosts the
same way. Find the disk and the management interface:

```sh
ls -l /dev/disk/by-id/ | grep -v -- -part   # stable disk names
ip link                                      # interface names
```

Then check it:

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

`init` accepts `apply`'s `--extra-module`, `--work-dir`, `--source-dir` and
`--target` options for the installation it offers. `--extra-module FILE`
adds a NixOS module to the configuration (as `extra-module.nix`), e.g. for
site-specific drivers.

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
