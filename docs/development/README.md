# Development

## Environment

All tooling comes from the flake's development shell; nothing needs to be
installed on the host besides Nix (flakes enabled).

```sh
nix develop
```

The shell provides: `go`, `gopls`, `gotools`, `gofumpt`, `golangci-lint`,
`make`, `git`, `nixfmt`, `statix`, `deadnix`, `actionlint`, `shellcheck`,
`govulncheck`, `jq`, `openssh`. It sets `GOTOOLCHAIN=local` so
Go never downloads a different toolchain.

Optional: with direnv, create an untracked `.envrc` containing `use flake`.

## Common tasks

| Command | What it does |
|---|---|
| `make test` | `go test ./...` |
| `make test-race` | Tests with the race detector |
| `make fmt` | Format Go (gofumpt) and Nix (`nix fmt`, nixfmt) |
| `make fmt-check` | Fail if anything is unformatted |
| `make lint` | golangci-lint, statix, deadnix, actionlint (workflows) |
| `make vulncheck` | govulncheck with the pinned Go toolchain; fails on called vulnerabilities not in `.govulncheck-allow` (needs network) |
| `make ir-golden` | Regenerates `internal/ir/testdata/standalone.json`, the IR of `examples/standalone` |
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
| `actions-lint` | actionlint (with shellcheck) on `.github/workflows` |
| `examples` | Evaluates `examples/standalone/host.nix` with the Strata modules, validates its IR with `strata-controller validate-ir` and requires it to equal `internal/ir/testdata/standalone.json` (regenerate with `make ir-golden`) |
| `module-eval` | Evaluates NixOS configurations using the Strata modules: option validation, assertions, generated config |
| `nixos-agent` | NixOS VM test: boots a host with the agent and checks identity, hardware report, privileges, reload and shutdown (needs KVM) |
| `nixos-host` | NixOS VM test with three nodes: admin SSH and sudo, refused password/root logins, firewall isolation of the management services, node exporter, Incus without networks, KVM (needs KVM) |
| `nixos-incus` | NixOS VM test of Incus on a compute host: the agent's identity runs a container in the Strata project and is refused privileged and nested containers, host paths and devices, low-level settings, other projects and project, server, trust and pool changes; the API listens on loopback only; `strata-incus-setup` reverts project changes and stale trust; the declared dir pool is the project's default (needs KVM) |
| `nixos-storage` | NixOS VM test of storage pools with seven extra disks: ZFS mirror, LVM-thin and dir pools used by the agent's identity; refusal of a disk with a filesystem, the system disk and a changed driver; adoption of a hand-made zpool; removing a pool keeps its data; a foreign volume group stays inactive across a reboot (needs KVM) |
| `nixos-networks` | NixOS VM test of guest networks with a peer on untagged and VLAN 20/30 segments: guests reach only their own network; the host has no address on any guest-facing interface; a guest tagging VLAN 20 on an untagged network does not reach it (with a positive control); undeclared networks and macvlan on the management NIC are refused; a declared change moves a network; a removed network stays while used; reboot (needs KVM) |
| `nixos-vms` | NixOS VM test of `strata vm` as the admin: a NixOS guest (built locally from nixpkgs' Incus VM image module) is created from a spec, boots, reaches a peer and sees its data disk; stop, restart, forced stop; an empty VM; refusals (non-operator user, existing VM, undeclared pool, remote images, image downloads); owner, protection and drift shown, `update`/`delete` refused for a Git-owned VM; the agent's host inventory; audit entries; autostart after a host reboot. Needs **nested** virtualization and fails without it |
| `nixos-nic-names` | NixOS VM test of interface names pinned to MAC addresses: two hosts with the same NICs in opposite order both name them `mgmt0`/`uplink0` by MAC; management (SSH, node exporter, firewall) and the guest bridge work on the pinned names; unpinned NICs keep their kernel names (needs KVM) |
| `nixos-gitops` | NixOS VM test of the controller with a Git server node over ssh: a signed commit is fetched, evaluated (in the service without network) and planned; CLI VMs unreconcilable or unmanaged; unsigned and foreign-signed commits refused and never evaluated; an invalid workload reported; a `flake.lock` change reported as a host change; periodic synchronization; approval creates Git-owned VMs and records the deployment; `strata vm` refused on them; an edit applied as an update; a force-push refused; drift restored; protection, and deletion only with `--allow-delete` keeping data volumes; adoption; pin/unpin; `autoApply = "safe"`; record pruning; state and history across a reboot; host evaluation (the target commit's system matches an independent instantiation, a failure leaves workload planning alone, different commits give different systems, nothing on the host changes); audit (needs KVM) |
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
lint, tests, race-detector tests and `make vulncheck` inside the development
shell. Two more workflows (ADR-0022):

- `codeql.yml`: CodeQL `security-extended` for Go and for the workflows, on
  pushes to `main`, pull requests and weekly. Results are under Security ->
  Code scanning.
- `govulncheck.yml`: weekly and on `main`, uploads govulncheck findings to
  code scanning.

Workflow actions are pinned to commit SHAs with the version in a comment;
Dependabot updates both. Run `make lint` after editing a workflow.

## Dependency updates

Dependabot (`.github/dependabot.yml`) opens pull requests for GitHub
Actions (weekly), flake inputs (monthly, one grouped pull request) and Go
modules (weekly). **A `flake.lock` update changes the kernel, Incus, ZFS,
QEMU and the Go toolchain: run `make vm-test` locally before merging it**,
as for any module change.

### Go vulnerabilities and the allow-list

`make vulncheck` fails when govulncheck finds a vulnerability in code
Strata calls. The Go toolchain comes from `flake.lock`, so a standard
library fix usually means merging (or making) a `flake.lock` update. If
nixpkgs does not have the fix yet, add the ID to `.govulncheck-allow`:

```text
GO-2026-6859 2026-12-31 why the impact is acceptable until the fix lands
```

The date is the last day the entry applies; after it, CI fails again. The
gate also prints entries that are no longer reported, so remove those.

### Repository settings

These are set in the GitHub UI (Settings -> Advanced Security), not in the
repository:

- Dependabot alerts and Dependabot security updates: on.
- Secret scanning and push protection: on.
- Private vulnerability reporting: on (see `SECURITY.md`).
- CodeQL **default setup: off**; `codeql.yml` is the advanced setup, and
  the two conflict.
- Optional: a ruleset on `main` requiring the `nix flake check`,
  `CodeQL (go)` and `CodeQL (actions)` checks.

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
