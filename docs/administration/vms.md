# Virtual machines

`strata vm` manages VMs on the host through the Strata agent (ADR-0020,
ADR-0021). The agent runs them with Incus in the restricted Strata project:
on the declared [storage pools](storage.md) and [guest networks](networking.md)
only.


## Who may use it

The CLI talks to the agent's local API on `/run/strata/agent.sock`. Members
of the `strata-operators` group may use it; the admin account is a member on
hosts with `strata.compute`. Every request is written to the agent's journal
with the caller's uid (`journalctl -u strata-agent | grep '"msg":"audit"'`).

```console
$ strata vm list
NAME    STATUS   OWNER  CPU  MEMORY  ADDRESSES
web-01  Running  cli    2    4GiB    192.0.2.60
```

Remote management (TLS, accounts, roles) comes with the management API (M5).

## Owner, protection and drift

Every VM has an owner. VMs created with `strata vm` (and every VM from
before M3) belong to the CLI (`cli`); VMs declared in the host
configuration and created by the controller belong to Git (`gitops`)
([workloads](workloads.md), [configuration from Git](gitops.md)); a CLI VM
moves to Git with `strata config adopt`. `strata vm list` shows the owner,
marked `protected` and `drift` where that applies; `strata vm show` shows
the details:

```console
$ strata vm show db-01
...
Owner:     gitops
Protected: true
Drift (changed outside Strata):
  config limits.cpu: "8", want "4"
```

- **`strata vm update` and `delete` are refused for VMs managed through
  Git** (`managed by Git: change its declaration in the configuration
  repository instead`). `start`, `stop` and `restart` are allowed.
- **Drift** is a difference between the VM in Incus and what Strata last
  applied, made with `incus` directly. `update` is refused for a drifted
  VM; for a VM managed through Git, the controller's plan restores it
  (with approval). Only the controller may create VMs as Git, restore
  them, or change a VM's owner: the agent recognises it by its system
  user (`strata-controller`).
- **Protected** VMs (`lifecycle.protected` in the declaration) are never
  deleted or restarted by the controller.

The agent also reports what the Strata project can use on the host
(`GET /api/v1/host` on the socket: pools, networks, data volumes); the
controller plans against it.

## The VM spec

A VM is described by a JSON file, schema `vm.strata.dev/v1alpha1`:

```json
{
  "apiVersion": "vm.strata.dev/v1alpha1",
  "name": "web-01",
  "resources": { "cpu": 2, "memory": "4GiB" },
  "image": "debian-13",
  "disks": [
    { "name": "root", "size": "20GiB", "pool": "fast" },
    { "name": "data", "size": "100GiB", "pool": "bulk" }
  ],
  "networks": [ { "name": "eth0", "network": "lan" } ],
  "autostart": true,
  "cloudInit": { "sshAuthorizedKeys": ["ssh-ed25519 AAAA... me@workstation"] }
}
```

| Field | Meaning |
|---|---|
| `name` | 1–63 lowercase letters, digits and hyphens, starting with a letter. |
| `resources.cpu` | Virtual CPUs (1–256). |
| `resources.memory` | Memory, at least `128MiB`. Sizes: a number and `B`, `KiB`, `MiB`, `GiB` or `TiB`. |
| `image` | An image alias: local in the Strata project, or on `imageServer` (see below). Omit it for an empty VM. |
| `imageServer` | Host name of an allowed simplestreams image server, e.g. `images.linuxcontainers.org`. Omit it for a local image. |
| `disks` | The first disk must be `root` (the boot disk, at least `1GiB`). Other disks are data volumes named `<vm>-<disk>` on their pool. At most 16. |
| `networks` | NICs on declared guest networks. The guest names its interfaces itself (for example `enp5s0`). At most 8. |
| `autostart` | Start the VM when the host boots. |
| `cloudInit.sshAuthorizedKeys` | SSH keys for images with cloud-init. |

Unknown fields are rejected. The spec is the whole definition: the VM does
not inherit profile settings, and its spec is stored with it
(`strata vm show NAME --json`).

## Images

By default only images imported into the Strata project can be used, and
the host never downloads images by itself: the project's
`restricted.images.servers` names no server. Import an image as root:

```sh
sudo incus image import METADATA.tar.xz ROOTFS.qcow2 --alias debian-13 --project strata
sudo incus image list --project strata
```

Images for Incus VMs consist of a metadata tarball and a disk image; NixOS
can build one with `nixos/maintainers/scripts/incus/incus-virtual-machine-image.nix`.

To create VMs from a trusted image server instead, allow it in the host
configuration and name it in the spec:

```nix
strata.compute.incus.imageServers = [ "images.linuxcontainers.org" ];
```

```json
"image": "debian/13/cloud",
"imageServer": "images.linuxcontainers.org"
```

Incus downloads the image over HTTPS (simplestreams) and refuses servers
that are not listed (`Image server "..." isn't allowed in this project`).
The `cloud` images there run cloud-init (DHCP, your SSH keys for the
`debian`/`ubuntu` user) but do not include an SSH server: install it once
with `sudo incus exec NAME --project strata -- apt-get install -y
openssh-server`, or use an image that has one.
Only list servers you trust: an image runs as the VM's operating system.

## Lifecycle

```sh
strata vm create -f web-01.json      # creates the VM, stopped
strata vm start web-01
strata vm show web-01
strata vm stop web-01                # graceful: the guest shuts down (60 s)
strata vm stop web-01 --force --timeout 10   # forced if not stopped after 10 s
strata vm restart web-01
strata vm update -f web-01.json      # shows a plan, applies it after confirmation
strata vm delete web-01 --confirm web-01 [--stop]
```

Each command is an operation in the agent. The CLI waits for it and reports
success only after the agent has **verified** the result: after `create`
the VM's configuration and devices are read back from Incus and compared
with the spec; after `start`, `stop` and `restart` its status is checked.
Only one operation per VM runs at a time.

If `create` fails, data volumes it had already created (new and empty) are
removed again. **An existing volume is never reused or removed**: if
`<vm>-<disk>` already exists, `create` is refused.

## Changing a VM

Edit the spec and run `strata vm update -f FILE`. The agent compares it
with the VM as it is in Incus and shows a plan:

```console
$ strata vm update -f web-01.json
Plan for web-01 (running), plan 3f9c1e2a7b6d4c10:
  disks.logs        + 10GiB on bulk  restart; new data volume web-01-logs
  resources.cpu     2 -> 4           restart
  resources.memory  4GiB -> 8GiB     restart
The VM will be stopped gracefully, changed and started again.
Apply? [y/N]
```

What can change:

| Change | Effect |
|---|---|
| CPU, memory | Applied; a running VM is restarted |
| Grow a disk | Applied; a running VM is restarted. The guest must grow its partition and filesystem itself |
| Add a data disk | A new volume `<vm>-<disk>` (refused if it exists); a running VM is restarted |
| Remove a data disk | **Detached only**: the volume is kept; a running VM is restarted |
| Add, remove or move a NIC | Applied; a running VM is restarted |
| `autostart` | Applied without a restart |
| `cloudInit` keys | Stored; cloud-init applies them only on a guest's first boot |
| Shrink a disk, move it to another pool, change the image or the name | **Refused** |

Changes to a running VM are applied with a graceful stop, the change and a
start; the result is verified. If the change fails after the stop, new
volumes are removed and the VM is started again unchanged.

The plan is applied only as confirmed: its ID identifies the VM's current
state and the new spec, and if the VM changes in between (for example it is
started or stopped), the update is refused with `plan again`. `--yes`
applies the plan without asking. A VM that was changed with `incus`
directly is refused (`it was changed outside Strata`): Strata does not
plan from a state it did not create.

## Deleting a VM

```sh
strata vm delete web-01 --confirm web-01          # a stopped VM
strata vm delete web-01 --confirm web-01 --stop   # stops it gracefully first
```

The name must be typed twice; the agent checks it too. The VM and its root
disk are deleted. **Data volumes are never deleted with a VM**: they are
listed afterwards (`Data volumes kept (not deleted): bulk/web-01-data`) and
remain on their pool. A new VM does not take them over: creating a VM
whose data disks would use an existing volume name is refused. Volume
management (attaching, deleting) comes with M4; until then, remove a volume
you no longer need with `sudo incus storage volume delete POOL NAME --project strata`.

## Troubleshooting

| Message | Meaning |
|---|---|
| `permission denied (members of the strata-operators group may use it ...)` | Your account is not in `strata-operators`, or you have not logged in again since being added. |
| `the Strata agent's API is not running on this host` | No `strata.agent.enable` / `strata.compute.enable`, or `strata-agent` is not running (`systemctl status strata-agent strata-agent.socket`). |
| `for a remote image set imageServer (server:alias is not accepted)` | Use a local alias, or set `imageServer` to an allowed server (see Images). |
| `... already exists` | A VM, or a data volume `<vm>-<disk>`, of that name exists. |
| `Storage pool "X" ... not found` / `Network not allowed in project` | The pool or network is not declared in the host configuration. |
| `verification failed: ...` | Incus did not end up as the spec says; the message lists the differences. |
| `plan again` | The VM changed since the plan was shown; run `update` again. |
| `it was changed outside Strata` | The VM's configuration in Incus differs from its spec (changed with `incus` directly; `strata vm show` lists the drift). Revert that change. |
| `managed by Git` | The VM is declared in the host configuration and managed by the controller: change its declaration instead. |
| `change refused: ...` | The change would lose data or cannot be applied (see Changing a VM). |
| `delete`: `it is Running` | Stop the VM first, or add `--stop`. |
| `stop`: `Failed shutting down instance, status is "Running"` | The guest did not shut down in time. A graceful stop is an ACPI power-button press: the guest must handle it, and systemd-based guests ignore it until boot has finished and for 30 s after startup (`HoldoffTimeoutSec`). Wait and retry, raise `--timeout`, or use `--force`. |
