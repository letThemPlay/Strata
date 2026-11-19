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
| `make nix-check` | `nix flake check` |
| `make check` | All of the above checks |

## What `nix flake check` runs

| Check | Description |
|---|---|
| `build` | Builds all four binaries and runs `go test` on every package |
| `go-fmt` | gofumpt formatting |
| `go-lint` | golangci-lint with `.golangci.yml` (offline) |
| `nix-fmt` | nixfmt formatting of all `.nix` files |
| `nix-lint` | statix and deadnix |
| `examples` | Evaluates `examples/standalone/strata.nix` and checks its structure |

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

`.github/workflows/ci.yml` runs `nix flake check`, then format check, lint,
tests and race-detector tests inside the development shell.
