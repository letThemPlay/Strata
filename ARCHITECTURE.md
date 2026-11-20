# Strata Architecture

**Status:** Milestone M1 in progress (slices 1–2 complete). This document describes the target
architecture and records which parts exist today. Anything not listed as
**Implemented** in [Current status](#current-status) is a plan, not a feature.

The full project specification lives in [CLAUDE.md](CLAUDE.md). This document
summarizes it and records the architectural decisions (ADRs) taken during
implementation.

## Goals

Strata is a declarative, bare-metal virtualization platform built on NixOS.
Desired infrastructure state is described in Nix, stored in Git, and
reconciled against the observed state of each host.

Core principles (see CLAUDE.md §2.1):

- **Declarative infrastructure** — desired state is Nix configuration.
- **Git is the source of truth** for desired state. PostgreSQL stores runtime
  inventory, operations and audit data only; it must never become a second
  source of desired configuration.
- **Reproducibility** — Nix flakes with pinned inputs.
- **Idempotency** — re-applying unchanged configuration produces no changes.
- **Modularity** — compute, storage, networking and container runtimes are
  independent providers behind interfaces.
- **Separation of concerns** — workloads keep running when the management
  plane is unavailable.
- **Data persistence** — VM disks and volumes are never disposable build
  artifacts and are never deleted implicitly.

## Logical planes

```text
┌──────────────────────── Management plane ────────────────────────┐
│  Web dashboard (React/TS)   CLI (strata)   REST API (strata-api) │
│  Authentication · Authorization (RBAC) · Audit logging           │
└───────────────────────────────┬──────────────────────────────────┘
                                │  /api/v1, WebSockets/SSE
┌──────────────────────── Control plane ───────────────────────────┐
│  strata-controller                                               │
│  Git sync → Nix eval → IR → validate → plan → approve → execute  │
│  → verify → record.  Scheduling, cluster coordination (later).   │
└───────────────────────────────┬──────────────────────────────────┘
                                │  authenticated agent protocol
┌──────────────────────── Infrastructure plane ────────────────────┐
│  strata-agent (privileged, per node)                             │
│  NixOS host · KVM/QEMU via libvirt · LXC · Podman                │
│  Storage providers (ZFS, LVM-thin, dir) · Networking (bridges,   │
│  VLANs, nftables)                                                │
└──────────────────────────────────────────────────────────────────┘
```

Privileged operations are only ever performed by `strata-agent`. The API
server never runs privileged commands directly from HTTP handlers.

## Configuration pipeline (target, M3)

1. Retrieve configuration from Git (read-only).
2. Resolve the pinned revision.
3. Evaluate Nix in a restricted environment, subject to a repository trust
   policy.
4. Emit a versioned JSON intermediate representation (IR).
5. Validate the IR.
6. Resolve dependencies.
7. Diff desired vs. observed state.
8. Produce an execution plan (create/update/delete/unreconcilable,
   disruptive and destructive changes flagged).
9. Require approval where needed (always for destructive operations).
10. Execute through the agent.
11. Verify resulting state — success is never reported before verification.
12. Record the deployment (repo, commit, flake lock rev, Strata version,
    node, timestamp, result).

The Go backend never interprets arbitrary Nix expressions; it consumes only
the IR.

## Repository layout

Directories are created when they gain real content (CLAUDE.md §5). Present
today:

| Path | Purpose |
|---|---|
| `flake.nix`, `flake.lock` | Pinned build, NixOS modules, dev shell, formatter, checks |
| `go.mod` | Go module `github.com/letthemplay/Strata` |
| `cmd/strata` | CLI client (`version`/`help` only) |
| `cmd/strata-api` | Management API server (`version`/`help` only) |
| `cmd/strata-agent` | Node agent: `run`, `discover` |
| `cmd/strata-controller` | Control-plane controller (`version`/`help` only) |
| `cmd/strata-install` | Host installer: `plan`, `apply` |
| `internal/version` | Build metadata injected via `-ldflags` |
| `internal/cli` | Shared, testable entrypoint and subcommand dispatch |
| `internal/hardware` | Hardware discovery from procfs/sysfs (no exec) |
| `internal/agent` | Agent run loop, atomic report writing, agent subcommands |
| `internal/install` | Answer file, `host.nix` rendering, disk and NIC probing, installation steps |
| `nix/packages/` | Package definitions (`strata`) |
| `nix/modules/` | NixOS modules: `base`, `agent`, `compute`, `access`, `networking`, `monitoring`, `disk` |
| `nix/hosts/` | Test values completing the host template into the reference host |
| `installer/partitioning/` | disko layouts used by `strata.disk` |
| `installer/iso/` | Installer ISO (NixOS module) and the host `flake.lock` it ships |
| `templates/host/` | Flake template for a host configuration repository |
| `nix/tests/` | Nix evaluation tests and NixOS VM tests (`nixos/`) |
| `examples/standalone/` | Non-normative sample infrastructure configuration |
| `docs/` | Development, administration and installation documentation |
| `.github/workflows/` | CI |

Planned (not yet created):
`internal/{compute,storage,networking,...}` (M2+), `internal/{gitops,schema,
planner,reconciler,executor}` (M3), `api/openapi`, `web/` (M5),
`internal/cluster` (M6).

## Current status

| Capability | Status |
|---|---|
| Nix flake (package, dev shell, formatter, checks) | **Implemented** |
| Go module and four binaries | **Implemented** (only the agent has functional commands) |
| Formatting (gofumpt, goimports, nixfmt) and linting (golangci-lint, statix, deadnix) | **Implemented** |
| Go unit tests, Nix evaluation tests, NixOS VM tests (including install-and-boot) | **Implemented** |
| GitHub Actions CI (all checks evaluated, non-VM checks built; VM tests run locally, ADR-0016) | **Implemented** |
| Sample infrastructure configuration | **Example only** — no schema, not consumed by any code |
| NixOS options `strata.{enable,package,node.id,node.role,agent.enable}` | **Implemented** (M1 slice 1, experimental) — see [options](docs/administration/options.md) |
| Hardware discovery (`strata-agent discover`, `/var/lib/strata/hardware.json`) | **Implemented** (M1 slice 1, experimental; schema `v1alpha1`) |
| Agent systemd service (unprivileged, no network) | **Implemented** (M1 slice 1, experimental) |
| NixOS options `strata.{compute,access,networking,monitoring}.*` (libvirt, key-only SSH, management network + nftables, node exporter) | **Implemented** (M1 slice 2, experimental) |
| `strata.disk` (disko ext4 layout, systemd-boot, UEFI), host flake template, reference host | **Implemented** (M1 slice 3, experimental) |
| Installer ISO and `strata-install plan/apply` (answer file, offline install) | **Implemented** (M1 slice 4a, experimental) |
| `strata-install init` (interactive answer file wizard) | **Implemented** (M1 slice 4b, experimental) |
| Physical hardware validation | **Pending** (M1 slice 5): [procedure](docs/installation/hardware-validation.md) ready, not yet run |
| VM management | Planned (M2); workload backend under evaluation (libvirt/LXC/Podman or Incus, [ADR-0015](#adr-0015-workload-backend-under-evaluation-incus)) |
| GitOps, IR, planner, reconciler | Planned (M3) |
| Containers (LXC, OCI), ZFS, LVM | Planned (M4); backend under evaluation ([ADR-0015](#adr-0015-workload-backend-under-evaluation-incus)) |
| REST API, auth, dashboard | Planned (M5) |
| Clustering | Planned (M6) |
| Ceph, backup, HA | Planned (M7) |

## M1 plan

M1 ("install and boot Strata on physical hardware") is delivered in slices:

1. `strata.*` module skeleton, node identity, hardware discovery, agent
   service. **Done.**
2. Host modules: `strata.compute` (libvirtd/QEMU; KVM modules are
   autoloaded by udev from CPU features, nothing vendor-specific is
   hardcoded), SSH key-only access, management networking with nftables,
   node exporter on the management network. **Done.**
3. Reference host configuration and disko layouts: `strata.disk` (ext4 on a
   single disk, UEFI, systemd-boot), the `host` flake template and the
   reference host built from it, installed and booted in a VM test.
   Mirrors, ZFS root and BIOS boot are deferred. **Done.**
4. Installer ISO and `strata-install`, in two parts:
   - 4a: `plan`/`apply` driven by a JSON answer file; offline installation
     from the ISO; typed confirmation before erasing the disk; the host flake
     is stored in `/etc/nixos` as a Git repository. **Done.**
   - 4b: `init`, an interactive wizard that writes the answer file, checks
     it like `plan` and offers to install. **Done.**
5. Installation guide and validation on physical Intel and AMD hardware.
   Virtualization is validated at the host level (`/dev/kvm`, the hardware
   report), not by managing workloads (ADR-0015). The guide and the
   [validation procedure](docs/installation/hardware-validation.md) are
   written; the `nixos-installer` test builds what a physical install builds
   (redistributable firmware, microcode). **Pending: running the procedure
   on physical machines.**

### M1 status

Everything except the acceptance run on physical hardware is done and
passes in VMs. **M1 is not accepted until the
[hardware validation](docs/installation/hardware-validation.md) passes on
at least one Intel and one AMD machine.** No workload management (libvirt,
LXC, Podman, Incus) exists: the agent and the Go code contain none, and on
a host libvirtd runs as a host service only, unused by Strata.

Known limitations at the end of M1:

- **Not validated on physical hardware** (see above).
- One system disk layout: ext4 on a single disk, UEFI only, systemd-boot,
  no mirroring, no swap, no Secure Boot.
- Management network: one interface, static or DHCP; no VLAN or bond.
  Disruptive network changes are not rolled back automatically (ADR-0011).
- Access: one admin account, SSH keys only; its key is root-equivalent
  (ADR-0010).
- The installer is x86_64 only and large (it carries a host closure and
  build tools; ADR-0014). Hosts are pinned to the ISO's Strata and nixpkgs
  revisions until updated with `nix flake update`.
- The agent only discovers hardware; it has no API and no network access
  (ADR-0007).
- `strata.compute` enables libvirtd although no workloads are managed; it
  may change with the workload backend decision (ADR-0015).
- `strata`, `strata-api` and `strata-controller` are placeholders.

## Architecture decision records

### ADR-0001: Lowercase binary names and NixOS option namespace

- **Context:** CLAUDE.md names binaries `Strata`, `Strata-api`, etc. and uses
  `Strata = { ... }` as the proposed NixOS option namespace.
- **Decision:** Use lowercase: `strata`, `strata-api`, `strata-agent`,
  `strata-controller`, and `strata.*` for NixOS options. The Go module path
  is `github.com/letthemplay/Strata`, matching the repository name.
- **Rationale:** Lowercase executable names and option namespaces are the Go
  and NixOS convention, avoid case-sensitivity surprises in shells, systemd
  units and package names.
- **Status:** Accepted (confirmed with project owner, M0).

### ADR-0002: Pin nixpkgs to `nixos-unstable` during early development

- **Decision:** The flake follows `nixos-unstable`; `flake.lock` pins the exact
  revision. Re-evaluated at the start of M1: host images and the installer
  ISO also stay on `nixos-unstable` for now (owner decision). Revisit before
  a production release.
- **Consequence:** The Go toolchain version is whatever the pinned nixpkgs
  provides (Go 1.27.x at the time of writing); `go.mod` declares the minimum
  (`go 1.26`). `GOTOOLCHAIN=local` prevents implicit toolchain downloads.

### ADR-0003: No flake framework; explicit system list

- **Decision:** `flake.nix` has a single input (`nixpkgs`) and an explicit
  `systems` list (`x86_64-linux`, `aarch64-linux`) instead of flake-utils or
  flake-parts.
- **Rationale:** Fewer inputs, less indirection. Strata targets Linux
  hypervisors only.

### ADR-0004: No third-party Go dependencies in M0

- **Decision:** M0 uses only the standard library (`vendorHash = null`).
  Structured logging will use `log/slog` from the standard library.
- **Consequence:** When the first dependency is added, `vendorHash` must be
  set to the real hash and `go.sum` committed.

### ADR-0005: Sample configuration is non-normative until M3

- **Decision:** `examples/standalone/strata.nix` is a plain attribute set that
  illustrates the intended configuration shape. It is not a NixOS module and
  no code consumes it. `nix/tests/examples.nix` only checks structure and
  cross-references (pools, bridges, VLAN range).
- **Rationale:** The schema and versioned IR are M3 deliverables; defining
  them now would pre-empt that design.

### ADR-0006: Tests run inside the Nix build

- **Decision:** The `strata` package runs `go test` over all packages in its
  `checkPhase`, so `nix flake check` and `nix build` both gate on unit tests.
  Tests must not depend on link-time values (the Nix build injects real
  version metadata).

### ADR-0007: Agent runs unprivileged and without network access until needed

- **Context:** CLAUDE.md requires a dedicated privileged node agent for local
  infrastructure operations, and least privilege everywhere.
- **Decision:** While the agent only performs hardware discovery, it runs as
  a systemd `DynamicUser` with an empty capability bounding set,
  `NoNewPrivileges`, `ProtectSystem=strict`, `RestrictAddressFamilies=AF_UNIX`
  and `IPAddressDeny=any`. It has no listening socket. `PrivateNetwork=` is
  deliberately not used because it hides the host's interfaces from sysfs.
  Each privilege (e.g. libvirt access in M2) is granted explicitly by the
  module that needs it, with a matching test.
- **Consequence:** The hardware report lives under
  `/var/lib/private/strata` (exposed as `/var/lib/strata`) and is readable by
  root only. Remote access to the agent arrives with authentication (M5).
- **Update (M1 slice 2):** the agent still has no libvirt access. Nothing in
  the agent uses libvirt yet; access is added with the M2 libvirt provider.
- **Update (ADR-0015):** which workload backend the agent talks to in M2 is
  open (libvirt or Incus). The privilege rule stands either way.

### ADR-0008: Versioned hardware report

- **Decision:** The hardware report is JSON with
  `apiVersion: hardware.strata.dev/v1alpha1`. Discovery reads only procfs and
  sysfs through an `fs.FS` and never executes external commands. Node identity
  is published as `/etc/strata/node.json` with
  `apiVersion: node.strata.dev/v1alpha1`.
- **Consequence:** `v1alpha1` may change incompatibly; consumers must check
  `apiVersion`. Fields needing root (e.g. DMI serial numbers) are omitted.
- **Freshness:** the agent rediscovers every minute (and on reload), so
  hardware that appears after boot is reported. A one-shot discovery at
  start-up proved insufficient: on CI the KVM module loaded after the agent's
  first discovery, leaving `kvmDevice: false` in the report indefinitely.

### ADR-0009: Installation uses disko and a CLI installer

- **Decision:** Disk partitioning uses nix-community/disko with Strata-provided
  layouts rather than a custom partitioner. The M1 installer is a CLI
  (`strata-install`) driven by a versioned answer file or interactive
  prompts. A web installer is deferred until the M5 API and authentication
  exist.
- **Status:** Accepted. Layouts implemented in M1 slice 3 (ADR-0012); the
  installer follows in slice 4.

### ADR-0010: Administrative access through a dedicated admin account

- **Decision:** SSH accepts public keys only. Root login is disabled; admins
  log in as a dedicated, password-locked account (`strata-admin` by default)
  in `wheel` with passwordless sudo. `AllowUsers` is limited to that account
  by default. At least one key is required (lockout prevention).
- **Rationale:** Logins are attributable to an account rather than to root,
  and root is never exposed to the network.
- **Consequence:** With no passwords, the admin's SSH keys are
  root-equivalent. Per-admin accounts and finer-grained authorization arrive
  with RBAC (M5).

### ADR-0011: systemd-networkd and an interface-scoped firewall for management

- **Decision:** Strata hosts use systemd-networkd. The management interface is
  configured by `10-strata-management.network` (static or DHCP). The nftables
  firewall opens no ports globally; management services are allowed only on
  the management interface, and globally opened ports fail evaluation.
  Services may still bind to all addresses: the firewall is the enforcement
  point (verified by the `nixos-host` VM test from a non-management network).
- **libvirt networks:** NixOS defines libvirt's `default` NAT network but does
  not autostart it. Strata leaves it inactive; the VM test asserts that no
  libvirt network is active or autostarted and that `virbr0` does not exist.
  Guest networking is provided by Strata network providers (M2).
  This paragraph is libvirt-specific and is revisited with ADR-0015.
- **Known limitation:** disruptive network changes are not rolled back
  automatically yet. Recovery relies on NixOS generations (`nixos-rebuild
  test`, boot-menu rollback) and console access. Automatic rollback belongs to
  the M3 executor. VLAN-tagged and bonded management interfaces are deferred
  to M2.

### ADR-0012: System disk layout: ext4 on one disk, UEFI, systemd-boot

- **Context:** M1 must install Strata on physical hardware. ADR-0009 chose
  disko; this records the first layout and how it is consumed.
- **Decision:** disko is a pinned flake input (following Strata's nixpkgs);
  `nixosModules.default` imports its module. `strata.disk` selects a layout
  from `installer/partitioning/` and a target device. The only layout,
  `ext4`, is a GPT disk with a 1 GiB EFI system partition on `/boot`
  (`umask=0077`) and an ext4 root filling the rest. Boot is UEFI only, with
  systemd-boot limited to 10 generations. Filesystems are mounted by GPT
  partition label, so the device path matters only during formatting.
- **Rationale:** One declaration produces both the installer's formatting
  script and the installed system's `fileSystems`, so they cannot drift. The
  smallest layout that boots on current server hardware comes first; every
  additional layout needs its own install-and-boot test.
- **Consequences:** the system disk holds the operating system only.
  Workload storage is kept off the system disk: it will live in explicitly
  configured pools on separate disks, with allocation policies (ADR-0015).
  Reinstalling formats the whole system disk, including those VM disks.
  No swap partition is created. Mirrored system disks, ZFS root and legacy
  BIOS boot are not supported yet. The `nixos-install-ext4` VM test installs
  the reference host with this layout and boots it from the disk with OVMF;
  it shares the host's Nix store with the VM, so copying the system closure
  is tested by the installer (M1 slice 4), not here. The test runs on
  x86_64 only.

### ADR-0013: One flake per host, created from a template

- **Decision:** A host's configuration is a flake in the operator's own Git
  repository, created with `nix flake init -t github:letThemPlay/Strata#host`.
  It takes nixpkgs from Strata's input (`nixpkgs.follows = "strata/nixpkgs"`)
  so hosts run the nixpkgs revision Strata is tested with. The template sets
  every Strata option except values only the operator knows (admin keys,
  install disk, management interface); those are left unset so evaluation
  fails with an assertion naming each one, instead of installing a host
  nobody can log in to.
- **Rationale:** The host repository is the Git source of truth that M3
  synchronizes from (CLAUDE.md §8). The installer (M1 slice 4) fills in the
  same three values plus a generated `hardware-configuration.nix`.
- **Testing:** `nixosConfigurations.reference` is the template plus test values
  (`nix/hosts/reference.nix`). It is built for the CI system and evaluated
  for every supported system, and
  evaluation tests check that the unfilled template fails on exactly the
  three missing values.
- **Update (M1 slice 4a):** machine-specific values (node ID, admin keys,
  install disk, management interface and addressing) moved from
  `configuration.nix` to `host.nix`, a plain attribute set that
  `strata-install` generates. `flake.nix` names the configuration after
  `strata.node.id` read from `host.nix`, so `nixos-rebuild --flake .` finds
  it by hostname. Strata hosts enable flakes and include Git.

### ADR-0014: Offline installer that builds before it erases

- **Context:** M1 must install Strata on physical hardware (ADR-0009: CLI
  installer, answer file, disko).
- **Decision:**
  - `strata-install` reads a strict, versioned JSON answer file
    (`install.strata.dev/v1alpha1`; unknown fields are errors). It generates
    the host flake from `templates/host`: template files are copied
    unchanged, `host.nix` is rendered from validated answers with every
    value emitted as an escaped Nix string literal (no answer file content
    can become Nix code), and `hardware-configuration.nix` comes from
    `nixos-generate-config --no-filesystems`.
  - **Build before erase:** the system and its disko formatting script are
    built first. Only then does the installer ask the operator to type the
    disk's device path (or accept `--confirm-wipe DEVICE`, which must match
    exactly) and run anything that writes to the disk. A configuration that
    does not evaluate or build never costs a disk.
  - **Offline:** the ISO carries the Strata, nixpkgs and disko sources, the
    reference host's closure and the tools to build per-machine derivations;
    Nix substituters are disabled. The host `flake.lock`
    (`installer/iso/host-lock.nix`) pins Strata to the ISO's source by
    `narHash` (revision too, for clean builds) and reuses Strata's own lock
    for everything else, so the installed host runs exactly what the ISO
    tested and Nix resolves every input from the local store.
  - The host flake is stored in `/etc/nixos` as a Git repository with one
    commit, and its inputs are copied into the installed system's store.
    The operator pushes it to their own repository; the installer holds no
    Git credentials.
  - External programs (`nix`, `nixos-install`, `git`, the disko script) are
    run with explicit arguments and timeouts, never through a shell.
  - `strata-install init` (slice 4b) is an interactive front end that only
    produces an answer file: it offers the machine's whole disks (by their
    `/dev/disk/by-id/` names, with the same checks as `plan`) and
    interfaces, validates every answer with the answer file's rules, writes
    the file and then follows the same `plan`/`apply` path, including the
    typed erase confirmation. Hand-written and generated answer files are
    therefore treated identically. SSH keys can be pasted, read from a file
    or downloaded over HTTPS (redirects must stay on HTTPS, size and time
    are limited); each key is shown with its SHA256 fingerprint for the
    operator to compare before it is accepted. This download is the only
    optional network use of the installer. Key lines are checked beyond
    their syntax: the base64 data must decode and name the key type the line
    claims.
- **Testing:** unit tests use a fake command runner and filesystem fixtures,
  including the rule that nothing destructive runs before a successful build
  and a confirmation. The `nixos-installer` VM test boots the real ISO image
  (a test variant) under UEFI with no outside network, checks that a
  mismatched confirmation leaves the disk blank, installs, then boots the
  disk alone and checks that `/etc/nixos` evaluates offline to exactly the
  running system. `--extra-module` (a site-specific NixOS module copied into
  the host flake) is how the test adds the test driver's instrumentation.
  The test drives `init` with scripted input: it compares the written
  answer file with the expected one and installs through `init`'s typed
  confirmation.
- **Consequences:** the ISO is large (it carries a host's closure plus build
  tools). The offline build-tool list is maintained by the VM test: a
  missing tool fails it. The test must vary every per-host value: for
  example, the nftables ruleset is checked at build time in an `lkl`
  sandbox, which is only needed once the management interface differs from
  the reference host's. Build-time-only inputs are in no runtime closure
  and must be listed explicitly: CPU microcode, which is prepended to the
  initrd, is only enabled by `nixos-generate-config` on physical machines,
  so the test's extra module enables redistributable firmware and both
  vendors' microcode as a physical install does. ISOs built from a dirty
  tree pin a placeholder revision (the `narHash` still pins the source);
  such hosts need `nix flake update` before fetching from GitHub. Only
  x86_64 ISOs are tested.

### ADR-0015: Workload backend under evaluation (Incus)

- **Status:** open; Incus adoption is not approved. The formal decision,
  together with updates to CLAUDE.md, this document and the affected ADRs,
  is made after M1 and before M2 starts.
- **Context:** CLAUDE.md specifies three independent workload providers:
  libvirt for VMs, LXC for system containers and Podman for OCI containers.
  Incus is being evaluated as the primary backend for both VMs and
  containers instead. Incus manages QEMU/KVM VMs and LXC containers itself,
  can run OCI images as containers, and has its own REST API, storage pools,
  networks, projects and clustering.
- **Interim decision (rest of M1):**
  - No direct libvirt workload management and no independent LXC or Podman
    providers are implemented.
  - Nothing new assumes that Strata manages VM or container lifecycles
    directly.
  - Host-level work (NixOS modules, hardware discovery, installation, host
    networking, disk and storage preparation) stays independent of the
    workload backend. The agent stays backend-agnostic where practical: it
    has no libvirt access and no workload code.
  - Existing code is not restructured and no Incus integration is added
    until the architecture is reviewed and approved.
  - libvirtd and its tests stay, as a host-level service only: `strata.compute`
    enables it, nothing in Strata manages workloads through it, and the
    agent has no access to it.
  - No etcd and no Incus clustering changes.
  - **Workload storage is separate from the system disk** (decided): it will
    use explicitly configured pools on separate disks with allocation
    policies, whichever backend is chosen. The system disk layout
    (ADR-0012) holds the operating system only.
- **Security design issue for the review: agent access to the Incus API.**
  Access to the Incus Unix socket is equivalent to root on the host (it can
  create privileged instances and attach host devices and paths). Granting
  the agent unrestricted access to it is **not** an accepted solution. The
  review must decide how the agent is restricted, for example: a dedicated
  Incus project with restricted settings (no privileged instances, no host
  device or path passthrough, limits), a TLS client identity confined to
  that project instead of the root socket, or a separate privileged helper
  that exposes only the operations Strata plans and audits. Until then the
  agent has no access to any workload backend.
- **Already backend-independent:** the `strata.{enable,node,agent,access,
  networking,monitoring,disk}` modules, the hardware report
  (`hardware.strata.dev/v1alpha1`), the agent, the installer and answer file,
  the host flake template and the offline ISO. Incus on NixOS requires the
  nftables firewall, which Strata already uses (ADR-0011).
- **Decisions to revisit if Incus is adopted:**
  - **CLAUDE.md** (technology stack, §10 workload providers, the libvirt
    rule in §3, etcd for cluster coordination in §16, the `/lxc` and `/oci`
    API resources): the specification must be revised first; it is the
    authority for these choices.
  - **`strata.compute`** enables libvirtd with unprivileged QEMU and swtpm.
    The module would enable Incus instead, or select a backend; its option
    name and documentation describe a libvirt host today. The checks that
    assert libvirtd (`module-eval`, `nixos-host`, `nixos-install-ext4`,
    `nixos-installer`) change with it.
  - **ADR-0007:** how the agent gets restricted access to the Incus API (see
    the security design issue above), instead of libvirt access.
  - **ADR-0011:** the libvirt NAT network paragraph; Incus bridges, its own
    nftables rules and, if enabled, its HTTPS API port on the management
    interface need the same interface-scoped firewall treatment.
  - **M4 storage:** whether workload volumes are Incus storage pools
    (LVM, ZFS, ...) or Strata-managed volumes. Either way they live on
    separate disks with explicit pool configuration (decided above).
  - **M3 IR and reconciler:** whether Strata plans against its own provider
    interfaces or translates the IR into Incus resources (instances,
    profiles, projects) and lets Incus manage lifecycles.
  - **M6 clustering:** Incus has built-in clustering; CLAUDE.md plans etcd
    and a central controller.
  - **M5 authentication and RBAC:** relationship to Incus's own TLS client
    certificates, projects and authorization.
  - **README and ARCHITECTURE.md overview** (diagram and summary name
    libvirt, LXC and Podman).

### ADR-0016: NixOS VM tests run locally, not in CI

- **Context:** the NixOS VM tests (`checks.<system>.nixos-*`) boot up to
  three guests each and, for the installer, build an ISO image. On GitHub's
  shared 4-CPU runners they ran slowly and, run concurrently, starved the
  runner until guests took minutes to boot and runs stalled until the job
  timeout. They are deterministic and pass locally.
- **Decision:** CI evaluates every check for every system and builds every
  check except the VM tests (`make nix-check-novm`), then runs the format,
  lint and Go test targets. The VM tests run locally, with `make vm-test`
  or `make nix-check`, on a machine with KVM.
- **Consequences:** a regression that only a VM test detects is not caught
  by CI. Contributors run `make vm-test` before pushing changes to the Nix
  modules, the installer, the ISO or the tests, and state in the pull
  request that it passed. A broken test definition still fails CI, because
  the tests are evaluated. This can be revisited with dedicated runners
  that have KVM and enough CPU and memory.
