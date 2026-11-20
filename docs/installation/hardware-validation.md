# Physical hardware validation (M1 acceptance)

M1's acceptance criterion is: **install and boot Strata on physical
hardware.** Everything up to installation is tested automatically in VMs
(`nix flake check`); this procedure covers what VMs cannot: real firmware,
disk and network controllers, and CPU virtualization.

M1 is accepted when this procedure passes on at least one Intel and one AMD
x86_64 machine, and the results are recorded below.

Workload management (VMs, containers) is **not** part of M1 and is not
tested here (ADR-0015). Virtualization is validated at the host level only.

## Before you start

- Build the ISO from the commit under test and note the commit:

  ```sh
  git rev-parse HEAD
  nix build .#iso
  ```

- Firmware settings on the machine:
  - **UEFI boot** enabled; legacy/CSM boot disabled.
  - **Secure Boot disabled.** The installer and systemd-boot are not
    signed.
  - **Intel VT-x or AMD-V (SVM)** enabled. VT-d / AMD-Vi (IOMMU) enabled if
    available (recorded, not required).
- A disk of at least 16 GiB that may be erased, and a cable on the intended
  management interface.
- An SSH key pair on your workstation, and a way to provide the public key
  to the installer (file on a second USB stick, HTTPS URL, or typing).

## 1. Installer

Boot the USB stick and follow the [installation guide](README.md) with
`sudo strata-install init`.

| # | Check | Expected |
|---|---|---|
| 1.1 | The installer boots in UEFI mode | `test -d /sys/firmware/efi` succeeds |
| 1.2 | The wizard lists the target disk by a `/dev/disk/by-id/` name | the installation stick is listed as unavailable |
| 1.3 | The wizard lists the management interface | link `up` with the cable connected |
| 1.4 | `plan` output matches the machine | disk size and model, existing partitions |
| 1.5 | The system builds **offline** | no download errors; with the network unplugged too |
| 1.6 | A wrong confirmation aborts | "nothing was changed"; disk untouched |
| 1.7 | The installation completes | "Installation complete" |

## 2. First boot

Remove the stick and reboot.

| # | Check | Expected |
|---|---|---|
| 2.1 | systemd-boot menu appears and boots the system | login prompt with the node ID as hostname |
| 2.2 | SSH with your key from the management network | `ssh strata-admin@ADDRESS` works; `sudo -n true` works |
| 2.3 | Password and root logins are refused | `ssh -o PubkeyAuthentication=no ...` and `ssh root@...` fail |
| 2.4 | Address and route | `networkctl status INTERFACE`: configured, routable; static or DHCP as answered |
| 2.5 | Host services | `systemctl --failed` lists nothing; `systemctl is-active strata-agent sshd libvirtd prometheus-node-exporter` |
| 2.6 | Node exporter from the management network only | `curl http://ADDRESS:9100/metrics` works from the management network |
| 2.7 | Firewall | `sudo nft list ruleset`: ports 22 and 9100 only on the management interface |

## 3. Host-level virtualization

| # | Check | Expected |
|---|---|---|
| 3.1 | KVM module and device | `lsmod \| grep -E '^kvm_(intel\|amd)'`; `test -c /dev/kvm` |
| 3.2 | Hardware report | `sudo cat /var/lib/strata/hardware.json`: `virtualization.extension` is `vmx` or `svm`, `kvmDevice: true`, `firmware.uefi: true`, CPU, memory, disks and NICs match the machine |
| 3.3 | Microcode loaded | `journalctl -k -b \| grep -i microcode` shows the revision |
| 3.4 | IOMMU (if enabled in firmware) | `iommuGroups` in the report is greater than 0 |

## 4. Configuration and recovery

| # | Check | Expected |
|---|---|---|
| 4.1 | `/etc/nixos` is a clean Git repository with one commit | `git -C /etc/nixos status`, `git -C /etc/nixos log` |
| 4.2 | It evaluates offline to the running system | `nix eval --offline --raw /etc/nixos#nixosConfigurations.ID.config.system.build.toplevel.outPath` equals `readlink -f /run/current-system` |
| 4.3 | Reboot persistence | after `sudo reboot`: 2.1, 2.2 and 2.5 pass again |
| 4.4 | Disk layout | `lsblk -f`: ESP (vfat) and root (ext4) only, partition labels `disk-system-ESP` and `disk-system-root` |

## Results

Record one row per machine. Attach `hardware.json` and any failing output
to the pull request that records the result.

| Date | Commit | Machine (vendor, model) | CPU | Disk controller | NIC | Result | Notes |
|---|---|---|---|---|---|---|---|
| | | | Intel | | | | |
| | | | AMD | | | | |
