# Strata Architecture

**Status:** Milestone M1 in progress (slice 1 complete). This document describes the target
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
| `internal/version` | Build metadata injected via `-ldflags` |
| `internal/cli` | Shared, testable entrypoint and subcommand dispatch |
| `internal/hardware` | Hardware discovery from procfs/sysfs (no exec) |
| `internal/agent` | Agent run loop, atomic report writing, agent subcommands |
| `nix/packages/` | Package definitions (`strata`) |
| `nix/modules/` | NixOS modules: `base` (identity), `agent` (service) |
| `nix/tests/` | Nix evaluation tests and NixOS VM tests (`nixos/`) |
| `examples/standalone/` | Non-normative sample infrastructure configuration |
| `docs/` | Development and administration documentation |
| `.github/workflows/` | CI |

Planned (not yet created): `installer/` (M1), more `nix/modules/*` (M1),
`internal/{compute,storage,networking,...}` (M2+), `internal/{gitops,schema,
planner,reconciler,executor}` (M3), `api/openapi`, `web/` (M5),
`internal/cluster` (M6).

## Current status

| Capability | Status |
|---|---|
| Nix flake (package, dev shell, formatter, checks) | **Implemented** |
| Go module and four binaries | **Implemented** (only the agent has functional commands) |
| Formatting (gofumpt, goimports, nixfmt) and linting (golangci-lint, statix, deadnix) | **Implemented** |
| Go unit tests, Nix evaluation tests, NixOS VM test | **Implemented** |
| GitHub Actions CI (including VM tests via KVM) | **Implemented** |
| Sample infrastructure configuration | **Example only** — no schema, not consumed by any code |
| NixOS options `strata.{enable,package,node.id,node.role,agent.enable}` | **Implemented** (M1 slice 1, experimental) — see [options](docs/administration/options.md) |
| Hardware discovery (`strata-agent discover`, `/var/lib/strata/hardware.json`) | **Implemented** (M1 slice 1, experimental; schema `v1alpha1`) |
| Agent systemd service (unprivileged, no network) | **Implemented** (M1 slice 1, experimental) |
| Host modules: compute/libvirt, SSH, networking, monitoring | Planned (M1 slice 2) |
| Disk layouts (disko), reference host configuration | Planned (M1 slice 3) |
| Installer ISO and `strata-install` | Planned (M1 slice 4) |
| Physical hardware validation | Planned (M1 slice 5) |
| VM management (libvirt) | Planned (M2) |
| GitOps, IR, planner, reconciler | Planned (M3) |
| LXC, Podman, ZFS, LVM | Planned (M4) |
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
   node exporter on the management network.
3. Reference host configuration and disko layouts (ext4 default; ZFS
   optional and gated on kernel support).
4. Installer ISO and `strata-install` (answer file or interactive prompts;
   explicit confirmation before wiping a disk).
5. Installation guide and validation on physical Intel and AMD hardware.

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

### ADR-0008: Versioned hardware report

- **Decision:** The hardware report is JSON with
  `apiVersion: hardware.strata.dev/v1alpha1`. Discovery reads only procfs and
  sysfs through an `fs.FS` and never executes external commands. Node identity
  is published as `/etc/strata/node.json` with
  `apiVersion: node.strata.dev/v1alpha1`.
- **Consequence:** `v1alpha1` may change incompatibly; consumers must check
  `apiVersion`. Fields needing root (e.g. DMI serial numbers) are omitted.

### ADR-0009: Installation uses disko and a CLI installer

- **Decision:** Disk partitioning uses nix-community/disko with Strata-provided
  layouts rather than a custom partitioner. The M1 installer is a CLI
  (`strata-install`) driven by a versioned answer file or interactive
  prompts. A web installer is deferred until the M5 API and authentication
  exist.
- **Status:** Accepted; implemented in M1 slices 3 and 4.
