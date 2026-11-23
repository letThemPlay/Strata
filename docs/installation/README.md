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
sudo blkid /dev/sdX    # LABEL="nixos-strata-..." TYPE="iso9660"
```

See [boot media](#boot-media) for what does not work.

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
- an optional **console password** for the administrator (typed twice, not
  shown). It works only for logging in on the local console, physically or
  through a remote KVM, to recover when the host cannot be reached over the
  network; SSH never accepts passwords. Only its hash is kept, in
  `/etc/strata/admin-console-password` on the host (root-only, not in the
  configuration repository). Without one, a host with a broken network
  configuration can only be repaired by booting the installer. With
  passwordless sudo, the console password is root-equivalent, like the
  SSH keys.
- the **system disk**, chosen from the machine's whole disks, listed by
  their stable `/dev/disk/by-id/` names. Disks that are too small or in use
  (such as the installation USB stick) are shown as unavailable. Disks
  with existing data (filesystems, ZFS, LVM or RAID members) are marked
  `HAS EXISTING DATA`; choosing one lists what it holds and asks before it
  is erased.
- the **management interface**, and DHCP or static addressing (addresses
  with prefix length, gateways, DNS servers). It is the **only interface
  configured** on the installed host; the others stay down (no link lights).
  Connect the management cable before you choose: an interface with a link
  is the default, and if none has one, the wizard asks you to connect the
  cable and press Enter to re-scan.

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

## Troubleshooting

### Boot media

- **Write the ISO raw** (`dd`, or Rufus in "DD image mode", or
  balenaEtcher). Tools that unpack the ISO onto a FAT filesystem (Rufus "ISO
  image mode") or boot it through their own loader (Ventoy) lose its
  filesystem label, and boot stops with "Timed out waiting for device
  /dev/disk/by-label/nixos-strata-...".
- **Remote virtual media of IP KVMs may not work.** With JetKVM, the
  firmware booted the ISO from JetKVM's own storage, but Linux reset the
  emulated drive repeatedly and never saw a disk, with the same timeout.
  The cause is not known yet; use a USB stick in the machine.

### The host is not reachable after installation

Check that the cable is in the management interface chosen during
installation: only that interface is configured, the others stay down. On
the console, `networkctl list` shows which one is configured.

- **Move the cable** to the configured interface; or
- **log in on the console** with the console password, if you set one, and
  correct `strata.networking.management` in `/etc/nixos/host.nix`. The host
  is offline, so bring the cabled interface up by hand first, e.g.
  `sudo ip link set eno2 up && sudo ip addr add 192.0.2.10/24 dev eno2 &&
  sudo ip route add default via 192.0.2.1`, then
  `sudo nixos-rebuild switch --flake /etc/nixos`, which downloads what it
  needs to rebuild, and commit the change;
- without a console password, boot the installer and install again.

### Leftover data on other disks

On a reused machine, old data on disks you did not install on is still
there. LVM volume groups on them are activated automatically at boot, and
fail if they are incomplete or use features the host lacks (Proxmox's
`pve` group fails with `lvm-activate-pve.service`). ZFS pools are not
imported (Strata does not enable ZFS). Erase disks you do not need, after
checking with `lsblk -f` which disk is which:

```sh
sudo vgchange -an VG                       # if an LVM group is active
sudo wipefs --all /dev/sdX2 /dev/sdX1 /dev/sdX   # partitions first, then the disk
```
