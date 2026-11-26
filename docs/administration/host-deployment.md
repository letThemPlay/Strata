# Host configuration

**Status: experimental, M3 slice 5 (evaluation only).** Strata can
evaluate a host's NixOS configuration from Git and compare it with the
system the host runs. **It does not build or deploy host configurations
yet**: building arrives in M3 slice 5b, deployment (activation, reboot,
verification, rollback) in M4
([design](../architecture/host-lifecycle.md), ADR-0027). Until then host
changes are applied by hand ([host changes](gitops.md#host-changes)).

Workloads (VMs) are separate: see [configuration from Git](gitops.md).

## What evaluation does

`strata host evaluate` evaluates
`nixosConfigurations.<node>.config.system.build.toplevel` of the commit
the controller targets: the branch head, or the pinned commit
(`strata config pin`). It:

1. checks the commit as synchronization does: signed by a trusted key, and
   descending from the last applied commit (not for a pinned commit);
2. records the source: repository, branch, commit, tree, `flake.lock`
   blob, signer, node and the evaluated attribute;
3. evaluates the system in the controller's evaluation service: no
   network, offline, pure, no import-from-derivation, no substitution, the
   same memory cap and timeout as workload evaluation
   (`strata.gitops.evalTimeout`);
4. reports the system's derivation and output path.

Evaluation **writes the system's derivations (`.drv` files) to the Nix
store**. It builds nothing, activates nothing, and changes neither the
running system, the system profile, the boot loader nor any workload. An
evaluation is bound to the commit it started with: if the controller's
target moves meanwhile (a new commit, a pin), it fails and must be run
again. One evaluation runs at a time.

Results are kept in the controller's memory: after the controller
restarts, the status is `unknown` until the next evaluation.

## Status

```console
$ strata host evaluate
Evaluating 900e78cb2287 (evaluation 20261126T131402Z-4bee07)...
Node:                hv-02
Repository:          ssh://git@github.com/OWNER/hv-01.git (branch main)
Target commit:       900e78cb2287ce8fdab262f643888366cde29e1f
Evaluation:          20261126T131402Z-4bee07, of 900e78cb2287: succeeded in 9.6 s (2026-11-26T13:14:11Z)
Desired system:      /nix/store/…-nixos-system-hv-02-26.11…
  Commit:            900e78cb2287ce8fdab262f643888366cde29e1f (signed by kelvin)
  Tree, flake.lock:  …, …
  Derivation:        /nix/store/…-nixos-system-hv-02-26.11….drv
Running system:      /nix/store/…-nixos-system-hv-02-26.11…
Booted system:       /nix/store/…-nixos-system-hv-02-26.11…

Configuration drift: in-sync
Boot state:          current

Note: Strata can evaluate host configurations but does not deploy them yet.
```

`strata host status` shows the same without evaluating; `--json` prints
it as JSON, `strata host evaluate --no-wait` returns once it started.
Members of `strata-operators` use them, through the controller's socket.

Two separate properties:

| Property | Compares | Values |
|---|---|---|
| **Configuration drift** | The desired system (the evaluated target commit) with the **running** system (`/run/current-system`) | `in-sync`; `differs` (the host runs a different system than Git declares, for example after a manual `nixos-rebuild` or before a host change is applied); `unknown` (the target commit has no successful evaluation, it is being evaluated, or the running system can't be read) |
| **Boot state** | The **running** system with the **booted** one (`/run/booted-system`) | `current`; `reboot-pending` (a configuration was activated without a reboot: the kernel in use is the booted system's); `unknown` (a link can't be read) |

The running system is what is active now; the booted system is what the
host started with. They differ after `nixos-rebuild switch` until the next
reboot.

## Limitations

- **No build and no deployment** (M3 slice 5b, M4). `differs` is reported,
  not corrected.
- **New flake inputs can't be evaluated:** a commit that changes
  `flake.lock` needs inputs that aren't on the host, and evaluation has no
  network. It fails with "changes flake.lock … apply it with nixos-rebuild
  first". Fetching inputs is M4.
- Configurations that need import-from-derivation can't be evaluated.
- Evaluation reads `/run/current-system` and `/run/booted-system` from
  the controller; in M4 the agent reports them.

## API

On the controller's socket (`/run/strata/controller.sock`):

| Request | Reply |
|---|---|
| `GET /api/v1/nodes` | `[{"id": "<node>"}]` |
| `GET /api/v1/nodes/{node}/configuration` | The status: target, latest evaluation, desired system, observed systems, `configurationDrift`, `bootState` |
| `POST /api/v1/nodes/{node}/configuration/evaluate` | `202` with the evaluation (running); `409` while one runs; `404` for another node |
