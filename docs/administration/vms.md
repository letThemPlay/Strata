# Virtual machines

`strata vm` manages VMs on the host through the Strata agent (ADR-0020,
ADR-0021). The agent runs them with Incus in the restricted Strata project:
on the declared [storage pools](storage.md) and [guest networks](networking.md)
only.

> **Status (M2 slice 4a):** create, list, show, start, stop and restart.
> Changing a VM (`update`) and deleting it (`delete`) come in the next slice;
> until then use `sudo incus ... --project strata` for those.

## Who may use it

The CLI talks to the agent's local API on `/run/strata/agent.sock`. Members
of the `strata-operators` group may use it; the admin account is a member on
hosts with `strata.compute`. Every request is written to the agent's journal
with the caller's uid (`journalctl -u strata-agent | grep '"msg":"audit"'`).

```console
$ strata vm list
NAME    STATUS   CPU  MEMORY  ADDRESSES
web-01  Running  2    4GiB    192.0.2.60
```

Remote management (TLS, accounts, roles) comes with the management API (M5).

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
| `image` | A **local** image alias in the Strata project (see below). Omit it for an empty VM. |
| `disks` | The first disk must be `root` (the boot disk, at least `1GiB`). Other disks are data volumes named `<vm>-<disk>` on their pool. At most 16. |
| `networks` | NICs on declared guest networks. The guest names its interfaces itself (for example `enp5s0`). At most 8. |
| `autostart` | Start the VM when the host boots. |
| `cloudInit.sshAuthorizedKeys` | SSH keys for images with cloud-init. |

Unknown fields are rejected. The spec is the whole definition: the VM does
not inherit profile settings, and its spec is stored with it
(`strata vm show NAME --json`).

## Images

Only images imported into the Strata project can be used. The host never
downloads images by itself: the project's `restricted.images.servers` names
no server. Import an image as root:

```sh
sudo incus image import METADATA.tar.xz ROOTFS.qcow2 --alias debian-13 --project strata
sudo incus image list --project strata
```

Images for Incus VMs consist of a metadata tarball and a disk image; NixOS
can build one with `nixos/maintainers/scripts/incus/incus-virtual-machine-image.nix`.
Allowing a trusted remote image server comes in the next slice.

## Lifecycle

```sh
strata vm create -f web-01.json      # creates the VM, stopped
strata vm start web-01
strata vm show web-01
strata vm stop web-01                # graceful: the guest shuts down (60 s)
strata vm stop web-01 --force --timeout 10   # forced if not stopped after 10 s
strata vm restart web-01
```

Each command is an operation in the agent. The CLI waits for it and reports
success only after the agent has **verified** the result: after `create`
the VM's configuration and devices are read back from Incus and compared
with the spec; after `start`, `stop` and `restart` its status is checked.
Only one operation per VM runs at a time.

If `create` fails, data volumes it had already created (new and empty) are
removed again. **An existing volume is never reused or removed**: if
`<vm>-<disk>` already exists, `create` is refused.

## Troubleshooting

| Message | Meaning |
|---|---|
| `permission denied (members of the strata-operators group may use it ...)` | Your account is not in `strata-operators`, or you have not logged in again since being added. |
| `the Strata agent's API is not running on this host` | No `strata.agent.enable` / `strata.compute.enable`, or `strata-agent` is not running (`systemctl status strata-agent strata-agent.socket`). |
| `remote images (server:alias) are not allowed` | Import the image into the project and use its alias. |
| `... already exists` | A VM, or a data volume `<vm>-<disk>`, of that name exists. |
| `Storage pool "X" ... not found` / `Network not allowed in project` | The pool or network is not declared in the host configuration. |
| `verification failed: ...` | Incus did not end up as the spec says; the message lists the differences. |
| `stop`: `Failed shutting down instance, status is "Running"` | The guest did not shut down in time. A graceful stop is an ACPI power-button press: the guest must handle it, and systemd-based guests ignore it until boot has finished and for 30 s after startup (`HoldoffTimeoutSec`). Wait and retry, raise `--timeout`, or use `--force`. |
