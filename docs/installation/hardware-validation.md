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
- **Boot media:** write the ISO to a USB stick with `dd` (see the
  [installation guide](README.md#boot-media)). Remote virtual media of an
  IP KVM may not work (see there).
- **Other disks on a reused machine:** erase old LVM, ZFS or RAID data on
  disks you are not installing on, or expect failed units such as
  `lvm-activate-<vg>.service` (see the
  [installation guide](README.md#leftover-data-on-other-disks)).
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
| 2.5 | Host services | `systemctl --failed` lists nothing; `systemctl is-active strata-agent sshd incus strata-incus-setup prometheus-node-exporter`; `sudo incus info` answers; `sudo cat /var/lib/strata/backend.json` shows `reachable` and `trusted` true. (Results recorded before M2 used libvirtd, checked with `virsh`.) |
| 2.6 | Node exporter from the management network only | `curl http://ADDRESS:9100/metrics` works from the management network |
| 2.7 | Firewall | `sudo nft list ruleset`: ports 22 and 9100 only on the management interface |

## 3. Host-level virtualization

| # | Check | Expected |
|---|---|---|
| 3.1 | KVM module and device | `lsmod \| grep -E '^kvm_(intel\|amd)'`; `test -c /dev/kvm` |
| 3.2 | Hardware report | `sudo cat /var/lib/strata/hardware.json`: `virtualization.extension` is `vmx` or `svm`, `kvmDevice: true`, `firmware.uefi: true`, CPU, memory, disks and NICs match the machine |
| 3.3 | Microcode loaded | `sudo dmesg \| grep -i microcode` shows the revision (without `sudo` the log may be unreadable and the output empty) |
| 3.4 | IOMMU (if enabled in firmware) | `iommuGroups` in the report is greater than 0. Known limitation: Strata does not enable the IOMMU on the kernel command line yet, so on Intel it is 0 even with VT-d enabled (AMD-Vi is enabled by the kernel by default) |

## 4. Configuration and recovery

| # | Check | Expected |
|---|---|---|
| 4.1 | `/etc/nixos` is a clean Git repository with one commit | `sudo git -C /etc/nixos status`, `sudo git -C /etc/nixos log` (it is owned by root; without `sudo`, Git refuses with "dubious ownership") |
| 4.2 | It evaluates offline to the running system | `sudo nix eval --offline --raw /etc/nixos#nixosConfigurations.ID.config.system.build.toplevel.outPath` equals `readlink -f /run/current-system` |
| 4.5 | Console login (if a console password was set) | log in as the admin on the local console |
| 4.3 | Reboot persistence | after `sudo reboot`: 2.1, 2.2 and 2.5 pass again |
| 4.4 | Disk layout | `lsblk -f`: ESP (vfat) and root (ext4) only, partition labels `disk-system-ESP` and `disk-system-root` |

## M2: VM management

M2's acceptance criterion is: **manage VMs through the CLI.** VM tests
cover it under nested virtualization; this procedure checks it on one
physical machine (Intel or AMD) that passed the M1 checks, updated to the
commit under test (`cd /etc/nixos && sudo nix flake update && sudo
nixos-rebuild switch --flake .#$(hostname)`). It needs a spare disk for a
storage pool and a NIC other than the management interface (or an isolated
network if there is none).

**After adding hardware, check the interface and disk names first**
(`ip -br link`, `ls -l /dev/disk/by-id/`): PCI-path NIC names (`enp3s0`)
and kernel disk names (`nvme0n1`) can change when a device is added. Disks
are always named by `by-id`; pin NIC names to their MAC addresses
([stable interface names](../administration/networking.md#stable-interface-names))
before adding hardware.

1. **Declare a pool and a guest network** in `/etc/nixos/host.nix`, then
   rebuild:

   ```nix
   strata.storage = { defaultPool = "vms"; pools.vms = { driver = "zfs"; devices = [ "/dev/disk/by-id/..." ]; }; };
   strata.networking.guest = { defaultNetwork = "lan"; networks.lan.uplink = "enp3s0"; };
   ```

   Expected: `systemctl --failed` lists nothing; `sudo incus storage list`
   and `sudo incus network list` show them. **The first ZFS pool on a host
   needs a reboot after the rebuild** (the running system cannot load the
   newly added ZFS module); `strata-incus-setup` fails with `Module zfs not
   found` until then.
2. **Provide an image:** import one (for example built with nixpkgs'
   `incus-virtual-machine-image.nix`): `sudo incus image import META.tar.xz
   DISK.qcow2 --alias test --project strata`; or allow a server
   (`strata.compute.incus.imageServers = [ "images.linuxcontainers.org" ];`)
   and use `"image": "debian/13/cloud", "imageServer":
   "images.linuxcontainers.org"` in the spec.
3. **Create and start** as the admin: write a spec (see
   [VMs](../administration/vms.md)) with a root disk, a 1 GiB data disk and
   a NIC on `lan`; `strata vm create -f vm.json`, `strata vm start NAME`.
   Expected: `strata vm list` shows it `Running`.
4. **Reach it:** from another machine on the guest network, ping or SSH to
   the guest (its address is in `strata vm list` once the guest agent or
   DHCP reports it, or use `sudo incus console NAME --project strata`).
   Images from `images.linuxcontainers.org` do not include an SSH server:
   install it with `sudo incus exec NAME --project strata -- apt-get install
   -y openssh-server` first.
5. **Update:** raise CPU and memory in the spec; `strata vm update -f
   vm.json` shows a plan with `restart`; confirm. Expected: the guest
   reports the new CPU count and memory.
6. **Reboot the host** with `autostart: true`. Expected: the VM runs again,
   `systemctl --failed` lists nothing.
7. **Stop and delete:** `strata vm stop NAME`, `strata vm delete NAME
   --confirm NAME`. Expected: the VM is gone; the data volume is listed as
   kept and shown by `sudo incus storage volume list vms --project strata`.

Record the result in the M2 table below.

## Results

Record one row per machine. Attach `hardware.json` and any failing output
to the pull request that records the result.

| Date | Commit | Machine (vendor, model) | CPU | Disk controller | NIC | Result | Notes |
|---|---|---|---|---|---|---|---|
| 2026-11-23 | ISO from `main` after PR #6 (exact commit not recorded) | Lenovo ThinkServer TS140 | Intel Xeon E3-1225 v3 (VT-x) | SATA (Samsung SSD 860 as system disk) | Intel dual-port (`ens3f0`/`ens3f1`, e1000e) | **Pass** | See below |
| 2026-11-23 | ISO from `main` after PR #8 | GMKtec M6 Ultra (mini PC) | AMD Ryzen 5 7640HS (AMD-V) | NVMe (GMK 512GB) | `eno1`/`enp3s0` (plus Wi-Fi, unused) | **Pass** | See below |

### Lenovo ThinkServer TS140 (Intel), 2026-11-23

Installed with `strata-install init` from a USB stick, DHCP on `ens3f1`.
Every check after installation passed (2.2–2.7, 3.1–3.3, 4.1–4.4),
including reboot persistence. Microcode was updated early
(`0x17` → `0x28`), and device firmware (Radeon) loaded.

- **1.2:** the old Proxmox disk was unavailable ("in use") because the
  live system had activated its LVM volume group: correct.
- **1.3:** the first install selected the wrong port: no port had a link,
  and the wizard pre-selected the first. The host was unreachable and,
  without a console password, could only be reinstalled. Fixed after this
  run: no default without a link, re-scan, optional console password.
- **1.5:** the first install ran with the network cable unplugged.
- **1.6:** not run on hardware (covered by the `nixos-installer` VM test).
- **2.5:** `lvm-activate-pve.service` failed until the leftover Proxmox LVM
  data on another disk (`sda`) was erased; not a Strata fault.
- **3.4:** VT-d is enabled in the firmware (DMAR table present), but the
  kernel does not enable the IOMMU: 0 groups (known limitation).
- **Boot media:** JetKVM virtual media from its own storage did not work:
  firmware booted from it, but Linux reset the emulated drive repeatedly
  and never saw a disk. A USB stick written with `dd` worked.

### GMKtec M6 Ultra (AMD), 2026-11-23

Installed with `strata-install init` from a USB stick, DHCP on `eno1`,
console password set. Every check after installation passed (2.2–2.7,
3.1–3.4, 4.1–4.5), including reboot persistence. This was the first
hardware run of the fixes from the TS140 run.

- **1.2:** the installation stick was listed as unavailable; the system
  disk showed the existing-data warning and needed confirmation.
- **1.6:** not run on hardware (covered by the `nixos-installer` VM test).
- **3.3:** microcode was updated early (`0x0a704107` → `0x0a704108`).
  `journalctl -k` as the admin user printed nothing; `sudo dmesg` showed
  it (the check now uses `sudo dmesg`).
- **3.4:** 30 IOMMU groups without any kernel option: the kernel enables
  AMD-Vi by default, so the IOMMU limitation applies to Intel only.
- **4.5:** console login with the console password works.
- Boot-time log entries were stamped three days early until time
  synchronisation corrected the clock (hardware clock behind); not a
  Strata fault.

### M2 results

| Date | Commit | Machine | Pool (driver, disk) | Guest network | Image | Result | Notes |
|---|---|---|---|---|---|---|---|
| 2026-11-24 | `main` after PR #15 (`58f26f6`) | GMKtec M6 Ultra (AMD Ryzen 5 7640HS) | `zfs`, Crucial P3 1 TB NVMe (added for the test) | `lan`, untagged on `enp4s0` (Realtek r8169), DHCP from the LAN | `debian/13/cloud` from `images.linuxcontainers.org` (allow-listed) | **Pass** | See below |

### GMKtec M6 Ultra (AMD), M2, 2026-11-24

The host from the M1 run, updated to `main` after PR #15, with a second
NVMe drive added. All seven steps passed, run as `strata-admin` through
`strata vm`:

- **1:** the new disk held a Windows installation; it was wiped by hand
  (`wipefs -a` on its partitions and the disk), as Strata never erases
  disks. The pool `vms` (ZFS) was then created on it. The first attempt
  failed with `Module zfs not found in /run/booted-system`: the first ZFS
  pool on a host needs a reboot after the rebuild (now documented). After
  the reboot: pool online, nothing failed.
- **1 (network):** adding the drive renamed the second NIC from `enp3s0` to
  `enp4s0` (and the Wi-Fi from `wlp2s0` to `wlp3s0`); `eno1` (management)
  kept its name. `strata-incus-setup` refused the declared uplink
  (`uplink enp3s0 does not exist`) instead of guessing; with `enp4s0` the
  `lan` bridge was created with no host address. See the follow-ups below.
- **2-3:** `test-01` (2 CPUs, 2 GiB, a 10 GiB root disk and a 1 GiB data
  disk on `vms`, NIC on `lan`, cloud-init SSH key) was created from the
  allow-listed server and started; it got addresses from the LAN's DHCP
  and IPv6.
- **4:** SSH from the workstation was refused at first: the Debian cloud
  image has no SSH server. After installing `openssh-server` in the guest
  (`incus exec`), SSH with the key from the spec worked; the guest showed
  the 10 GiB root disk and the 1 GiB data disk.
- **5:** `strata vm update` showed the plan (CPU 2 -> 4, memory 2 -> 4 GiB,
  both with restart) and applied it; the guest reported 4 CPUs and about
  4 GiB.
- **6:** after a host reboot `test-01` was running again (autostart), with
  its addresses; no failed units.
- **7:** a delete without `--confirm` was refused; with it, the VM and its
  root disk were deleted and `vms/test-01-data` was kept and listed.

Follow-ups (not blocking M2):

- **Stable NIC names:** a hardware change renamed a PCI-path NIC. Had it
  been the management interface, the host would have lost its management
  network. **Done after this run:** interface names are pinned to MAC
  addresses (`strata.networking.interfaces`), written by `strata-install`
  for the management interface.
- **cloud-init packages:** the VM spec passes only SSH keys to cloud-init;
  images without an SSH server need another step. Proposed: an optional
  `cloudInit.packages` list in the spec (additive).
