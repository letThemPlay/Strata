# Strata

Strata is a declarative, bare-metal virtualization platform built on NixOS.
Infrastructure is described in Nix, stored in Git, and reconciled onto KVM/QEMU
(libvirt), LXC and Podman workloads.

> **Status: milestone M1 in progress.** The repository contains the build,
> development environment, a NixOS module skeleton and a hardware-discovery
> agent. No virtualization, configuration or management functionality is
> implemented yet. See [ARCHITECTURE.md](ARCHITECTURE.md#current-status).

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

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) — architecture, status and decision records
- [docs/development](docs/development/README.md) — development workflow
- [docs/administration/options.md](docs/administration/options.md) — implemented NixOS options
- [examples/standalone](examples/standalone/strata.nix) — sample (non-normative)
  infrastructure configuration
- [CLAUDE.md](CLAUDE.md) — full project specification and roadmap
