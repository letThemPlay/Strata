# Development

## Environment

All tooling comes from the flake's development shell; nothing needs to be
installed on the host besides Nix (flakes enabled).

```sh
nix develop
```

The shell provides: `go`, `gopls`, `gotools`, `gofumpt`, `golangci-lint`,
`make`, `git`, `nixfmt`, `statix`, `deadnix`. It sets `GOTOOLCHAIN=local` so
Go never downloads a different toolchain.

Optional: with direnv, create an untracked `.envrc` containing `use flake`.

## Common tasks

| Command | What it does |
|---|---|
| `make test` | `go test ./...` |
| `make test-race` | Tests with the race detector |
| `make fmt` | Format Go (gofumpt) and Nix (`nix fmt`, nixfmt) |
| `make fmt-check` | Fail if anything is unformatted |
| `make lint` | golangci-lint, statix, deadnix |
| `make build` | Build binaries into `./bin` with version metadata |
| `make nix-check` | `nix flake check`: every check, including the VM tests |
| `make nix-check-novm` | Evaluates every check and builds all but the VM tests (what CI runs) |
| `make vm-test` | Runs only the NixOS VM tests (`checks.<system>.nixos-*`; needs KVM) |
| `make check` | Format, lint, Go tests and `nix flake check` |

## What `nix flake check` runs

| Check | Description |
|---|---|
| `build` | Builds all four binaries and runs `go test` on every package |
| `go-fmt` | gofumpt formatting |
| `go-lint` | golangci-lint with `.golangci.yml` (offline) |
| `nix-fmt` | nixfmt formatting of all `.nix` files |
| `nix-lint` | statix and deadnix |
| `examples` | Evaluates `examples/standalone/strata.nix` and checks its structure |
| `module-eval` | Evaluates NixOS configurations using the Strata modules: option validation, assertions, generated config |
| `nixos-agent` | NixOS VM test: boots a host with the agent and checks identity, hardware report, privileges, reload and shutdown (needs KVM) |
| `nixos-host` | NixOS VM test with three nodes: admin SSH and sudo, refused password/root logins, firewall isolation of the management services, node exporter, Incus without networks, KVM (needs KVM) |
| `nixos-incus` | NixOS VM test of Incus on a compute host: the agent's identity runs a container in the Strata project and is refused privileged and nested containers, host paths and devices, low-level settings, other projects and project, server, trust and pool changes; the API listens on loopback only; `strata-incus-setup` reverts project changes and stale trust; the declared dir pool is the project's default (needs KVM) |
| `nixos-storage` | NixOS VM test of storage pools with seven extra disks: ZFS mirror, LVM-thin and dir pools used by the agent's identity; refusal of a disk with a filesystem, the system disk and a changed driver; adoption of a hand-made zpool; removing a pool keeps its data; a foreign volume group stays inactive across a reboot (needs KVM) |
| `nixos-networks` | NixOS VM test of guest networks with a peer on untagged and VLAN 20/30 segments: guests reach only their own network; the host has no address on any guest-facing interface; a guest tagging VLAN 20 on an untagged network does not reach it (with a positive control); undeclared networks and macvlan on the management NIC are refused; a declared change moves a network; a removed network stays while used; reboot (needs KVM) |
| `reference-host` | Builds the reference host: the `host` template plus `nix/hosts/reference.nix` |
| `nixos-install-ext4` | x86_64 only. Formats an empty disk with the `ext4` layout, installs the reference host and boots it with UEFI firmware, twice (needs KVM) |
| `installer-system` | Builds the installer ISO's NixOS system (the image itself: `nix build .#iso`) |
| `nixos-installer` | x86_64 only. Boots the installer ISO (test variant) under UEFI with no outside network, checks `plan`, a refused confirmation, and `apply`; then boots the installed disk alone and checks services and that `/etc/nixos` reproduces the running system (needs KVM; the slowest check) |

Nix flakes only see files tracked by Git. Run `git add` on new files before
`nix flake check` or `nix build`, otherwise they are silently ignored.

## Conventions

- Go: standard library first; `context.Context` for cancellable operations;
  wrap errors with context; no global mutable state; structured logging via
  `log/slog`. Command entrypoints are thin `main` functions that delegate to
  testable code taking explicit `io.Writer`s.
- Tests: table-driven, cover failure paths, and never depend on production
  hardware or link-time values.
- Nix: formatted with `nixfmt`, linted with `statix` and `deadnix`.

## CI

`.github/workflows/ci.yml` runs `make nix-check-novm`, then format check,
lint, tests and race-detector tests inside the development shell.

**CI does not run the NixOS VM tests** (ADR-0016). It evaluates them, so a
broken test definition still fails CI, but it does not boot them. Run them
locally before you push a change to the Nix modules, the installer, the ISO
or the tests, and say in the pull request that they passed:

```sh
make vm-test       # or: make nix-check (everything)
```

## NixOS VM tests

VM tests live in `nix/tests/nixos/` and run as part of `nix flake check`
and `make vm-test`, locally only. They need KVM (`/dev/kvm` and the `kvm`/`nixos-test` Nix system features).
To debug one interactively:

```sh
nix build .#checks.x86_64-linux.nixos-agent.driverInteractive
./result/bin/nixos-test-driver   # then: start_all(), hv01.shell_interact()
```
