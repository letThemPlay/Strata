# Declared workloads

**Status: experimental, M3 slice 1.** VMs can be declared in the host
configuration and Strata produces their intermediate representation (IR).
Nothing applies them yet: the controller that fetches the configuration
from Git, plans and applies it arrives in M3 slices 2 and 3. Until then,
manage VMs with `strata vm` ([VMs](vms.md)).

## Declaring VMs

VMs are declared in `host.nix`, next to the pools and networks they use:

```nix
strata.workloads.vms.web-01 = {
  resources = {
    cpu = 2;
    memory = "2GiB";
  };
  image = "debian/13/cloud";
  imageServer = "images.linuxcontainers.org"; # in strata.compute.incus.imageServers
  disks.root.size = "20GiB";                   # pool: strata.storage.defaultPool
  disks.data = {
    size = "100GiB";
    pool = "bulk";
  };
  networks.eth0.network = "lan";
  autostart = true;
  cloudInit.sshAuthorizedKeys = [ "ssh-ed25519 AAAA... admin@workstation" ];
  lifecycle.protected = true;
};
```

The fields are those of the VM spec (`vm.strata.dev/v1alpha1`, see
[VMs](vms.md)); the attribute name is the VM name. Disks and network
interfaces are attribute sets keyed by name: `root` is the boot disk and is
always first; the other disks and the interfaces follow in name order
(`eth0`, `eth1`, ...). [examples/standalone/host.nix](../../examples/standalone/host.nix)
is a complete example.

`lifecycle.protected` will make Strata refuse to delete or restart the VM,
even after it is removed from the configuration, until the flag is set to
false and applied (M3 slice 3).

## How declarations will be applied

The planner (M3 slice 2, [ADR-0024](../../ARCHITECTURE.md#adr-0024-planning-and-reconciliation))
compares the IR with the host and makes one plan for the node: VMs to
**create** (then started), **update**, **restore** (changed with `incus`
directly), whose **protection** changes, and VMs managed through Git that
are no longer declared, to **delete** (root disk only; data volumes are
kept). What cannot be applied is listed with the reason: a VM of that name
created with `strata vm` (adopt it, slice 3), a pool or network the host
does not have yet (run `nixos-rebuild` first), a kept data volume in the
way, a refused change, the project limits (counting VMs created with
`strata vm` too), or a protected VM that would be deleted or restarted.
VMs created with `strata vm` that are not declared are never touched. The
controller and `strata config`, which run this from Git with approval,
come in slice 3.

The options are data: they do not change the system `nixos-rebuild` builds.
They are checked whenever the configuration is evaluated, so **an invalid
workload also fails `nixos-rebuild`**, as for any other assertion.

## What is checked

At evaluation (assertions):

- names: VMs 1–63, disks 1–31 lowercase letters, digits and hyphens,
  starting with a letter; interfaces 1–15 lowercase letters and digits;
  a disk may not share an interface's name;
- a `root` disk of at least 1GiB; memory of at least 128MiB; at most 16
  disks and 8 interfaces;
- every disk's pool is declared in `strata.storage.pools` (or there is a
  `strata.storage.defaultPool`), and every network in
  `strata.networking.guest.networks`;
- `imageServer` is in `strata.compute.incus.imageServers` and comes with an
  `image`;
- the VMs together stay within `strata.compute.incus.limits.instances`,
  `.cpu` and `.memory`, where set (VMs created with `strata vm` are not
  counted here; the planner will check the host as it is);
- `strata.compute.enable` is set.

Go validates the IR again (`strata-controller validate-ir`) and is the
authority on the VM schema, for example on SSH key and image alias formats.

## The intermediate representation

```sh
nix eval --json /etc/nixos#nixosConfigurations.hv-01.config.strata.ir > ir.json
strata-controller validate-ir ir.json
```

`config.strata.ir` is the JSON document the controller will consume
(`ir.strata.dev/v1alpha1`, [ADR-0023](../../ARCHITECTURE.md#adr-0023-the-intermediate-representation)).
Evaluating it fails if any assertion of the configuration fails.
[internal/ir/testdata/standalone.json](../../internal/ir/testdata/standalone.json)
is the IR of the example. It contains:

| Field | Content |
|---|---|
| `apiVersion` | `ir.strata.dev/v1alpha1` |
| `generator.strataVersion` | The Strata version that produced it |
| `node` | `strata.node.id` |
| `host` | What the workloads were checked against: the Incus project, pools (name, driver), `defaultPool`, guest networks, allowed image servers, project limits |
| `resources[]` | One per workload, sorted by `id` |
| `resources[].id` | `vm/NAME`, stable for the VM's lifetime |
| `resources[].type` | `vm` (containers follow in M4) |
| `resources[].dependsOn` | The host objects the spec uses (`pool/NAME`, `network/NAME`), sorted |
| `resources[].lifecycle` | `{ "protected": bool }` |
| `resources[].spec` | The VM spec, `vm.strata.dev/v1alpha1`, with defaults resolved |

The same configuration always gives the same document, byte for byte: keys
and lists are sorted and there are no timestamps. Where the configuration
came from (repository, commit, flake.lock) is not part of the IR; the
controller records it with each deployment.
