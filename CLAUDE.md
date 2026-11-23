# Strata — Claude Code Project Specification

**Version:** 0.2.0 (workload backend revised: Incus, ADR-0015)  
**Status:** Initial development specification  
**Project:** Strata  
**Platform:** NixOS  
**Primary language:** Nix and Go  
**Frontend:** React and TypeScript

---

# 1. Instructions to Claude Code

You are acting as a senior systems architect, Linux virtualization engineer, NixOS specialist, and Go software engineer.

Your responsibility is to help design, implement, test, and maintain Strata.

Strata is a declarative, bare-metal virtualization platform built on NixOS. Its intended capabilities are comparable to Proxmox VE, but its defining architectural principle is that infrastructure configuration is declarative, reproducible, and managed through Git.

You must prioritize:

1. Correctness over implementation speed.
2. Reproducibility over convenience.
3. Explicit architecture over implicit behavior.
4. Security over unrestricted functionality.
5. Small, testable changes over large rewrites.
6. Documented decisions over assumptions.

Do not attempt to implement the entire platform in one operation.

Always inspect the existing repository before making changes.

Before implementing a substantial feature:
- Identify relevant existing modules.
- Review the project's architecture documentation.
- Identify affected interfaces.
- Determine the necessary tests.
- Explain significant architectural changes.

Do not silently change established architectural decisions.

If an implementation requirement conflicts with this specification, explain the conflict and propose a resolution before making a major architectural change.

# 2. Project vision

Strata is a bare-metal virtualization and infrastructure management platform powered by NixOS.

The platform provides:

- Virtual machine management.
- System container management.
- OCI application container management.
- Declarative infrastructure configuration.
- Git-based configuration management.
- Centralized web management.
- Command-line management.
- REST API.
- Storage management.
- Virtual networking.
- Multi-node cluster management.
- Monitoring and auditing.
- Backup and recovery.

The long-term goal is to provide a complete alternative to traditional virtualization platforms, with NixOS and declarative infrastructure as the foundation.

## 2.1 Core principles

All implementation decisions should follow these principles.

### Declarative infrastructure

Infrastructure should be described using Nix configurations.

The system must be capable of comparing desired infrastructure state with actual infrastructure state.

### Git as source of truth

Git repositories are authoritative for desired infrastructure configuration.

PostgreSQL stores runtime inventory, operations, audit information, and other application state.

PostgreSQL must not silently become a second source of desired infrastructure configuration.

### Reproducibility

Use Nix flakes and pinned dependencies.

Host configurations and platform releases must be reproducible.

### Idempotency

Repeatedly applying an unchanged configuration must not create unnecessary infrastructure changes.

### Modularity

Workload, storage, and networking concerns must be separated behind interfaces. The workload backend (Incus) is accessed only through its provider package, so that it can be replaced by a mock in tests and is not referenced elsewhere.

### Separation of concerns

The management plane must be independent of workload execution.

Running workloads must continue operating when the management API or dashboard is unavailable.

### Data persistence

VM disks, container volumes, and persistent application data must never be treated as disposable Nix build artifacts.

# 3. Technology stack

Use the following technology choices unless an architectural decision is explicitly revised.

| Component | Technology |
|---|---|
| Operating system | NixOS |
| Configuration | Nix flakes |
| Hypervisor | KVM |
| VM runtime | QEMU |
| Workload management | Incus (LTS release) |
| System containers | LXC, managed by Incus |
| OCI containers | Incus OCI application containers |
| Backend | Go |
| Frontend | React |
| Frontend language | TypeScript |
| API | REST and WebSockets |
| Database | PostgreSQL |
| Cluster coordination | Decided in M6 (Incus clustering is the likely candidate) |
| Local storage | ZFS and LVM-thin, as Incus storage pools on dedicated disks |
| Distributed storage | Ceph |
| Networking | Linux bridges, VLANs, nftables |
| Secrets | SOPS and age |
| Monitoring | Prometheus |
| Dashboards | Grafana |
| Logging | Loki |

Do not introduce Kubernetes, OpenStack, or additional orchestration platforms unless explicitly justified.

Do not replace Incus with a custom QEMU or LXC management implementation, and do not reimplement instance lifecycle management that Incus provides. Do not manage workloads through libvirt or a separate Podman runtime.

The decision and its rationale are recorded in ARCHITECTURE.md, ADR-0015.

# 4. Architecture

The platform consists of three logical planes.

## 4.1 Management plane

Responsible for user interaction.

Components:

- Web dashboard.
- CLI.
- REST API.
- Authentication.
- Authorization.
- Audit logging.

## 4.2 Control plane

Responsible for infrastructure management.

Components:

- Git synchronization.
- Nix evaluation.
- Configuration validation.
- Desired-state management.
- Execution planning.
- Reconciliation.
- Scheduling.
- Cluster coordination.

## 4.3 Infrastructure plane

Responsible for actual workload execution.

Components:

- NixOS host.
- KVM/QEMU and LXC, managed by Incus.
- Storage pools (Incus).
- Networking providers.
- Node agents.

The infrastructure plane must not depend on the management dashboard to maintain running workloads.

# 5. Repository structure

Maintain the following modular structure.

```text
Strata/
├── flake.nix
├── flake.lock
├── go.mod
├── go.sum
├── Makefile
├── README.md
├── CLAUDE.md
├── ARCHITECTURE.md
│
├── installer/
│   ├── iso/
│   ├── backend/
│   ├── frontend/
│   └── partitioning/
│
├── nix/
│   ├── modules/
│   │   ├── base/
│   │   ├── node/
│   │   ├── compute/
│   │   ├── containers/
│   │   ├── storage/
│   │   ├── networking/
│   │   ├── cluster/
│   │   └── monitoring/
│   ├── packages/
│   ├── overlays/
│   └── tests/
│
├── cmd/
│   ├── Strata/
│   ├── Strata-api/
│   ├── Strata-agent/
│   └── Strata-controller/
│
├── internal/
│   ├── api/
│   ├── auth/
│   ├── config/
│   ├── schema/
│   ├── gitops/
│   ├── planner/
│   ├── reconciler/
│   ├── executor/
│   ├── inventory/
│   ├── workload/
│   ├── incus/
│   ├── storage/
│   ├── networking/
│   ├── cluster/
│   ├── scheduler/
│   ├── monitoring/
│   └── audit/
│
├── api/
│   ├── openapi/
│   └── proto/
│
├── web/
│   └── src/
│
├── docs/
│   ├── architecture/
│   ├── installation/
│   ├── administration/
│   ├── development/
│   └── api/
│
└── tests/
    ├── unit/
    ├── integration/
    ├── nixos/
    └── end-to-end/
```

Directories may be introduced incrementally. Do not create large numbers of empty packages without an implementation requirement.

# 6. NixOS requirements

NixOS is the underlying operating system.

The platform must use NixOS modules to manage host configuration.

The initial host implementation must support:

- Host identity.
- Hardware configuration.
- KVM.
- Incus (QEMU and LXC).
- SSH.
- Host monitoring.
- Network configuration.
- Storage configuration.
- Strata agent.

Create a dedicated NixOS module namespace:

```nix
Strata = {
  enable = true;

  node = {
    id = "hv-01";
    role = "hypervisor";
  };

  compute.enable = true;
  storage.enable = true;
  networking.enable = true;
  agent.enable = true;
};
```

This is a proposed interface.

Do not assume that proposed Strata options already exist.

Implement and test every option before documenting it as supported.

Use existing NixOS modules wherever practical.

Do not duplicate functionality already provided by NixOS.

Use `nix flake check` and relevant NixOS tests to validate configuration changes.

Avoid hardcoding architecture-specific kernel modules.

Detect CPU virtualization capabilities and support both Intel and AMD.

Do not assume that ZFS is available with every kernel configuration.

# 7. Declarative configuration system

Strata configuration is expressed using Nix.

The initial configuration model must support:

- Nodes.
- Clusters.
- Virtual machines.
- System containers.
- OCI containers.
- Storage pools.
- Volumes.
- Networks.
- Network interfaces.
- Resource limits.
- Lifecycle policies.

Example proposed workload configuration:

```nix
{
  workloads.web-01 = {
    type = "vm";

    resources = {
      cpu = 4;
      memory = "8GiB";
    };

    disks = [
      {
        name = "system";
        size = "80GiB";
        storage = "fast-pool";
      }
    ];

    networking = [
      {
        bridge = "vmbr0";
        vlan = 20;
      }
    ];
  };
}
```

This example defines the desired project schema, not an existing NixOS option.

## 7.1 Configuration processing

Implement the following pipeline:

1. Retrieve configuration from Git.
2. Resolve the pinned revision.
3. Evaluate Nix configuration.
4. Convert evaluated output into an intermediate representation.
5. Validate the intermediate representation.
6. Resolve dependencies.
7. Compare desired and actual state.
8. Generate an execution plan.
9. Request approval where required.
10. Execute operations.
11. Verify resulting state.
12. Record deployment information.

## 7.2 Intermediate representation

The Go backend must not attempt to understand arbitrary Nix language expressions.

Nix evaluation should produce a well-defined intermediate representation.

The IR should have:

- Explicit schema version.
- Stable resource identifiers.
- Resource types.
- Dependencies.
- Desired properties.
- Lifecycle policies.
- Backend capabilities.
- Validation metadata.

Prefer JSON as the initial serialization format.

Version the IR from the beginning.

# 8. GitOps architecture

Git is the authoritative source of desired infrastructure state.

Support:

- HTTPS repositories.
- SSH repositories.
- Private repositories.
- Branch tracking.
- Commit pinning.
- Webhook-triggered synchronization.
- Periodic synchronization.
- Configuration validation.
- Deployment history.
- Rollback to a previous configuration revision.

The first implementation should use read-only Git synchronization.

Strata must not automatically modify infrastructure repositories.

Every deployment must record:

- Repository identity.
- Commit SHA.
- Flake lock revision.
- Strata version.
- Node identity.
- Deployment timestamp.
- Result.

Do not apply untrusted Git configurations automatically.

Nix evaluation must use an appropriately restricted execution environment and an explicit repository trust policy.

# 9. Reconciliation engine

The reconciliation engine is one of the most important platform components.

It compares:

- Desired state.
- Observed state.

It produces a plan describing the differences.

The planner must identify:

- Resources to create.
- Resources to update.
- Resources to delete.
- Resources that cannot be reconciled.
- Dependencies.
- Potentially disruptive changes.
- Destructive operations.

The executor applies approved plans.

Operations must be idempotent wherever possible.

Every operation must have:

- Unique ID.
- Resource ID.
- Operation type.
- Execution status.
- Start time.
- Completion time.
- Error details.
- Audit record.

Use explicit operation states:

```text
PENDING
VALIDATING
PLANNING
AWAITING_APPROVAL
APPLYING
VERIFYING
SUCCEEDED
FAILED
CANCELLED
```

Never report success until the resulting infrastructure state has been verified.

Destructive operations require explicit approval.

# 10. Workload providers

Incus is the single workload backend (ARCHITECTURE.md, ADR-0015). Strata is the declarative layer: it plans changes and applies them through the Incus API. Incus owns instance lifecycles.

Strata supports three workload types, all executed by Incus:

## 10.1 Virtual machines

Backend: KVM/QEMU, managed by Incus.

Implement:

- VM creation.
- VM modification.
- VM deletion.
- Start.
- Stop.
- Restart.
- Inspection.
- Resource configuration.
- Disk management.
- Network interface management.
- Autostart.

Do not expose raw Incus or QEMU configuration as the primary public configuration interface.

Maintain an independent Strata VM schema.

## 10.2 System containers

Backend: LXC, managed by Incus.

Implement:

- Container creation.
- Container lifecycle.
- Resource limits.
- Persistent storage.
- Network configuration.
- Container inspection.

Unprivileged containers must be the default.

Do not silently enable privileged container execution. Strata's Incus projects forbid privileged containers.

## 10.3 OCI containers

Backend: Incus OCI application containers.

Implement:

- Image management.
- Container lifecycle.
- Environment configuration.
- Resource limits.
- Persistent volumes.
- Network configuration.
- Restart policies, where Incus supports them.

Do not assume Docker or Podman behavior. Podman features without an Incus equivalent (for example health checks and pods) are documented as unsupported, not reimplemented.

## 10.4 Backend access

The node agent accesses Incus only through a TLS client identity that Incus restricts to Strata's projects. Those projects are restricted (no privileged containers, no host device or path passthrough, no raw configuration) and have resource limits.

Never give a Strata component access to the Incus Unix socket or an unrestricted Incus identity.

Storage pools, networks, projects, and project restrictions are server-level Incus configuration. They are declared through NixOS, not created by the agent.

Expose backend capabilities and limitations through the provider interface.

# 11. Go backend standards

Use Go for backend services.

Use a modular architecture with clear package boundaries.

Avoid unnecessary microservices in the initial release.

The initial executable components are:

- Strata
- Strata-api
- Strata-agent
- Strata-controller

Use interfaces to separate infrastructure providers from management logic.

Provider implementations must be independently testable.

Follow these standards:

- Use context.Context for cancellable operations.
- Return errors rather than hiding failures.
- Wrap errors with useful context.
- Avoid global mutable state.
- Use structured logging.
- Validate all external input.
- Use explicit timeouts for external operations.
- Implement graceful shutdown.
- Avoid shell execution where a suitable library or API exists.
- Never interpolate untrusted input into shell commands.

Do not run privileged infrastructure commands directly from HTTP handlers.

All privileged operations must pass through the appropriate authorized execution layer.

# 12. API standards

Use a versioned REST API.

Initial namespace:

```text
/api/v1/
```

Initial resources:

```text
/nodes
/clusters
/vms
/containers
/oci
/storage
/networks
/config
/deployments
/operations
```

Use OpenAPI to document the API.

Use WebSockets or server-sent events for live status updates.

Long-running operations must return an operation ID.

The CLI and dashboard must use the same API and authorization model.

Do not duplicate business logic in the frontend.

# 13. Frontend standards

Use React and TypeScript.

The dashboard should provide:

- Cluster overview.
- Node overview.
- Hardware information.
- VM management.
- System container management.
- OCI container management.
- Storage management.
- Networking.
- Configuration revisions.
- Deployment plans.
- Operation history.
- Monitoring.
- Audit logs.

Use reusable components.

Keep API communication in dedicated service modules.

Never embed infrastructure execution logic in the frontend.

Show administrators the planned impact of configuration changes before applying them.

# 14. Storage architecture

Initial backends, as Incus storage pools:

- ZFS.
- LVM-thin.
- Directory-backed storage where appropriate.

Future backend:

- Ceph.

Storage pools are declared through NixOS on dedicated disks, with explicit configuration and allocation policies. Workload storage never uses the system disk.

Volumes are managed through Incus by the Incus provider.

Separate storage configuration from persistent data.

Important rules:

- Never delete volumes implicitly.
- Never assume a configuration removal means data deletion is authorized.
- Validate available capacity before provisioning.
- Expose snapshot capabilities accurately.
- Test backup restoration.
- Make storage operations auditable.

Do not claim that ZFS snapshots are equivalent to application-consistent backups.

# 15. Networking architecture

Support:

- Linux bridges.
- VLANs.
- Bonding.
- Virtual interfaces.
- Static addressing.
- DHCP integration.
- nftables.
- Management network isolation.

Host networking must be managed through NixOS.

Workload networks are Incus networks declared through NixOS. Workloads attach only to networks their project is allowed to use. No workload network may reach the management interface.

Do not apply disruptive network changes without a recovery mechanism.

The management network must be isolated from guest networking by default.

# 16. Cluster architecture

Cluster support is a later development phase.

Use:

- Node agents.
- Central controller.
- PostgreSQL.
- Git for desired configuration.

Cluster coordination is designed in M6. Incus clustering is the likely candidate for workload placement and migration. Do not introduce etcd or Incus clustering before M6.

Do not implement automatic failover until quorum, fencing, workload ownership, and storage consistency have been designed and tested.

Do not assume that distributed configuration automatically provides distributed storage.

# 17. Security requirements

Security is mandatory.

Implement:

- TLS.
- Authentication.
- Role-based access control.
- Least-privilege service accounts.
- SSH key authentication.
- Secrets management.
- Audit logging.
- Management network isolation.
- Input validation.
- Secure API communication.
- Restricted Nix evaluation.
- Explicit approval of destructive operations.

Never:

- Commit plaintext secrets.
- Place secrets directly into ordinary Nix configuration values.
- Expose privileged sockets publicly.
- Give any Strata component access to the Incus Unix socket or an unrestricted Incus identity.
- Expose the Incus API on any network interface.
- Give the API server unrestricted root access.
- Execute untrusted shell commands.
- Automatically trust arbitrary Git repositories.

Use SOPS and age for encrypted configuration secrets.

Use a dedicated node agent for local infrastructure operations, with only the privileges it needs (a restricted Incus identity for workloads).

# 18. Testing requirements

All significant features must have tests.

Testing layers:

1. Go unit tests.
2. Nix evaluation tests.
3. Provider integration tests.
4. NixOS VM tests.
5. Physical hardware tests.
6. API integration tests.
7. Frontend tests.
8. End-to-end tests.

Required commands should be documented in the Makefile.

At minimum, establish:

```bash
go test ./...
nix flake check
```

Add formatter and linter checks.

Use test fixtures and mock providers for unit tests.

Never require production hardware for ordinary unit tests.

Test failure paths, not only successful operations.

# 19. Development roadmap

Implement the project incrementally.

## M0 — Foundation

- Repository initialization.
- Nix flake.
- Go module.
- CI.
- Development environment.
- Architecture documentation.

Acceptance: Repository builds and automated checks pass.

## M1 — Bare-metal platform

- Custom NixOS ISO.
- Hardware discovery.
- Installation.
- Host configuration.
- Agent installation.

Acceptance: Install and boot Strata on physical hardware.

## M2 — VM management

- Incus on the host, replacing libvirtd.
- Restricted Strata project and agent identity, proven by tests.
- Incus provider.
- VM schema.
- VM lifecycle.
- Local storage (Incus storage pools on dedicated disks).
- Virtual networking (Incus networks declared through NixOS).

Acceptance: Manage VMs through the CLI.

## M3 — Declarative engine

- Git integration.
- Nix evaluation.
- Intermediate representation.
- Validation.
- Planning.
- Reconciliation.
- Drift detection.

Acceptance: Git changes generate safe and repeatable infrastructure changes.

## M4 — Containers and storage

- System containers (Incus).
- OCI application containers (Incus).
- ZFS.
- LVM-thin.
- Volume management.

Acceptance: Manage VMs and both container types declaratively.

## M5 — Web interface

- REST API.
- Dashboard.
- Authentication.
- Authorization.
- Monitoring.
- Audit logs.

Acceptance: Manage a standalone hypervisor through both interfaces.

## M6 — Cluster management

- Node registration.
- Remote operations.
- Cluster inventory.
- Cluster coordination (designed in M6).
- Scheduling.
- Migration.

Acceptance: Manage multiple hypervisor nodes.

## M7 — Production capabilities

- Ceph.
- Backup and restore.
- High availability.
- Fencing.
- Failure recovery.
- Security hardening.

Acceptance: Demonstrate tested recovery from node and storage failures.

# 20. Claude Code working methodology

Follow this development methodology for every task.

## Step 1 — Inspect

Inspect the repository and identify relevant files.

Do not assume the repository matches the specification.

## Step 2 — Plan

Describe:

- What will be implemented.
- Which files will change.
- Which interfaces are affected.
- Which tests will be added.
- Any architectural risks.

For small, obvious changes, keep planning concise.

## Step 3 — Implement

Make the smallest complete implementation.

Avoid unrelated refactoring.

Follow existing naming and package conventions.

## Step 4 — Validate

Run relevant:

- Go tests.
- Nix checks.
- Formatting.
- Linting.
- Integration tests.

Never claim a test passed unless it was actually executed successfully.

## Step 5 — Document

Update documentation when interfaces, configuration options, or architecture change.

## Step 6 — Report

Summarize:

- What changed.
- Files created or modified.
- Tests executed.
- Known limitations.
- Suggested next task.

# 21. Rules against premature complexity

Do not:

- Implement clustering before standalone operation works.
- Introduce Ceph before local storage is reliable.
- Implement HA before fencing and quorum are understood.
- Introduce microservices without a concrete operational requirement.
- Create abstractions without at least one real use case.
- Implement custom replacements for established Linux virtualization components without justification.
- Build the entire web dashboard before the backend interfaces stabilize.

Prefer working vertical slices.

For example:

Git configuration → Nix evaluation → VM planning → Incus execution → state verification.

A complete working path is more valuable than ten unfinished modules.

# 22. Definition of done

A feature is complete only when:

1. Its implementation exists.
2. Its interfaces are documented.
3. Its errors are handled.
4. Its security implications are considered.
5. Its tests pass.
6. Its documentation is updated.
7. It does not break existing functionality.

Do not mark planned functionality as implemented.

Clearly distinguish experimental, unsupported, and production-ready capabilities.

# 23. Initial task

When beginning work in a new Strata repository:

First inspect the repository.

Then implement M0 only.

Your first deliverable should include:

1. A working Nix flake.
2. A minimal Go project structure.
3. A development shell.
4. Basic CI configuration.
5. Initial formatting and linting.
6. A documented Makefile.
7. ARCHITECTURE.md.
8. A sample infrastructure configuration.
9. A basic automated test suite.

Do not implement the installer, VM engine, cluster manager, or web dashboard during M0.

Before finishing, run the available validation commands and report their actual results.

The goal is a clean, reproducible foundation for the subsequent Strata implementation.

---

**End of specification.**

Strata is intended to become a robust, declarative, NixOS-native virtualization platform. Every implementation decision should contribute to that goal.
