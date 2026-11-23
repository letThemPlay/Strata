# Strata

Strata is a declarative, bare-metal virtualization platform built on NixOS.
Infrastructure is described in Nix, stored in Git, and reconciled onto VMs
and containers managed by Incus (KVM/QEMU, LXC and OCI application containers).

> **Status: milestone M1 complete** (validated on Intel and AMD
> hardware). The repository contains the build, development environment,
> NixOS modules for a hypervisor host (SSH access, management networking,
> monitoring, system disk layout, libvirtd as a host service), a host
> configuration template, an offline installer ISO with an interactive
> installer and a hardware-discovery agent, all tested in VMs. There is no
> VM, container, configuration or management functionality yet; the
> workload backend is Incus (ADR-0015); integration starts in M2.
> See [ARCHITECTURE.md](ARCHITECTURE.md#current-status).

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
- [examples/standalone](examples/standalone/strata.nix) — sample (non-normative)
  infrastructure configuration
- [CLAUDE.md](CLAUDE.md) — full project specification and roadmap
