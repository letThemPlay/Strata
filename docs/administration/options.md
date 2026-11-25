# NixOS options

Only options listed here are implemented and tested. Everything is
**experimental** until the first release.

The easiest start is the installer ([installation](../installation/README.md)),
which generates the host configuration from the host template. To start
from the template by hand (machine-specific values go in `host.nix`):

```sh
mkdir hv-01 && cd hv-01
nix flake init -t github:letThemPlay/Strata#host
```

Written by hand, a host flake looks like this:

```nix
{
  inputs = {
    strata.url = "github:letThemPlay/Strata";
    nixpkgs.follows = "strata/nixpkgs";
  };

  outputs = { nixpkgs, strata, ... }: {
    nixosConfigurations.hv-01 = nixpkgs.lib.nixosSystem {
      modules = [
        strata.nixosModules.default
        ./hardware-configuration.nix
        {
          strata = {
            enable = true;
            node.id = "hv-01";
            agent.enable = true;
            compute.enable = true;
            access = {
              enable = true;
              admin.authorizedKeys = [ "ssh-ed25519 AAAA... admin@workstation" ];
            };
            networking = {
              enable = true;
              management = {
                interface = "eno1";
                addresses = [ "192.0.2.10/24" ];
                gateway4 = "192.0.2.1";
                dns = [ "192.0.2.53" ];
              };
            };
            monitoring.enable = true;
            disk = {
              enable = true;
              device = "/dev/disk/by-id/nvme-EXAMPLE";
            };
          };
          system.stateVersion = "26.05";
        }
      ];
    };
  };
}
```

Every `strata.<module>.enable` below requires `strata.enable`.

## Base

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.enable` | bool | `false` | Enable Strata on this host. Installs the Strata binaries and writes `/etc/strata/node.json`, installs Git and enables Nix flakes (host configurations are flakes, ADR-0013). |
| `strata.package` | package | flake's `strata` package | Package providing the binaries. Set automatically by `nixosModules.default`. |
| `strata.node.id` | null or RFC 1123 label | `null` | Stable node identifier: lowercase letters, digits and `-`, 1–63 characters, not starting or ending with `-`. Required. Becomes the default `networking.hostName`. |
| `strata.node.role` | `"hypervisor"` | `"hypervisor"` | Node role. No other roles exist yet. |

## Agent

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.agent.enable` | bool | `false` | Run `strata-agent.service` (hardware discovery; on a compute host also the workload backend status and the local API for `strata vm`, on `/run/strata/agent.sock` for members of `strata-operators`). Unprivileged; no network access, except loopback to the Incus API on a compute host. |

## Compute

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.compute.enable` | bool | `false` | Run Incus (`incus-lts`), which manages QEMU/KVM VMs and LXC containers. Enables the nftables firewall, which Incus requires. libvirt is not installed. |
| `strata.compute.incus.project` | string | `"strata"` | Incus project for Strata's workloads: lowercase letters, digits and hyphens, starting with a letter; not `"default"`. |
| `strata.compute.incus.limits.cpu` | null or positive int | `null` | Most CPUs the project's instances may use in total. `null`: no limit. |
| `strata.compute.incus.limits.memory` | null or size | `null` | Most memory in total, e.g. `"64GiB"`. |
| `strata.compute.incus.limits.disk` | null or size | `null` | Most disk space across all pools, e.g. `"2TiB"`. |
| `strata.compute.incus.limits.instances` | null or positive int | `null` | Most instances (VMs and containers). |
| `strata.compute.incus.imageServers` | list of host names | `[]` | Simplestreams image servers (`https://NAME`) VMs may be created from (`imageServer` in a VM spec). Empty: only images imported into the project; the host never downloads images. |

Sizes are a number followed by `B`, `KiB`, `MiB`, `GiB`, `TiB` or `PiB`.

What `strata.compute` sets up:

- **The Incus API listens on `127.0.0.1:8443` only.** It is not opened on any
  interface, and the Incus web UI is not enabled. Administrators use
  `sudo incus` on the host (Unix socket, root).
- **A restricted project** (`strata.compute.incus.project`). Its
  configuration is enforced by `strata-incus-setup.service` on every boot:
  changes made with `incus project set` are reverted. Instances in it must be
  unprivileged; nesting, syscall interception, `raw.*` and other low-level
  settings, disks not backed by a storage pool, and GPU, PCI, USB,
  Infiniband, unix-char, unix-block, unix-hotplug and proxy devices are
  refused. NICs must use Incus-managed networks. Snapshots are allowed;
  backup export is blocked until the backup design (M7). The project may use
  only the pools declared in `strata.storage`, within the limits above.
- **The agent's identity** (with `strata.agent.enable`): a key and
  certificate generated on the host in `/var/lib/strata-incus/` (root only,
  never in Git), trusted by Incus as `strata-agent`, restricted to the
  project. The agent reads them as systemd credentials and reports in
  `/var/lib/strata/backend.json` whether Incus is reachable and trusts it.
  It never uses the Incus Unix socket (ADR-0017).
- **Storage pools** come from [`strata.storage`](#storage), **guest
  networks** from [`strata.networking.guest`](#guest-networks). Without a
  pool, instances cannot be created; the project may use only the declared
  pools and networks.
- Restarting `incus.service` does not stop instances.

KVM modules are not configured: udev loads `kvm-intel` or `kvm-amd` from the
CPU's features.

> **Upgrading a host installed before M2:** after `nix flake update` and
> `nixos-rebuild switch`, libvirtd is gone and Incus starts. M1 hosts have no
> workloads; `/var/lib/libvirt` is left behind and can be deleted.

## Storage

Workload storage pools on dedicated disks, created as Incus storage pools by
`strata-incus-setup.service`. See the [storage guide](storage.md) for how to
choose disks, adopt existing pools and what removing a pool does.

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.storage.pools.<name>.driver` | `"zfs"`, `"lvm"` or `"dir"` | — | Incus storage driver. `<name>`: lowercase letters, digits and hyphens, starting with a letter, at most 31 characters. |
| `strata.storage.pools.<name>.devices` | list of `/dev/disk/by-id/` paths | `[]` | Whole disks: one for `lvm` (a thin pool), one or two for `zfs`. Must be empty unless adopted. |
| `strata.storage.pools.<name>.zfs.layout` | `"single"` or `"mirror"` | `"single"` | ZFS layout; `mirror` needs two devices. For other layouts, create the zpool by hand and adopt it. |
| `strata.storage.pools.<name>.path` | null or absolute path | `null` | For `dir`: a mount point declared in `fileSystems`, on its own disk (not `/`, `/boot`, `/nix`). The pool lives in `<path>/<name>`. |
| `strata.storage.pools.<name>.adopt` | bool | `false` | Use an existing, empty zpool or volume group named `<name>` on the devices instead of creating one. |
| `strata.storage.pools.<name>.limit` | null or size | `null` | Most disk space the project may use on this pool. |
| `strata.storage.defaultPool` | null or pool name | `null` | Pool for the root disk of new instances. |
| `strata.storage.rootSize` | null or size | `null` | Root disk size of new instances (default profile). Required when the default pool or the project has a disk limit. |
| `strata.storage.lvm.autoActivation` | `"declared"` or `"all"` | `"declared"` with `strata.disk`, else `"all"` | Which LVM volume groups activate at boot. `"declared"`: only those of `lvm` pools, so leftover volume groups on other disks stay inactive. |

Checked at evaluation: devices are `by-id` paths, used by one pool only, and
never the system disk (`strata.disk.device`); device counts match the driver
and layout; a `dir` path is a declared mount point; the default pool exists;
pools require `strata.compute`. A `zfs` pool enables ZFS (and fails
evaluation if the kernel's ZFS module is unavailable) and sets
`networking.hostId` from the node ID unless it is set.

## Access

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.access.enable` | bool | `false` | Enable key-only SSH for the admin account. |
| `strata.access.admin.name` | POSIX user name, not `root` | `"strata-admin"` | Administrator account (member of `wheel`; password locked unless a console password is set). |
| `strata.access.admin.authorizedKeys` | list of OpenSSH public keys | `[ ]` | Keys allowed to log in as the admin. At least one is required. |
| `strata.access.admin.consolePasswordFile` | null or absolute path | `null` | File with the crypt(3) hash of a password for **local console** login, for recovery when the network is unreachable. Root-only, outside the Nix store and the configuration repository (the installer uses `/etc/strata/admin-console-password`). Read when the account is created; later use `sudo passwd <admin>` or `sudo passwd -l <admin>`. SSH never accepts passwords. |

sshd is configured with `PasswordAuthentication no`,
`KbdInteractiveAuthentication no`, `PermitRootLogin no`, `X11Forwarding no`
and `AllowUsers <admin>` (override with `mkForce` if more users need SSH).

> **Warning:** the admin has passwordless sudo, because password
> authentication is disabled. Its SSH keys, and its console password if
> one is set, are therefore root-equivalent.
> Protect them accordingly (hardware keys via `sk-ssh-ed25519@openssh.com`
> are supported).

## Networking

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.networking.enable` | bool | `false` | Manage the management interface with systemd-networkd and enable the nftables firewall. |
| `strata.networking.interfaces.<name>.macAddress` | MAC address (lowercase) | — | Pin interface `<name>` to the NIC with this permanent MAC address: it gets that name at every boot, and networkd matches it by MAC. Pin the management interface and guest uplinks; an unpinned one produces a warning. See [stable interface names](networking.md#stable-interface-names). |
| `strata.networking.management.interface` | interface name | `null` | Management interface. Required. |
| `strata.networking.management.addressing` | `"static"` or `"dhcp"` | `"static"` | Addressing mode. |
| `strata.networking.management.addresses` | list of CIDR | `[ ]` | Static addresses. Required with `static`; not allowed with `dhcp`. |
| `strata.networking.management.gateway4` / `gateway6` | null or address | `null` | Default gateways (static only). |
| `strata.networking.management.dns` | list of addresses | `[ ]` | DNS servers (static only). |

Firewall model:

- Nothing is open on any interface by default.
- SSH (with `strata.access`) and the node exporter (with `strata.monitoring`)
  are opened **only on the management interface**.
- Opening ports globally (`networking.firewall.allowedTCPPorts` etc.) is
  rejected by an assertion. Use `networking.firewall.interfaces.<if>`.

### Recovering from a bad network change

Strata does not yet roll back network changes automatically (planned with
the M3 executor). Until then:

- Try changes with `nixos-rebuild test`: they are not made the boot default,
  so a reboot restores the previous configuration.
- Every `nixos-rebuild switch` creates a generation; select an older one in
  the boot menu, or run `nixos-rebuild switch --rollback` from the console.
- Keep out-of-band console access (IPMI/BMC or physical) for hypervisors.

## Guest networks

Incus bridges for workloads, created by `strata-incus-setup.service`. The
host has no address on them. See the [networking guide](networking.md).

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.networking.guest.networks.<name>.uplink` | null or interface name | `null` | Host NIC the network is bridged to; never the management interface. `null`: isolated (guests on this host only). `<name>` becomes the bridge name (at most 15 characters; letters, digits, `-`, `_`). |
| `strata.networking.guest.networks.<name>.vlan` | null or 1–4094 | `null` | VLAN ID, tagged on the uplink. `null`: untagged. |
| `strata.networking.guest.defaultNetwork` | null or network name | `null` | Network for `eth0` of new instances (the project's default profile). |

Checked at evaluation: the management interface is not an uplink; an uplink
is either untagged for one network or carries VLANs (unique IDs), not both;
`<uplink>.<vlan>` fits in 15 characters; a VLAN needs an uplink; the default
network exists; guest networks require `strata.compute` and
`strata.networking`. Uplinks get a networkd unit with no address
(`20-strata-uplink-<if>`), the VLAN interfaces Incus creates likewise
(`21-strata-guest-vlans`), and guest bridges are exempt from the
reverse-path filter.

## Workloads

Declared VMs, applied by the Strata controller from Git (M3; not applied
yet). Their IR is `config.strata.ir`. See [workloads](workloads.md).

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.workloads.vms.<name>.resources.cpu` | 1–256 | required | Virtual CPUs. |
| `strata.workloads.vms.<name>.resources.memory` | size | required | Memory, at least `128MiB`. |
| `strata.workloads.vms.<name>.image` | null or string | `null` | Local image alias, or with `imageServer` an alias on that server. `null`: an empty VM. Only used at creation. |
| `strata.workloads.vms.<name>.imageServer` | null or host name | `null` | Image server; must be in `strata.compute.incus.imageServers`. |
| `strata.workloads.vms.<name>.disks.<disk>.size` | size | required | Disk size; `root` (required, at least `1GiB`) is the boot disk, others are data volumes `<name>-<disk>`. |
| `strata.workloads.vms.<name>.disks.<disk>.pool` | null or pool name | `null` | Pool; `null`: `strata.storage.defaultPool`. |
| `strata.workloads.vms.<name>.networks.<nic>.network` | network name | required | Guest network of interface `<nic>`. |
| `strata.workloads.vms.<name>.autostart` | bool | `false` | Start with the host. |
| `strata.workloads.vms.<name>.cloudInit.sshAuthorizedKeys` | list of strings | `[ ]` | SSH keys for cloud-init. Only used at creation. |
| `strata.workloads.vms.<name>.lifecycle.protected` | bool | `false` | Never deleted or restarted by Strata until cleared and applied. |
| `strata.ir` | read-only | | The IR (`ir.strata.dev/v1alpha1`) of the declared workloads. |

Sizes are a number followed by `B`, `KiB`, `MiB`, `GiB` or `TiB`. The checks
at evaluation are listed in the [workloads guide](workloads.md#what-is-checked).

## GitOps

The Strata controller: plans from a Git repository (M3; experimental; plans
are not applied yet). See [configuration from Git](gitops.md).

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.gitops.enable` | bool | `false` | Run the controller. Requires `strata.agent` and `strata.compute`. |
| `strata.gitops.repository` | `https://...` or `ssh://...` | required | The host's configuration repository (read only). |
| `strata.gitops.branch` | branch name | `"main"` | Branch to track. |
| `strata.gitops.sshKeyFile` | null or path | `null` | Deploy key for `ssh://`; outside the Nix store (`LoadCredential`). |
| `strata.gitops.knownHosts` | list of known_hosts lines | `[ ]` | The server's host key; required for `ssh://`. |
| `strata.gitops.httpsTokenFile` | null or path | `null` | Access token for `https://`; outside the Nix store. |
| `strata.gitops.trustedSigners` | list of `"TYPE KEY [NAME]"` | required | SSH public keys whose signatures make a commit trusted; at least one. |
| `strata.gitops.interval` | 10–86400 | `300` | Seconds between synchronizations. |
| `strata.gitops.evalTimeout` | 10–3600 | `300` | Seconds one evaluation may take. |

## Disk

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.disk.enable` | bool | `false` | Partition the system disk with disko and boot with systemd-boot (UEFI only). |
| `strata.disk.device` | null or path under `/dev/` | `null` | Disk to install on. Required. Prefer `/dev/disk/by-id/...`; it is used only when formatting. |
| `strata.disk.layout` | `"ext4"` | `"ext4"` | Partition layout; see [disk layout](../installation/disk-layout.md). |

With `strata.disk` enabled, do not declare `fileSystems."/"` or
`fileSystems."/boot"` yourself (generate hardware configuration with
`nixos-generate-config --no-filesystems`); conflicting definitions fail
evaluation. systemd-boot keeps at most 10 generations
(`boot.loader.systemd-boot.configurationLimit`, overridable).

> **Warning:** formatting destroys all data on `strata.disk.device`.

## Monitoring

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.monitoring.enable` | bool | `false` | Run the Prometheus node exporter (with the `systemd` collector). Requires `strata.networking.enable`. |
| `strata.monitoring.nodeExporter.port` | port | `9100` | Exporter port, opened on the management interface only. |

## Files and services

| Path / unit | Content |
|---|---|
| `/etc/strata/node.json` | `{"apiVersion": "node.strata.dev/v1alpha1", "id": ..., "role": ...}` |
| `strata-agent.service` | Discovers hardware at start, every minute, and on `systemctl reload`. Changes are logged at info level. |
| `/var/lib/strata/hardware.json` | Hardware report, `apiVersion: hardware.strata.dev/v1alpha1`. Root-readable only. |
| `/etc/systemd/network/10-strata-management.network` | Management interface configuration. |

## Tests

- `checks.<system>.module-eval`: option validation, assertions and generated configuration.
- `checks.<system>.nixos-agent`: VM test of the agent.
- `checks.<system>.nixos-host`: three-node VM test of a full host (SSH, sudo, firewall isolation, node exporter, Incus, KVM).
- `checks.<system>.nixos-incus`: VM test of Incus and the agent's restricted identity (allowed and refused operations, loopback-only API, configuration enforcement).
- `checks.<system>.nixos-storage`: VM test of storage pools (ZFS mirror, LVM-thin, dir; refusals; adoption; removal; LVM autoactivation; reboot).
- `checks.<system>.nixos-nic-names`: VM test of interface names pinned to MAC addresses (opposite NIC order on two hosts).
- `checks.<system>.nixos-vms`: VM test of VM lifecycle through the CLI (needs nested virtualization; boots a NixOS guest).
- `checks.<system>.nixos-networks`: VM test of guest networks (untagged, VLAN and isolated networks; no host address; no VLAN hopping; access refusals; changes; reboot).
- `checks.<system>.reference-host`: builds the reference host (the `host` template plus test values).
- `checks.x86_64-linux.nixos-install-ext4`: formats an empty disk with the `ext4` layout, installs the reference host and boots it under UEFI.
- `checks.x86_64-linux.nixos-installer`: installs a host with the installer ISO, offline, and boots it.
