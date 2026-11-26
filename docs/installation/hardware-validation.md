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

## M3: configuration from Git

M3's acceptance criterion is: **Git changes generate safe and repeatable
infrastructure changes.** The `nixos-gitops` VM test covers the controller
with a Git server inside the test; this procedure checks it with a real
Git host over SSH, commits signed on a workstation, evaluation on real
hardware and a real reboot. It needs a machine that passed the M2 checks,
with its pool and guest network (the examples use `vms` and `lan`), and a
**private repository** on a Git host reachable over SSH (the examples use
GitHub; `OWNER/hv-01` is a placeholder).

Run the commands marked *workstation* in your clone of the repository,
and the others on the host as the admin user.

0. **Prepare.**
   - Update the host to the commit under test, and commit the result in
     `/etc/nixos` (the controller evaluates the repository, so the
     repository must hold what the host runs):

     ```sh
     cd /etc/nixos && sudo nix flake update && sudo nixos-rebuild switch --flake .#$(hostname)
     sudo git add -A && sudo git -c user.name=admin -c user.email=admin@localhost commit -m "Update to the commit under test"
     ```

   - Remove what M2 left: `sudo incus storage volume delete vms
     test-01-data --project strata` (and any test VM).
   - Copy the repository to the workstation and push it to the new
     private repository (`/etc/nixos` holds no secrets: the console
     password hash and keys are outside it):

     ```sh
     sudo git -C /etc/nixos bundle create /tmp/hv-01.bundle --all && sudo chmod 644 /tmp/hv-01.bundle
     # workstation:
     scp strata-admin@HOST:/tmp/hv-01.bundle . && git clone hv-01.bundle hv-01 && cd hv-01
     git remote set-url origin git@github.com:OWNER/hv-01.git && git push -u origin main
     ```

   - A signing key on the workstation, used for every commit from now on:

     ```sh
     ssh-keygen -t ed25519 -C "alice (strata signing)" -f ~/.ssh/strata-signing
     git config gpg.format ssh && git config user.signingkey ~/.ssh/strata-signing.pub && git config commit.gpgsign true
     ```

1. **Enable gitops.** Create the deploy key on the host
   ([configuration from Git](../administration/gitops.md#setting-it-up))
   and add its public key to the repository as a **read-only** deploy key.
   Get GitHub's host key with `ssh-keyscan -t ed25519 github.com` and
   compare `ssh-keygen -lf` of it with the fingerprint GitHub publishes
   ("GitHub's SSH key fingerprints"). *Workstation:* add to `host.nix`,
   commit and push:

   ```nix
   strata.compute.incus.imageServers = [ "images.linuxcontainers.org" ];  # if not set in M2
   strata.gitops = {
     enable = true;
     repository = "ssh://git@github.com/OWNER/hv-01.git";
     sshKeyFile = "/etc/strata/gitops-key";
     knownHosts = [ "github.com ssh-ed25519 AAAA..." ];
     trustedSigners = [ "ssh-ed25519 AAAA... alice" ];   # ~/.ssh/strata-signing.pub
   };
   ```

   On the host, pull it ([host changes](../administration/gitops.md#host-changes))
   and rebuild. Expected: `systemctl --failed` lists nothing; `strata
   config status` shows the commit evaluated and signed by `alice`, the
   plan `nothing to do`. Record how long the evaluation took and its
   memory peak: `journalctl -u strata-controller -o cat | grep
   '"evaluated"'` (`seconds`) and `journalctl -u
   'strata-controller-eval@*' | grep 'memory peak'`.
2. **Declare a VM.** *Workstation:* add, commit and push:

   ```nix
   strata.workloads.vms.git-01 = {
     resources = { cpu = 2; memory = "2GiB"; };
     image = "debian/13/cloud";
     imageServer = "images.linuxcontainers.org";
     disks.root.size = "10GiB";
     disks.data.size = "1GiB";
     networks.eth0.network = "lan";
     autostart = true;
     cloudInit.sshAuthorizedKeys = [ "ssh-ed25519 AAAA... alice" ];
   };
   ```

   `strata config sync`, `strata config plan` (one `create`), `strata
   config apply --plan ID`. Expected: the deployment ends `SUCCEEDED`;
   `strata vm list` shows `git-01` running, owner `gitops`; `strata config
   history` shows it approved by your uid. SSH to the guest after
   installing `openssh-server` (`sudo incus exec git-01 --project strata
   -- apt-get install -y openssh-server`).
3. **Repeatable.** `strata config sync` again. Expected: `nothing to do`,
   no new record in `strata config history`.
4. **Edit.** *Workstation:* `cpu = 4`, commit, push; sync. Expected: the
   plan shows an update with a restart; after `apply`, the guest reports
   4 CPUs (`nproc`).
5. **The CLI boundary.** `strata vm update -f ...` and `strata vm delete
   git-01 --confirm git-01` are refused (`managed by Git`); `strata vm
   stop git-01` works and the plan stays `nothing to do` (the power state
   is not declared); `strata vm start git-01` again.
6. **Drift.** `sudo incus config set git-01 limits.memory=3GiB --project
   strata`; sync. Expected: `status` lists `git-01` under drift, the plan
   restores it; after `apply`, `limits.memory` is `2GiB` again.
7. **Trust.** *Workstation:* push a commit made with `git commit
   --no-gpg-sign`; sync. Expected: `refused: ... not signed`, not
   evaluated (no new `"evaluated"` line in the journal). Then rewind the
   branch past the last applied commit: `git reset --hard HEAD~2 && git
   push --force`; sync. Expected: `does not descend from the last applied
   commit`. Recover: `git reset --hard <last applied>` (from `status`) and
   `git push --force`. Expected: evaluated, `nothing to do`.
8. **Roll back.** `strata config pin <commit of step 2>`. Expected:
   `status` shows it pinned; the plan sets 2 CPUs (restart); `apply`.
   `strata config unpin`. Expected: the branch head is planned again
   (4 CPUs); `apply`.
9. **Adopt.** Create a VM with the CLI, and declare the same VM in Git:

   ```sh
   echo '{"apiVersion":"vm.strata.dev/v1alpha1","name":"cli-01","resources":{"cpu":1,"memory":"1GiB"},"disks":[{"name":"root","size":"2GiB","pool":"vms"}]}' > cli-01.json
   strata vm create -f cli-01.json
   ```

   ```nix
   strata.workloads.vms.cli-01 = {
     resources = { cpu = 1; memory = "1GiB"; };
     disks.root = { size = "2GiB"; pool = "vms"; };
   };
   ```

   Push, sync. Expected: the plan says `cli-01` cannot be applied (owned
   by the CLI; adopt it). `strata config adopt
   cli-01`. Expected: owner `gitops`, the plan `nothing to do`.
10. **Reboot** the host. Expected: `git-01` runs again (autostart);
    `strata config status` keeps the last applied commit; `strata config
    history` is kept; `systemctl --failed` lists nothing.
11. **Protection and deletion.** Set `lifecycle.protected = true` on
    `cli-01`, push, apply. Remove `cli-01` from the configuration, push,
    sync. Expected: the plan says it cannot be applied (protected). Put it
    back with `protected = false`, apply. `cli-01` has no OS and ignores a
    shutdown request, so stop it first: `strata vm stop cli-01 --force`
    (**before** the next sync: a change on the node after the plan is
    made makes the approval stale, and the deployment fails at `PLANNING`
    with nothing applied). Remove it again, sync. Expected: `strata
    config apply --plan ID` without `--allow-delete` is refused; with it,
    `cli-01` is deleted. Remove `git-01` the same way. Expected:
    the result lists the data volume `git-01-data` as kept, and `sudo
    incus storage volume list vms --project strata` shows it.
12. **Host change.** *Workstation:* `nix flake update`, commit, push;
    sync. Expected: reported as a host change (`changes flake.lock`), not
    evaluated. Pull on the host and rebuild; sync. Expected: evaluated,
    `nothing to do`.
13. **Optional: automatic application.** Set `strata.gitops.autoApply =
    "safe"`, push, pull and rebuild on the host. Declare another VM,
    push. Expected: within `interval` (or after `strata config sync`) it
    is created, approved by `auto`. Remove it: the plan waits for
    approval.

Record the result in the M3 table below. Afterwards, delete the kept
volume with `sudo incus storage volume delete vms git-01-data --project
strata`.

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

### M3 results

| Date | Commit | Machine | Git host | Evaluation (seconds, memory peak) | Result | Notes |
|---|---|---|---|---|---|---|
| 2026-11-26 | `main` after PR #23 (`3008795`); from step 12, `m3/hardware-validation` (`dc64ef8`) | GMKtec M6 Ultra (AMD Ryzen 5 7640HS), node `hv-02` | Private GitHub repository over SSH (read-only deploy key), commits signed with an SSH key on a workstation | 1.6 s, 471 MB (first, cold); 1.5 s, 467 MB (next) | **Pass** (steps 0–12; 13 not run) | See below |

### GMKtec M6 Ultra (AMD), M3, 2026-11-26

The host from the M2 run (pool `vms`, ZFS; guest network `lan`), with its
configuration pushed to a private GitHub repository and the controller
enabled. Every required step passed, run as `strata-admin` with commits
pushed from a workstation:

- **1:** the deploy key worked with `ssh -i`, but `sudo git pull` in
  `/etc/nixos` was refused (`Permission denied (publickey)`): Git as root
  used root's own keys. Setting `core.sshCommand` in `/etc/nixos` fixed
  it (now documented). The first commit was refused with `cannot run gpg`:
  the workstation signs with GPG for other repositories, and the commit
  had an OpenPGP signature. After switching the clone to SSH signing the
  commit was verified and evaluated. **Fixed after this:** commits with an
  OpenPGP or X.509 signature are refused with the reason, without running
  gpg. The signer showed as `signer1` (no name in `trustedSigners`).
- **2-3:** `git-01` (Debian 13 from the allow-listed image server, 10 GiB
  root, 1 GiB data disk, `lan`, autostart) was created through an approved
  plan in 5 seconds; the record holds repository, commit, signer,
  `flake.lock` blob, Strata version, node, approver and every state with
  its time. SSH to the guest worked (after installing `openssh-server`
  with `incus exec`, as in M2). A second sync: nothing to do, no new
  record.
- **4-6:** a CPU change was planned with a restart and applied (the guest
  reported 4 CPUs); `strata vm update` and `delete` were refused for the Git
  VM, `stop`/`start` allowed, and the plan stayed converged while it was
  stopped; a memory change made with `incus` was found by the periodic sync
  and restored after approval.
- **7:** an unsigned head was refused and not evaluated; a force-push
  behind the last applied commit was refused; moving the branch back made
  the controller follow it again.
- **8:** pinning the step 2 commit planned the full rollback (CPU 4 -> 2 and
  deleting the then adopted `cli-01`), refused without `--allow-delete`,
  applied with it, and recorded as pinned (the guest reported 2 CPUs);
  unpinning planned and applied the branch head again (4 CPUs).
- **9:** a VM created with `strata vm` and declared in Git was reported as
  owned by the CLI; `strata config adopt` moved it to Git (a first attempt
  before the commit was pushed was refused and recorded as failed).
  **Fixed after this:** the status summarized a plan with only an
  unreconcilable item as "nothing to do"; and the commit was not recorded
  as applied, because nothing needed applying, so the forward-only check
  stayed at an older commit. A commit the node already matches is now
  recorded as applied (approved by `auto`); confirmed on the host after
  step 12.
- **10:** after a host reboot `git-01` ran again; status, last applied
  commit and history were kept; no failed units.
- **11:** a protected VM removed from the configuration was kept and
  reported; after clearing protection, deletion was refused without
  `--allow-delete`. The first approval failed at `PLANNING`, with nothing
  applied, because the VM had been force-stopped after the plan was made
  (the stale-plan check); the new plan was applied. Deleting `git-01` kept
  `vms/git-01-data`, listed in the plan, the result and Incus.
- **12:** a `flake.lock` change (to the branch with the fixes) was reported
  as a host change and not evaluated; after `git pull` and `nixos-rebuild`
  in `/etc/nixos` it was evaluated offline. **Fixed after this:** the
  message included Nix's whole trace; it now shows Nix's final error, and
  the journal keeps the trace.
- **13** (automatic application) was not run on hardware; `nixos-gitops`
  covers it.

Follow-ups (not blocking M3):

- Approvals go stale when anything on the node changes after the plan,
  including power state. Correct, but an operator stopping a VM to delete
  it must do so before the plan is made (documented in step 11).
- The pull into `/etc/nixos` for host changes does not verify signatures;
  only the controller does (documented).

### M3 slice 5a: host evaluation on hv-02, 2026-11-26

The host evaluation command the evaluation service runs (`nix derivation
show` of `nixosConfigurations.hv-02.config.system.build.toplevel`, offline,
pure, no import-from-derivation, no substitution, evaluation cache off,
through nix-daemon) was run by hand on hv-02 with the service's identity
and sandbox (`strata-controller`, untrusted; `ProtectSystem=strict`,
`PrivateNetwork`, `AF_UNIX` only, `MemoryMax=4G`), for the commit the
controller targets. hv-02 still ran the build from the M3 run, so this
checks the evaluation step, not `strata host evaluate` itself.

| Run | Runtime | Memory peak | Output path vs `/run/current-system` |
|---|---|---|---|
| 1 | 2.93 s | 653.8 MB | equal (`2q6cgqlr…-nixos-system-hv-02-26.11.20261121.2786a2a`) |
| 2 | 2.91 s | 654.1 MB | equal |

- Instantiating as an untrusted user through nix-daemon works on the
  host's store; the VM test needs derivations registered as root first
  (ADR-0028, implementation note).
- The output path equals the running system: the host runs exactly its
  repository configuration, so configuration drift is `in-sync`.
- Both runs came after an earlier probe had written the derivations; a
  first evaluation of a new commit writes more and may take longer. The
  4 GiB cap leaves ample room.
