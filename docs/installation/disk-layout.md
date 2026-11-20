# System disk layout

**Status:** experimental. `strata-install` applies this layout
([installation](README.md)). It is tested by installing and booting hosts
in VMs (`checks.x86_64-linux.nixos-install-ext4`, `nixos-installer`); it
has not been validated on physical hardware yet (M1 slice 5).

## The `ext4` layout

Selected with `strata.disk.layout = "ext4"` (the default and only layout).
Defined in [`installer/partitioning/ext4.nix`](../../installer/partitioning/ext4.nix).

| Partition | GPT label | Size | Filesystem | Mount point |
|---|---|---|---|---|
| EFI system partition | `disk-system-ESP` | 1 GiB | vfat (`umask=0077`) | `/boot` |
| Root | `disk-system-root` | rest of the disk | ext4 | `/` |

- **UEFI only.** The machine must boot in UEFI mode; legacy BIOS (CSM) is not
  supported. The boot loader is systemd-boot, which keeps at most 10
  generations on the ESP.
- **Single disk.** No mirroring (RAID1) of the system disk yet.
- **No swap partition.**
- **VM data.** Until storage pools arrive (M4), libvirt stores VM disks under
  `/var/lib/libvirt` on the root filesystem.

## Choosing the device

Set `strata.disk.device` to a stable path from `/dev/disk/by-id/`:

```sh
ls -l /dev/disk/by-id/ | grep -v -- -part
```

`/dev/sdX` and `/dev/nvmeXnY` names can differ between boots and between
the installer and the installed system. The installed system mounts its
filesystems by GPT partition label, so the device path is only used when
formatting.

> **Warning:** formatting destroys every partition and all data on the
> selected disk, including VM disks stored under `/var/lib/libvirt` by a
> previous installation. Strata never formats a disk implicitly;
> `strata-install` requires you to type the device path first.
