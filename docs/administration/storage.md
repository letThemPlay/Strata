# Storage pools

Workloads (VMs and containers) store their disks in **storage pools on
dedicated disks**. The system disk holds the operating system only
(ADR-0012). Pools are declared in the host configuration and created by
`strata-incus-setup.service` as Incus storage pools (ADR-0018). Options:
[options.md](options.md#storage).

**Strata never erases a disk and never deletes or modifies a pool.** It
creates a pool only on disks that are verifiably empty, or adopts a pool you
prepared. Removing a pool from the configuration leaves it and its data in
place.

## Drivers

| Driver | Devices | Notes |
|---|---|---|
| `zfs` | one disk, or two with `zfs.layout = "mirror"` | Snapshots and copies are cheap; checksums detect corruption; a mirror survives one disk failure. Needs ZFS for the running kernel. |
| `lvm` | one disk | LVM thin pool. No checksums; no redundancy. |
| `dir` | a mount point (`path`) | Plain directories on a filesystem you mount. Simple, but snapshots and copies are full copies. |

For other ZFS layouts (raidz, several mirrors), create the zpool by hand and
adopt it (below).

## Declaring pools

1. Find the disks' stable names. Use the model/serial names (`nvme-…`,
   `ata-…`, `scsi-…`), one per disk, not the `wwn-…`/`eui.…` duplicates:

   ```sh
   ls -l /dev/disk/by-id/ | grep -v -- -part
   lsblk -o NAME,SIZE,MODEL,SERIAL,FSTYPE,MOUNTPOINTS
   ```

2. Add the pools to `/etc/nixos/host.nix`:

   ```nix
   strata.storage = {
     defaultPool = "fast";
     pools.fast = {
       driver = "zfs";
       zfs.layout = "mirror";
       devices = [
         "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_2TB_S7DNNJ0X000001"
         "/dev/disk/by-id/nvme-Samsung_SSD_990_PRO_2TB_S7DNNJ0X000002"
       ];
     };
     pools.bulk = {
       driver = "lvm";
       devices = [ "/dev/disk/by-id/ata-WDC_WD80EFZZ-68BTXN0_XXXXXXXX" ];
       limit = "4TiB";
     };
   };
   ```

   A `dir` pool also needs its filesystem declared:

   ```nix
   fileSystems."/srv/scratch" = {
     device = "/dev/disk/by-id/ata-...-part1";
     fsType = "ext4";
   };
   strata.storage.pools.scratch = { driver = "dir"; path = "/srv/scratch"; };
   ```

3. Apply: `sudo nixos-rebuild switch --flake /etc/nixos#$(hostname)`. A ZFS
   pool needs the ZFS kernel module, which is downloaded from the binary
   cache the first time (the host needs network access). **The first ZFS
   pool on a host also needs a reboot**: the running system cannot load the
   newly added module, and `strata-incus-setup` fails with `Module zfs not
   found` until the host has rebooted into the new configuration.

4. Check:

   ```sh
   systemctl status strata-incus-setup      # failed = a pool was refused
   journalctl -u strata-incus-setup -b      # the reason, per pool
   sudo incus storage list
   ```

A refused pool does not stop the others. The service fails, so
`systemctl --failed` shows it, and the journal names the disk and what was
found on it.

## When a disk is refused

| Message | Meaning and what to do |
|---|---|
| `not empty (/dev/sdb1: ext4 "olddata", ...)` | The disk holds data. If it is no longer needed, erase it deliberately: `sudo wipefs -a /dev/disk/by-id/...` (and its partitions first), then restart `strata-incus-setup`. |
| `holds the running system` | The disk carries `/`, `/boot` or `/nix`. Use another disk. |
| `in use: /dev/sdb1 is mounted on ...` / `is part of /dev/dm-0` | Unmount it, or deactivate the volume group / RAID (`vgchange -an NAME`), first. |
| `not a whole disk` | Pools use whole disks; give the disk's own `by-id` name, not a `-partN` name. |
| `existing data cannot be checked` | No udev database; Strata refuses rather than guess. |
| `exists with driver X, declared Y` / `with source ...` | A pool of this name already exists differently. Strata does not change it: fix the declaration, or remove the pool yourself (`sudo incus storage delete NAME`, which destroys its data). |
| `nothing is mounted on PATH` | A `dir` pool's filesystem is not mounted. |

## Adopting a pool you prepared

To use a ZFS layout Strata does not model, or a volume group you created:

```sh
sudo zpool create -m none tank raidz2 /dev/disk/by-id/ata-A /dev/disk/by-id/ata-B /dev/disk/by-id/ata-C /dev/disk/by-id/ata-D
sudo zpool export tank
```

```nix
strata.storage.pools.tank = {
  driver = "zfs";
  adopt = true;
  devices = [ "/dev/disk/by-id/ata-A" "/dev/disk/by-id/ata-B" "/dev/disk/by-id/ata-C" "/dev/disk/by-id/ata-D" ];
};
```

The pool (or volume group, for `lvm`) must be named like the Strata pool, and
every listed disk must carry only its signatures. It must be **empty**:
Incus refuses a zpool or volume group with other content.

Recovering a pool that already holds Incus instances (for example after
reinstalling the host) is not adoption: use `sudo incus admin recover`.

## Removing a pool

Deleting a pool from the configuration does **not** delete the Incus pool,
its volumes or its instances. What happens next depends on whether
instances of the Strata project still use it:

- **Still used** (instances, or the default profile): the pool stays open
  to the project, and `strata-incus-setup` logs `storage pool is not
  declared but still used by the project`. Incus does not allow closing a
  pool under existing instances, and Strata never deletes them. Move them
  (`incus move NAME --storage OTHER`) or delete them.
- **Not used:** the pool is closed to the project
  (`limits.disk.pool.NAME=0`): no new instances or volumes can be created on
  it. This happens on the next run of `strata-incus-setup` (next boot or
  rebuild, or `sudo systemctl restart strata-incus-setup`).

To really remove it, move or delete its instances, then run
`sudo incus storage delete NAME`. That destroys its data.

## Limits

`strata.storage.pools.<name>.limit` caps the project's use of one pool;
`strata.compute.incus.limits.disk` caps all pools together. Creating an
instance or volume beyond a limit fails with `Reached maximum aggregate
value`. Thin pools (ZFS, LVM-thin) can be overcommitted: limits count the
sizes given to instances, not the space they use.

Because limits count sizes, **every instance on a limited pool needs a root
disk size** (and with `limits.disk`, every instance). Set
`strata.storage.rootSize` for instances created from the default profile;
evaluation fails without it when the default pool or the project is
limited.

## LVM volume groups on other disks

On hosts installed by Strata (`strata.disk`), only the volume groups of
declared `lvm` pools are activated at boot
(`strata.storage.lvm.autoActivation = "declared"`), and the initrd
activates none. Leftover volume groups on other disks, for example from a
previous Proxmox installation, stay inactive and untouched, and no
`lvm-activate-*` unit fails. To use one, activate it by hand
(`sudo vgchange -ay NAME`) or declare it as an adopted pool.

Set `"all"` on a host whose own filesystems use LVM: `"declared"` would
leave them inactive and the host would not boot.

## Snapshots and backups

Instances in the Strata project may take snapshots (they stay in the
pool). Backup export (`incus export`) is blocked until Strata's backup
design (M7). **A snapshot is not a backup**: it lives on the same disks.
