# Strata Architecture

**Status:** Milestones M1 and M2 complete (M2: Incus host, the agent's
restricted identity, storage pools, guest networks and VM management
through the CLI, validated on physical hardware). M3 has not started. Incus
is the workload backend (ADR-0015). This document describes the target
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
- **Modularity** — workload, storage and networking concerns are separated
  behind interfaces; the workload backend (Incus) is reached only through
  its provider package.
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
│  strata-agent (per node; restricted Incus identity)              │
│  NixOS host · Incus: KVM/QEMU VMs, LXC system containers,        │
│  OCI application containers                                      │
│  Incus storage pools (ZFS, LVM-thin, dir) · Networking           │
│  (bridges, VLANs, nftables)                                      │
└──────────────────────────────────────────────────────────────────┘
```

Privileged operations are only ever performed by `strata-agent`. The API
server never runs privileged commands directly from HTTP handlers. The
agent reaches Incus through a restricted TLS identity, never through the
Incus Unix socket (ADR-0015).

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
| `cmd/strata` | CLI client: `strata vm` (local agent API) |
| `cmd/strata-api` | Management API server (`version`/`help` only) |
| `cmd/strata-agent` | Node agent: `run` (discovery, backend status, local API), `discover` |
| `cmd/strata-install` | Host installer: `plan`, `apply` |
| `internal/version` | Build metadata injected via `-ldflags` |
| `internal/cli` | Shared, testable entrypoint and subcommand dispatch |
| `internal/hardware` | Hardware discovery from procfs/sysfs (no exec) |
| `internal/agent` | Agent run loop, atomic report writing, agent subcommands |
| `internal/install` | Answer file, `host.nix` rendering, disk and NIC probing, installation steps |
| `internal/incus` | Incus API client: loopback HTTPS with a client identity and pinned server certificate (agent); Unix socket (`strata-host` only); VM provider |
| `internal/workload` | Backend-neutral workload schemas (`vm.strata.dev/v1alpha1`) and provider errors |
| `internal/agent/api` | The agent's local API (Unix socket, peer-uid authorization, audit, operations) |
| `internal/vmcli` | `strata vm` |
| `internal/blockdev` | Block device inspection (procfs, sysfs, udev database): resolution, signatures, uses, system disks; shared by the installer and pool setup |
| `internal/hostsetup`, `cmd/strata-host` | Root-only Incus setup run by NixOS: project, agent identity and trust, storage pools |
| `nix/packages/` | Package definitions (`strata`) |
| `nix/modules/` | NixOS modules: `base`, `agent`, `compute`, `access`, `networking`, `monitoring`, `disk`, `storage`, `workloads` |
| `nix/hosts/` | Test values completing the host template into the reference host |
| `installer/partitioning/` | disko layouts used by `strata.disk` |
| `installer/iso/` | Installer ISO (NixOS module) and the host `flake.lock` it ships |
| `templates/host/` | Flake template for a host configuration repository |
| `nix/tests/` | Nix evaluation tests and NixOS VM tests (`nixos/`) |
| `examples/standalone/` | Example host with declared workloads (its IR is checked by the `examples` check) |
| `internal/ir` | The IR (`ir.strata.dev/v1alpha1`): parsing and validation |
| `internal/controller`, `cmd/strata-controller` | Control plane; currently `validate-ir` |
| `docs/` | Development, administration and installation documentation |
| `.github/workflows/` | CI |

Planned (not yet created):
`internal/{networking,...}` (later), `internal/{gitops,planner,reconciler,executor}` (M3), `api/openapi`, `web/` (M5),
`internal/cluster` (M6).

## Current status

| Capability | Status |
|---|---|
| Nix flake (package, dev shell, formatter, checks) | **Implemented** |
| Go module and four binaries | **Implemented** (only the agent has functional commands) |
| Formatting (gofumpt, goimports, nixfmt) and linting (golangci-lint, statix, deadnix) | **Implemented** |
| Go unit tests, Nix evaluation tests, NixOS VM tests (including install-and-boot) | **Implemented** |
| GitHub Actions CI (all checks evaluated, non-VM checks built; VM tests run locally, ADR-0016) | **Implemented** |
| Dependabot (Actions, flake inputs, Go modules), CodeQL (Go, Actions), govulncheck gate with an expiring allow-list, SHA-pinned actions | **Implemented** (ADR-0022) |
| Sample infrastructure configuration | **Example only** — no schema, not consumed by any code |
| NixOS options `strata.{enable,package,node.id,node.role,agent.enable}` | **Implemented** (M1 slice 1, experimental) — see [options](docs/administration/options.md) |
| Hardware discovery (`strata-agent discover`, `/var/lib/strata/hardware.json`) | **Implemented** (M1 slice 1, experimental; schema `v1alpha1`) |
| Agent systemd service (unprivileged, no network) | **Implemented** (M1 slice 1, experimental) |
| NixOS options `strata.{compute,access,networking,monitoring}.*` (compute host, key-only SSH, management network + nftables, node exporter) | **Implemented** (M1 slice 2, experimental) |
| Incus on compute hosts (replaces libvirtd), restricted Strata project, agent identity and backend status (`/var/lib/strata/backend.json`) | **Implemented** (M2 slice 1, experimental; ADR-0017) |
| `strata.disk` (disko ext4 layout, systemd-boot, UEFI), host flake template, reference host | **Implemented** (M1 slice 3, experimental) |
| Installer ISO and `strata-install plan/apply` (answer file, offline install) | **Implemented** (M1 slice 4a, experimental) |
| `strata-install init` (interactive answer file wizard) | **Implemented** (M1 slice 4b, experimental) |
| Physical hardware validation | **Passed** (M1 slice 5): Intel (Lenovo ThinkServer TS140) and AMD (GMKtec M6 Ultra). See [results](docs/installation/hardware-validation.md#results) |
| `strata.storage` pools on dedicated disks (ZFS, LVM-thin, dir), project limits, LVM autoactivation policy | **Implemented** (M2 slice 2, experimental; ADR-0018) |
| `strata.networking.guest` networks (Incus bridges on uplinks, untagged or VLAN, or isolated; no host address) | **Implemented** (M2 slice 3, experimental; ADR-0019) |
| VM schema `vm.strata.dev/v1alpha1`; `strata vm create/list/show/start/stop/restart/update/delete` through the agent's local API (operations, verification, audit; plans confirmed by ID; deletion keeps data volumes); image server allow-list | **Implemented** (M2 slices 4a–4b, experimental; ADR-0020, ADR-0021) |
| M2 acceptance on physical hardware | **Passed** (GMKtec M6 Ultra, AMD; see [results](docs/installation/hardware-validation.md#m2-results)) |
| NAT/routed guest networks; bonds | Planned (later) |
| `strata.workloads.vms` and the IR `ir.strata.dev/v1alpha1` (`config.strata.ir`, `strata-controller validate-ir`); not applied yet | **Implemented** (M3 slice 1, experimental; ADR-0023) |
| GitOps, planner, reconciler | Planned (M3 slices 2–3) |
| System and OCI containers, volume management | Planned (M4), through Incus ([ADR-0015](#adr-0015-incus-is-the-workload-backend)) |
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
   (redistributable firmware, microcode). **Intel passed** (ThinkServer
   TS140, 2026-11-23); its findings led to the console password, the
   existing-data warning and the management interface fix. **AMD passed**
   (GMKtec M6 Ultra, 2026-11-23), including those fixes. **Done.**

### M1 status

**M1 is complete.** Everything passes in VMs, and the
[hardware validation](docs/installation/hardware-validation.md) passed on
one Intel and one AMD machine. No workload management (libvirt,
LXC, Podman, Incus) exists: the agent and the Go code contain none, and on
a host libvirtd runs as a host service only, unused by Strata. After M1,
Incus was chosen as the workload backend (ADR-0015).

Known limitations at the end of M1:

- Validated on two physical machines (one Intel, one AMD).
- The IOMMU is not enabled on the kernel command line, so on Intel there
  are no IOMMU groups even with VT-d enabled (AMD-Vi is on by default).
  Needed later for PCI passthrough and DMA protection; a host-level,
  backend-independent setting (proposed: a `strata.compute` option, on by default).
- Installing from remote IP KVM virtual media (JetKVM) did not work; use a
  USB stick.
- On reused machines, LVM groups on other disks were activated
  automatically at boot (standard NixOS behaviour). Settled in M2 slice 2:
  only declared pools' volume groups activate (ADR-0018).
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
- `strata.compute` enabled libvirtd although no workloads were managed;
  M2 slice 1 replaced it with Incus (ADR-0015).
- `strata`, `strata-api` and `strata-controller` are placeholders.

## M2 plan

M2 ("manage VMs through the CLI") is delivered in slices, through Incus
(ADR-0015):

1. Incus on compute hosts instead of libvirtd; the API on loopback only; a
   restricted Strata project whose configuration is enforced on every boot;
   the agent's restricted TLS identity and backend status (ADR-0017),
   proven by the `nixos-incus` VM test. **Done.**
2. Storage pools on dedicated disks (ZFS, LVM-thin, directory), declared in
   NixOS, created only on empty disks or adopted, never deleted; project
   limits; LVM autoactivation limited to declared pools (ADR-0018). The
   project setup moved from a shell script to `strata-host` (Go). **Done.**
3. Guest networks: Incus bridges declared in NixOS, on uplinks (untagged
   or VLAN) or isolated, with no host address; the project may use exactly
   the declared networks (ADR-0019). **Done.**
4. VM management through the CLI, in two parts:
   - 4a: the VM schema, the Incus VM provider, the agent's local API with
     operations, verification and audit, and `strata vm create/list/show/
     start/stop/restart` (ADR-0020, ADR-0021). **Done.**
   - 4b: `update` with a plan confirmed by its ID, `delete` with typed
     confirmation keeping data volumes, and the image server allow-list
     (ADR-0021). **Done.**

### M2 status

**M2 is complete.** Everything passes in VMs (eight NixOS VM tests), and
the M2 section of the [hardware validation](docs/installation/hardware-validation.md#m2-vm-management)
passed on one physical machine (GMKtec M6 Ultra, AMD): a ZFS pool on an
added disk, a guest network on the second NIC, a VM created from an
allow-listed image server, reached over SSH, updated with a plan, started
again after a host reboot and deleted with its data volume kept.

Known limitations at the end of M2:

- VM management is local to each host (`strata vm` through the agent's
  socket); remote management, accounts and roles come with M5.
- Changes to a running VM restart it; live hotplug is not used.
- Data volumes are kept but cannot be managed through Strata (M4).
- Operation records are kept in memory only (until M3/M5).
- Guest networks are bridged or isolated; no NAT or routed networks, no
  bonds, no VLAN or bonded management interface.
- NIC names are pinned to MAC addresses (`strata.networking.interfaces`,
  ADR-0011), since a hardware change renamed a NIC on hardware. Existing
  hosts must add the pins themselves (a warning asks them to).
- The VM spec passes only SSH keys to cloud-init (`cloudInit.packages` is
  proposed).
- The first ZFS pool on a host needs a reboot after the rebuild.

## M3 plan

M3 ("Git changes generate safe and repeatable infrastructure changes")
puts a declarative engine in front of the M2 execution path:

```
Git remote --fetch (read-only)--> strata-controller --Unix socket--> strata-agent --restricted TLS--> Incus
                                   verify signature, nix eval -> IR,
                                   validate, plan, approve, record
strata config ... (CLI) --Unix socket--^
```

Decisions taken for the milestone (each gets an ADR in the slice that
implements it):

1. Workloads are declared in the host flake (`strata.workloads.*`); the IR
   is `nixosConfigurations.<node>.config.strata.ir`. A workload-only commit
   leaves `flake.lock` alone, so evaluation needs no network. To be
   revisited when the cluster is designed (M6).
2. `strata-controller` is a separate unprivileged service with its own
   socket; it reaches workloads only as a client of the agent.
3. Only commits signed by an allowed key are evaluated, and the tracked
   branch must only move forward from the last applied commit.
4. Every plan needs approval by default (by plan ID); an opt-in
   `autoApply = "safe"` covers plans without restarts or deletions.
   Deletions always need approval.
5. A VM removed from the configuration is deleted only with approval; data
   volumes are always kept.
6. VMs carry an owner (`gitops` or `cli`); the engine manages only its own,
   adoption is explicit, and the agent refuses `strata vm update/delete` on
   Git-managed VMs.
7. Deployment records and plans are JSON files until PostgreSQL arrives
   with the API server (M5).
8. Git is accessed with the `git` binary (fixed arguments, no shell), into a
   bare repository; Nix evaluates from `git+file://...?rev=<sha>`.

Slices:

1. The IR and the Nix side: `strata.workloads.vms`, `config.strata.ir`,
   `internal/ir`, `strata-controller validate-ir` (ADR-0023). **Done.**
2. Planning and reconciliation against a mock provider: a plan across all
   VMs (create, update, delete, unreconcilable, drift), owner labels,
   adoption.
3. The controller: Git sync with signature and fast-forward checks,
   sandboxed evaluation, approval, execution through the agent, deployment
   records, `strata.gitops`, `strata config`, VM test with a Git server.
4. Validation on physical hardware.


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

- **Status:** superseded by ADR-0023 (M3): the example is now a host
  configuration with declared workloads, and the `examples` check validates
  its IR.
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
- **Update (ADR-0015):** the agent will never get libvirt access. In M2 it
  gets a TLS client identity that Incus restricts to Strata's projects,
  reached over loopback; never the Incus Unix socket. Its hardening is
  relaxed only for that (loopback TCP, its own key), with a test.
- **Update (M2 slice 1, ADR-0017):** on a compute host the agent may open
  IPv4/IPv6 sockets, but `IPAddressAllow=localhost` on top of
  `IPAddressDeny=any` limits it to loopback. Its identity arrives through
  `LoadCredential=`. Everything else is unchanged; without `strata.compute`
  the agent has no network access at all. A failure of
  `strata-incus-setup` (no identity to load) stops the agent from starting
  on a compute host, which `systemctl --failed` shows.

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
- **Update (M1 hardware validation): optional console password.** On the
  first physical install, a wrong management interface made the host
  unreachable, and with no password there was no console login either: the
  only recovery was to reinstall. `strata.access.admin.consolePasswordFile`
  now optionally gives the admin a password for the **local console**
  (physical, or through a remote KVM). SSH still accepts keys only. The
  installer asks for it, keeps only its hash, and writes it to
  `/etc/strata/admin-console-password` (root-only), outside the Nix store
  and the configuration repository, as CLAUDE.md requires for secrets. NixOS
  reads the file when it creates the account; later changes use `passwd`.
  With passwordless sudo, the console password is root-equivalent, like the
  keys; someone at the console can also boot other media, so it adds little
  exposure. Without it, the account stays locked as before.

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
  This paragraph is libvirt-specific: libvirtd is removed in M2 (ADR-0015)
  and replaced by Incus networks declared in NixOS. The Incus API is never
  opened on any interface, and no guest network may reach the management
  interface.
- **Update (M2 slice 1):** libvirtd is gone. The Incus API listens on
  `127.0.0.1:8443` only, and no Incus network exists until guest networks
  are declared (M2 slice 3); `nixos-host` asserts there is no managed Incus
  network and no `incusbr0`, `nixos-incus` that port 8443 is closed from the
  management network.
- **Update (M2 slice 3, ADR-0019):** guest networks are Incus bridges with
  no host address. The interface-scoped firewall is unchanged (nothing is
  opened on guest bridges); the bridges are only exempt from the
  reverse-path filter, which would drop bridged traffic passing nftables
  through `br_netfilter`. Uplinks and the VLAN interfaces Incus creates get
  networkd units with no address and `KeepMaster`, so networkd neither
  addresses nor detaches them.
- **Update (M2 follow-up): stable interface names.** Adding an NVMe drive
  renamed a PCI-path NIC on hardware. `strata.networking.interfaces.<name>.macAddress`
  pins a name to a NIC's permanent MAC address with a systemd `.link` file
  (system and initrd); networkd units for pinned interfaces match by MAC
  address, the firewall by the (now stable) name. Pinning uses the current
  name, so a rebuild renames nothing and cannot cut off management;
  another name takes effect at boot. `strata-install` pins the management
  interface (refusing a non-permanent MAC); unpinned management interfaces
  and uplinks produce warnings, not errors, so existing hosts keep
  building; `strata-host` reports pins whose NIC is missing or has another
  MAC. Verified by `module-eval`, the installer test and `nixos-nic-names`
  (two hosts with their NICs in opposite order end up with the same names
  on the same networks; without the `.link` files the test fails).
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
  - **Existing data** (update after the first physical install): `plan`,
    `apply` and `init` list the filesystems and inactive ZFS, LVM and RAID
    members found on the target disk (from udev's blkid results in
    `/run/udev/data`), and the wizard asks before it selects such a disk.
    Disks in use (mounted, swap, active LVM/RAID) stay unavailable. The
    typed confirmation is still required for every disk.
  - **Management interface** (same update): only the chosen interface is
    configured on the host, so the wizard pre-selects one only if it has a
    link, and otherwise asks the operator to connect the cable and re-scan.
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

### ADR-0015: Incus is the workload backend

- **Status:** Accepted (project owner, after M1, before M2). Supersedes the
  libvirt, LXC and Podman provider plan of CLAUDE.md 0.1.0; CLAUDE.md 0.2.0
  records the revised stack.
- **Context:** CLAUDE.md 0.1.0 specified three independent workload
  providers: libvirt for VMs, LXC for system containers and Podman for OCI
  containers, and etcd for cluster coordination. Incus manages QEMU/KVM VMs
  and LXC containers itself, runs OCI images as application containers, and
  has its own REST API, storage pools, networks, projects, restricted client
  identities and clustering. During M1 it was evaluated while host-level
  work stayed backend-independent (see "M1 interim rules" below).
- **Decision:**
  1. **Incus is Strata's single workload backend** for VMs, system
     containers and OCI application containers. Strata is the declarative
     layer: Git → Nix → IR → plan → Incus API → verify. Incus owns instance
     lifecycles; Strata does not reimplement them, and does not manage QEMU,
     LXC or Podman directly.
  2. **Package:** `incus-lts` (the NixOS module's default; 7.0.x in the
     pinned nixpkgs). Moving to feature releases needs a reason and an ADR
     update.
  3. **libvirtd is removed** from `strata.compute` in the first M2 slice,
     which enables Incus instead. A host has one QEMU manager.
  4. **OCI containers run as Incus OCI application containers.** Podman
     features without an Incus equivalent (health checks, pods, compose-style
     restart policies) are documented gaps, not reimplemented. Users who
     need them run Podman inside an Incus system container or VM. Revisited
     only for a concrete requirement.
  5. **Agent access: a restricted TLS identity, never the Unix socket**
     (resolves the security design issue recorded during M1):
     - The Incus API listens on loopback only (`core.https_address` on
       `127.0.0.1`/`[::1]`); it is never opened on any interface,
       including the management interface. The Incus web UI is not enabled.
     - The agent authenticates with a TLS client certificate that Incus
       trusts as **restricted** to the Strata project(s). It is not in the
       `incus-admin` group and has no access to `/var/lib/incus/unix.socket`
       or the `incus-user` socket.
     - Strata projects are created by NixOS (Incus preseed, as root) with
       `restricted=true`: unprivileged containers only, no nesting, no host
       path, PCI, USB, GPU, unix-char/block or raw device passthrough, no
       `raw.*` keys, and project limits (CPU, memory, disk, instances) as
       allocation policy.
     - **Server-level resources (storage pools, networks, server config,
       projects and their restrictions) are declared in NixOS**, not created
       by the agent. A restricted identity cannot change them.
     - The agent's systemd hardening changes only as far as this requires
       (ADR-0007): loopback TCP to Incus, read access to its own key.
     - **Must be proven before relying on it:** the first M2 slice adds a
       NixOS VM test showing that the agent's identity can manage instances
       in its project and is refused privileged containers, host devices and
       paths, `raw.*` keys, other projects, project and server
       configuration, and the Unix socket.
  6. **Workload storage** (decided during M1, unchanged): Incus storage
     pools (ZFS, LVM-thin, directory) on dedicated disks, declared in NixOS
     with explicit configuration, never on the system disk (ADR-0012).
     Allocation policy is expressed as project limits and per-pool
     configuration. Removing a pool from the configuration never deletes it
     or its data.
  7. **Clustering is deferred to M6.** CLAUDE.md no longer commits to etcd.
     Incus clustering is the likely candidate for workload placement and
     migration, to be designed in M6 together with quorum, fencing and
     storage consistency. No etcd and no Incus clustering before M6.
  8. **Strata's API and IR stay backend-neutral in their vocabulary**
     (VMs, system containers, OCI containers, volumes, networks), with
     Incus behind a provider interface (`internal/incus`) so that it can be
     replaced by a mock in tests. Incus concepts (profiles, raw config) are
     not exposed as Strata's public configuration.
- **Open questions for M2 (decided there, with tests):**
  - **Guest networking:** restricted projects only allow NICs on
    Incus-managed networks (`restricted.devices.nic=managed`). The default
    approach is Incus-managed networks declared in NixOS (bridges and VLANs
    on dedicated uplinks) and listed in `restricted.networks.access`,
    instead of NixOS bridges attached as unmanaged devices. Either way, no
    guest network may bridge, macvlan or otherwise reach the management
    interface (ADR-0011), and Incus's own nftables rules must coexist with
    the interface-scoped firewall.
  - How the agent's certificate is generated and enrolled (on the host at
    first boot, never stored in Git).
  - Mapping of the IR to Incus resources (instances, project per
    environment or one Strata project per host) and drift detection.
- **Consequences:**
  - **CLAUDE.md 0.2.0:** technology stack, §3 rules, §4.3, §5 layout
    (`internal/workload`, `internal/incus` instead of `compute`, `lxc`,
    `oci`), §6, §10 workload providers, §12 API resources (`/containers`
    instead of `/lxc`), §13, §14, §15, §16 (etcd removed), §17, §19 M2, M4
    and M6, §21.
  - **`strata.compute`** switches from libvirtd to Incus in the first M2
    slice; the checks that assert libvirtd (`module-eval`, `nixos-host`,
    `nixos-install-ext4`, `nixos-installer`) change with it, and the
    libvirt paragraph of ADR-0011 is replaced.
  - **M3 reconciler:** plans are computed against Incus's observed state
    through the provider interface.
  - **M5 authentication and RBAC:** Strata's API is the only management
    interface; Incus's TLS identities and projects are an implementation
    detail of the agent, not user accounts.
  - **Unchanged:** the `strata.{enable,node,agent,access,networking,
    monitoring,disk}` modules, the hardware report, the installer and answer
    file, the host template and the offline ISO. Incus on NixOS requires the
    nftables firewall, which Strata already uses (ADR-0011).
- **M1 interim rules (history):** until this decision, no libvirt workload
  management and no LXC or Podman providers were implemented, nothing
  assumed that Strata manages lifecycles directly, host-level work stayed
  backend-independent, libvirtd stayed as an unused host service, the agent
  had no access to any workload backend, and no etcd or clustering work was
  done. Workload storage separate from the system disk was decided then.

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

### ADR-0017: The agent's Incus identity

- **Status:** Accepted (M2 slice 1). Implements the agent access decision of
  ADR-0015.
- **Context:** ADR-0015 decided that the agent uses a TLS identity that
  Incus restricts to Strata's project, over a loopback-only API, never the
  Incus Unix socket. This ADR records how the identity is created, trusted
  and delivered, and what enforces the project's restrictions.
- **Decision:**
  - **Project:** `strata-incus-setup.service` (root, oneshot, after Incus)
    creates the project or replaces its whole configuration with a `PUT` on
    every boot, so changes made by hand are reverted. All restriction keys
    are listed explicitly (`restricted=true` plus each `restricted.*`
    value), so that a change of Incus's defaults cannot loosen them.
    Networks stay in the default project, where a restricted identity can
    only view them.
  - **Identity:** the same service generates an EC P-384 key and a
    self-signed client certificate (`CN=strata-agent@<node id>`, 10 years)
    in `/var/lib/strata-incus/` (0700, root) if they do not exist. The key
    never leaves the host and is not in Git or the Nix store.
  - **Trust:** exactly one trust entry named `strata-agent`: restricted,
    type client, projects = [the Strata project]. Entries with that name and
    another fingerprint, and an entry for this certificate with other
    permissions, are removed and the correct entry is added.
  - **Delivery:** the agent (a `DynamicUser`) receives the key, certificate
    and Incus's server certificate through `LoadCredential=`; it cannot read
    `/var/lib/strata-incus` or the Incus Unix socket, and is not in
    `incus-admin`.
  - **Connection:** `internal/incus` connects only to a loopback IP address,
    requires TLS 1.3 and accepts only the pinned server certificate (no CA,
    no host name). It uses the Go standard library only (ADR-0004); whether
    the Incus Go client is adopted is decided with the provider slice.
  - **Status:** every agent cycle checks `GET /1.0` in the project and
    writes `/var/lib/strata/backend.json`
    (`backend.strata.dev/v1alpha1`: backend, reachable, trusted, version,
    error). Failures are reported there and logged at warning level; they
    do not stop hardware discovery.
- **Verified by:** the `nixos-incus` VM test (allowed and refused operations
  with the agent's identity, loopback-only API, Unix socket denied,
  enforcement of project configuration and trust after tampering,
  containers surviving an incusd restart) and `module-eval`.
- **Consequences and limitations:**
  - The setup service is the only Strata code that uses the Incus Unix
    socket; it runs as root from NixOS at boot, never on request.
  - Rotating the identity means deleting `/var/lib/strata-incus/client.*`
    and restarting `strata-incus-setup` and `strata-agent`.
  - Snapshots and backups are blocked by the project restrictions until the
    storage slice decides them; image sources (`restricted.images.servers`)
    are decided with the VM schema slice.
- **Update (M2 slice 4a):** the project's `restricted.images.servers` is
  set to a name that is never a server, so Incus downloads no image on the
  project's behalf. Until then an empty value had allowed every server
  (Incus's default), although the agent itself has no network access.
- **Update (M2 slice 2):** the setup is now `strata-host incus-setup`
  (Go, `internal/hostsetup`), reading a JSON description of the desired
  state that NixOS generates, instead of a shell script; it runs again
  whenever that description changes. Behaviour is unchanged, with one
  addition: the project's allowed pools and limits are set (ADR-0018). The
  key is now created in Go (EC P-384, PKCS#8); existing identities are kept.
  Snapshots are allowed; backup export stays blocked until M7.

### ADR-0018: Storage pools on dedicated disks

- **Status:** Accepted (M2 slice 2). Implements the storage decision of
  ADR-0015 and settles the M1 LVM autoactivation finding.
- **Context:** workload storage is separate from the system disk, with
  explicit pool configuration and allocation policies (ADR-0015). Incus
  creates ZFS and LVM pools on block devices itself (`zpool create`,
  `pvcreate`/`vgcreate`); with `source.wipe` it would erase them first.
  Data loss from a wrong device name must be impossible by construction.
- **Decision:**
  - **Declared in NixOS** (`strata.storage.pools`): driver `zfs` (one disk or
    a two-disk mirror), `lvm` (one disk, thin pool) or `dir` (a mount point
    declared in `fileSystems`, pool in `<path>/<name>`). Devices are
    `/dev/disk/by-id/` paths, used by one pool only, never
    `strata.disk.device` (checked at evaluation).
  - **Created by `strata-host incus-setup`** (root) as Incus pools, only on
    devices that are verifiably empty: a whole disk, not behind `/`, `/boot`
    or `/nix`, not mounted, swap, or held by device-mapper or md, with no
    partition table, partition or signature in the udev database. Without a
    udev database it refuses. `source.wipe` is never set. **Strata has no
    erase option**: an administrator wipes a disk deliberately.
  - **Adoption** (`adopt = true`): an existing, empty zpool or volume group
    named like the pool, whose disks carry only its signatures (checked via
    udev labels for ZFS and `pvs` for LVM). This covers layouts Strata does
    not model (raidz). Recovering pools with instances is `incus admin
    recover`, not adoption.
  - **Never modified or deleted:** an existing pool whose driver or original
    source differs from its declaration is reported and left alone. A pool
    removed from the configuration keeps its data and instances. It is
    closed to the project (`limits.disk.pool.<name>=0`, and left out of
    the allowed pools) once no instance or profile of the project uses it;
    until then it stays open with a warning, because Incus refuses to close
    a pool under existing instances and Strata never deletes them. Cached
    images do not count. Other undeclared pools are closed the same way.
  - **Project access and allocation:** `restricted.storage-pools.access`
    lists exactly the declared pools (a reserved sentinel name when there
    are none, because Incus treats an empty list as "all"). Per-pool limits
    (`limits.disk.pool.<name>`, applied once the pool exists, as Incus
    requires) and project limits (`limits.cpu`, `limits.memory`,
    `limits.disk`, `limits.instances`) come from options. The default
    profile's root disk is put on `strata.storage.defaultPool`, with
    `strata.storage.rootSize`: Incus counts disk limits by the sizes given
    to instances, so instances on a limited pool need one (required at
    evaluation when the default pool or the project is limited).
  - **Refusals are per pool:** one refused pool does not stop the project,
    identity or other pools; the service exits non-zero and the journal
    names the device and what was found.
  - **ZFS** is enabled only when a zfs pool is declared, with an
    evaluation-time check that the kernel's ZFS module is available
    (CLAUDE.md §6) and `networking.hostId` derived from the node ID.
  - **LVM autoactivation:** `activation/auto_activation_volume_list` lists
    only declared lvm pools' volume groups, and is empty in the initrd's own
    `lvm.conf` (the VM test showed the initrd activating every volume group,
    and failing on thin pools, before the real system starts). It is the
    default where Strata owns the disk layout (`strata.disk`, no LVM);
    elsewhere the NixOS default (all) stays, because a hand-made host may
    need its own volume groups at boot.
  - **Kernel modules:** an lvm pool loads `dm_thin_pool` and `dm_snapshot`
    at boot; Incus needs them and nothing loads them on demand.
  - **Snapshots allowed, backup export blocked** (until M7).
- **Verified by:** `internal/hostsetup` and `internal/blockdev` unit tests
  (refusal table, adoption, existing pools, sentinel, trust correction;
  mutation-checked), `module-eval`, and the `nixos-storage` VM test (ZFS
  mirror, LVM-thin and dir pools used by the agent's identity; refusal of a
  disk with a filesystem, the system disk and a changed driver; adoption of
  a hand-made zpool; removal keeping data; a foreign volume group staying
  inactive across a reboot; pools and containers surviving a reboot).
- **Consequences and limitations:**
  - Growing pools (adding disks, replacing a mirror disk) is done with
    `zpool`/`lvm` by hand; Strata does not track it and reports a source
    mismatch only if the original source changes.
  - Pool health (degraded mirrors, full thin pools) is not monitored yet
    (M5 monitoring).
  - ZFS on a host means the ZFS kernel module must exist for its kernel;
    nixpkgs kernel updates can lag ZFS releases.

### ADR-0019: Guest networks bridged to uplinks, without a host address

- **Status:** Accepted (M2 slice 3). Settles the guest networking question
  of ADR-0015.
- **Context:** CLAUDE.md §15 requires the management network to be isolated
  from guest networking by default and forbids disruptive host network
  changes without recovery. The restricted Strata project allows NICs only
  on Incus-managed networks (`restricted.devices.nic=managed`).
- **Decision:**
  - **Declared in NixOS** (`strata.networking.guest.networks`), created by
    `strata-host incus-setup` as Incus `bridge` networks with
    `ipv4.address=none` and `ipv6.address=none`: no NAT, DHCP or DNS from
    the host, and **no host address**. A network is bridged to an uplink NIC
    (untagged), to a VLAN on it (`bridge.external_interfaces =
    <uplink>.<vlan>/<uplink>/<vlan>`, created by Incus), or isolated.
  - **The management interface is never an uplink** (evaluation and runtime).
    An uplink is either untagged for one network or a VLAN trunk, never
    both (a bridge port stops delivering frames to VLAN interfaces on it).
    At runtime an uplink must exist and have no IP address.
  - **Host side:** networkd brings uplinks up with no address of any kind
    and `KeepMaster`; the VLAN interfaces Incus creates get the same (the
    VM test found the kernel giving them IPv6 link-local addresses
    otherwise). Guest bridges are exempt from the reverse-path filter.
  - **Configuration is enforced** (unlike pools): a changed declaration
    updates the network (PATCH), logged with old and new values. A
    network holds no data; the change affects only guests, never the
    host's management connectivity.
  - **Never deleted:** an undeclared network stays open to the project
    while its instances or profiles use it (Incus refuses otherwise), then
    is closed (left out of `restricted.networks.access`; a reserved
    sentinel when none is declared, since empty means "all").
  - **Default profile:** `eth0` on `strata.networking.guest.defaultNetwork`.
  - **Guests choose networks, not VLAN tags.** The restricted project
    cannot forbid a NIC's `vlan` key; the VM test proves that a guest
    tagging VLAN 20 on an untagged network does not reach VLAN 20 on the
    trunk (bridge VLAN filtering), with a positive control.
- **Deferred:** NAT or routed guest networks (host address, DHCP/DNS and
  forward rules that keep guests away from the management network), bonds,
  and VLAN or bonded management interfaces.
- **Verified by:** `internal/hostsetup` unit tests (creation, enforcement,
  refusals, undeclared networks, sentinel; mutation-checked),
  `module-eval`, and the `nixos-networks` VM test (untagged, VLAN and
  isolated networks with a peer; no host address on any guest-facing
  interface; no path from guests to the host; no VLAN hopping; refusal of
  undeclared networks, macvlan on the management NIC and network creation;
  a declared change moving a network; a removed network staying while used;
  networkd reload; reboot).

### ADR-0020: The agent's local API

- **Status:** Accepted (M2 slice 4a).
- **Context:** M2's acceptance is managing VMs through the CLI. The
  management API (M5) does not exist yet, and privileged operations must
  pass through the agent (CLAUDE.md §11, §17).
- **Decision:**
  - The agent serves versioned HTTP+JSON on a Unix socket,
    `/run/strata/agent.sock`, with paths shaped like the planned API
    (`/api/v1/vms`, `/api/v1/vms/{name}/{start,stop,restart}`,
    `/api/v1/operations/{id}`), so the M5 API server can take its place.
  - **systemd owns the socket** (`strata-agent.socket`: root,
    group `strata-operators`, mode 0660) and passes it to the agent, which
    gains no privilege. The admin account is in `strata-operators`.
  - **Authorization:** the agent reads the caller's uid (`SO_PEERCRED`) and
    allows root and members of `strata-operators` only, independently of
    the socket's permissions.
  - **Audit:** every request, including refusals, is logged with the
    caller's uid, method, path, status and operation ID (JSON, journal).
  - **Operations:** changes return `202` with an operation (ID, type,
    resource, status `PENDING` → `APPLYING` → `SUCCEEDED`/`FAILED`, caller
    uid). One operation per VM at a time; operations are kept in memory
    (the last 1000) until M3/M5 persist them.
  - Request bodies are limited to 64 KiB, requests and operations have
    timeouts, and the server shuts down gracefully.
- **Consequences:** VM management is local to the host and limited to
  members of a group; the group is as trusted as the restricted Incus
  project allows (no host access through it). Remote access, TLS, accounts
  and roles come with M5.

### ADR-0021: VM schema and lifecycle

- **Status:** Accepted (M2 slices 4a and 4b).
- **Decision:**
  - **Schema `vm.strata.dev/v1alpha1`** (JSON; ADR-0004): name, CPU,
    memory, an image alias (local, or on an allowed `imageServer`) or none,
    disks (the first is `root`; others
    are data volumes `<vm>-<disk>`), NICs on guest networks, autostart and
    cloud-init SSH keys. Unknown fields are rejected; everything is
    validated before an operation starts. No backend keys appear in it.
  - **The spec alone defines the VM:** the Incus instance has no profiles,
    and the spec is stored on it (`user.strata.spec`); only instances marked
    `user.strata.managed` are visible to Strata.
  - **Verification:** after `create`, configuration and devices are read
    back and compared with the spec; after an action, the status is
    checked. Only then is the operation `SUCCEEDED`.
  - **Data volumes** are created before the instance. On failure, the
    volumes this request created are removed; an existing volume is never
    reused or removed.
  - **Images:** local images in the project; downloads only from servers
    listed in `strata.compute.incus.imageServers` (default none), enforced
    by Incus through `restricted.images.servers` (simplestreams over
    HTTPS).
  - **Changes (`update`) are planned and confirmed by plan ID.** The agent
    compares the desired spec with the VM's spec, which must match what
    Incus has (a VM changed with `incus` directly is refused). The plan ID
    is a hash of the current spec, the desired spec and whether the VM
    runs; applying requires it, so what is applied is what was confirmed,
    and a VM that changed in between is refused (`plan again`).
  - **Changes to a running VM restart it** (graceful stop, change, start,
    verify), except `autostart`. Live changes (hotplug) are not used: they
    depend on the guest and are less predictable. If a change fails after
    the stop, new volumes are removed and the VM is started again unchanged.
  - **Allowed:** CPU, memory, growing disks, adding data disks (new
    volumes only), detaching data disks (the volume is kept), NICs,
    autostart, cloud-init keys (first boot only). **Refused:** shrinking a
    disk, moving it to another pool, another image, renaming.
  - **Delete** needs the VM's name as confirmation (checked by the agent)
    and `--stop` for a running VM. The VM and its root disk are deleted;
    **data volumes are never deleted with it**: the agent checks they still
    exist and returns them. Deleting volumes belongs to M4 volume
    management.
- **Verified by:** `internal/workload`, `internal/incus`,
  `internal/agent/api` and `internal/vmcli` unit tests, `module-eval`, and
  the `nixos-vms` VM test (a NixOS guest created from a spec, booted,
  reaching its network and its data disk; stop, restart, forced stop; an
  empty VM; refusals; image servers; an update declined, then applied with
  a restart and seen by the guest (CPU, memory, a new disk); refused
  changes; a stale plan; detaching keeping the volume; audit; autostart
  after a host reboot; delete with confirmation keeping data volumes).

### ADR-0022: Dependency updates and code scanning

- **Context:** Strata pins everything through `flake.lock`, including the
  Go toolchain (nixpkgs' `go`; `GOTOOLCHAIN=local`), so Go standard library
  fixes only arrive with a nixpkgs update. Go has no third-party modules yet
  (ADR-0004). A nixpkgs update also moves the kernel, Incus, ZFS and QEMU,
  which only the NixOS VM tests exercise, and CI does not run them
  (ADR-0016). The repository is public.
- **Decision:**
  - **Dependabot** (`.github/dependabot.yml`) opens version updates for
    GitHub Actions (weekly, minor and patch grouped), Nix flake inputs
    (monthly, all inputs in one pull request, at most one open) and Go
    modules (weekly; no modules yet). A `flake.lock` pull request is a
    module change for ADR-0016: run `make vm-test` before merging it.
  - **CodeQL** (`.github/workflows/codeql.yml`) analyses Go and the
    workflows themselves with the `security-extended` suite on pushes to
    `main`, pull requests and weekly. Go is built (autobuild) with the
    toolchain from `flake.lock`, not a separately installed release.
  - **govulncheck** checks the code against the Go vulnerability database
    with the pinned toolchain. CI (`make vulncheck`) fails on any
    vulnerability in called code unless `.govulncheck-allow` lists it with
    a reason and an expiry date (`tools/vulngate`); expired entries fail
    again and entries no longer reported are flagged for removal. A weekly
    workflow uploads the findings to code scanning, so new advisories
    appear without a commit. The usual fix is the pending `flake.lock`
    update; when nixpkgs has no fix yet, an allow-list entry with an
    assessment of the impact is the documented exception.
  - Workflows pin actions to commit SHAs (Dependabot updates them), check
    out without persisted credentials, and grant per-job permissions.
    `actionlint` (with shellcheck) checks them in `make lint` and in the
    `actions-lint` flake check.
- **Consequences:** a new Go advisory can fail CI for pull requests that did
  not change Go code; that is intended, and the allow-list keeps the
  exception visible and time-limited. Not covered: CVE scanning of the
  host closure (vulnix; too noisy to act on while tracking
  nixos-unstable), container images (none yet) and the frontend
  dependencies (added with `web/` in M5). Repository settings that the
  configuration cannot express (Dependabot alerts, secret scanning with
  push protection, private vulnerability reporting, CodeQL default setup
  left off) are listed in `docs/development/README.md`.

### ADR-0023: The intermediate representation

- **Context:** the Go backend must not interpret Nix (CLAUDE.md §7.2), and
  workloads are declared in the host's NixOS configuration (M3 decision 1).
  The controller needs a stable, versioned description of the desired
  workloads that it can validate on its own and plan from.
- **Decision:**
  - VMs are declared as `strata.workloads.vms.<name>`, whose fields map one
    to one onto `vm.strata.dev/v1alpha1`. Disks and interfaces are
    attribute sets keyed by name; in the spec `root` comes first, then the
    other disks and the interfaces in name order. Unset disk pools resolve
    to `strata.storage.defaultPool`.
  - `config.strata.ir` is the IR, `ir.strata.dev/v1alpha1`: the generator
    (Strata version), the node, the host context (Incus project, pools
    with drivers, default pool, guest networks, allowed image servers,
    project limits) and the resources. A resource has a stable ID
    (`TYPE/NAME`), a type (`vm`), its dependencies (exactly the host
    objects its spec uses, sorted), a lifecycle policy (`protected`) and
    its spec in the workload's own schema, so the VM schema is not
    duplicated.
  - The IR holds desired state only. Its source (repository, commit,
    flake.lock) is recorded by the controller around it, because pure
    evaluation of a NixOS configuration cannot reliably see it.
  - The output is deterministic: sorted keys and lists, no timestamps.
    The same configuration always gives the same bytes, so plan IDs
    derived from it are stable.
  - Nix checks structure and references to the declared host objects as
    assertions, which also fail `nixos-rebuild`, and evaluating the IR
    fails if any assertion of the configuration fails. Go
    (`internal/ir`, `strata-controller validate-ir`) validates everything
    again, rejects unknown fields and versions, and is the authority on
    the VM schema; detailed formats (SSH keys, image aliases) are checked
    in Go only.
  - Dependencies are only on host objects in M3, so there is no ordering
    to resolve; a topological order arrives with dependencies between
    resources (M4).
- **Consequences:** the `examples` check ties the two sides together: it
  evaluates `examples/standalone/host.nix`, validates the IR with
  `strata-controller validate-ir` and requires it to equal
  `internal/ir/testdata/standalone.json`, which the Go tests parse. An
  invalid workload blocks host rebuilds until it is fixed. The IR is per
  node; a cluster layer (M6) can produce it. Supersedes ADR-0005.
