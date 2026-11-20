# Strata

Strata is a declarative, bare-metal virtualization platform built on NixOS.
Infrastructure is described in Nix, stored in Git, and reconciled onto KVM/QEMU
(libvirt), LXC and Podman workloads.

> **Status: milestone M1 in progress.** The repository contains the build,
> development environment, NixOS modules for a hypervisor host (libvirt,
> SSH access, management networking, monitoring, system disk layout), a
> host configuration template, an offline installer ISO with an
> interactive installer and a hardware-discovery agent. There is no VM, configuration or management
> functionality yet.
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
- [examples/standalone](examples/standalone/strata.nix) — sample (non-normative)
  infrastructure configuration
- [CLAUDE.md](CLAUDE.md) — full project specification and roadmap
