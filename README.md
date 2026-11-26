# Strata

Strata is a declarative, bare-metal virtualization platform built on NixOS.
Infrastructure is described in Nix, stored in Git, and reconciled onto VMs
and containers managed by Incus (KVM/QEMU, LXC and OCI application containers).

> **Status: milestones M1 and M2 complete; M3 complete for its original
> scope** (validated on physical hardware). A Strata host is installed from an offline ISO and runs Incus
> with a restricted project for workloads: storage pools on dedicated disks,
> guest networks bridged to their own NICs, and VMs managed with
> `strata vm` (create, update with a plan, delete with confirmation)
> through the host's agent, or declared in the host configuration and
> applied from a signed Git repository after approval (`strata config`).
> Host lifecycle management (evaluating, building and deploying the host's
> own NixOS configuration; designed, not implemented: M3 slice 5 and M4),
> containers and volumes (M4), the web interface and API (M5) and clusters
> (M6) are not implemented yet. See
> [ARCHITECTURE.md](ARCHITECTURE.md#current-status).

## Quick start

Requirements: Nix with flakes enabled.

```sh
nix develop          # enter the development shell (Go, linters, formatters)
make help            # list developer targets
make test            # go test ./...
make check           # format check, lint, tests, and nix flake check
nix build            # build all binaries into ./result/bin
./result/bin/strata version
```

To start a host configuration (see
[options](docs/administration/options.md)):

```sh
nix flake init -t github:letThemPlay/Strata#host
```

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) — architecture, status and decision records
- [docs/development](docs/development/README.md) — development workflow
- [docs/administration/options.md](docs/administration/options.md) — implemented NixOS options
- [docs/installation](docs/installation/README.md) — installing a host with the installer ISO
- [docs/installation/disk-layout.md](docs/installation/disk-layout.md) — system disk layout
- [docs/installation/hardware-validation.md](docs/installation/hardware-validation.md) — physical hardware validation (M1 acceptance)
- [docs/administration/workloads.md](docs/administration/workloads.md) — declared workloads and the IR (M3, experimental)
- [docs/administration/gitops.md](docs/administration/gitops.md) — the controller: configuration from Git (M3, experimental)
- [docs/administration/host-deployment.md](docs/administration/host-deployment.md) — host configuration: evaluation, drift and boot state (M3, experimental; no deployment yet)
- [docs/architecture/host-lifecycle.md](docs/architecture/host-lifecycle.md) — host lifecycle management (accepted design, not implemented)
- [examples/standalone](examples/standalone/host.nix) — example host with declared workloads
- [CLAUDE.md](CLAUDE.md) — full project specification and roadmap
