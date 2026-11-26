# Host lifecycle management (design)

**Status: accepted architecture, not implemented.** Nothing in this
document exists yet unless [ARCHITECTURE.md](../../ARCHITECTURE.md#current-status)
lists it as **Implemented**. The decisions are recorded in ADR-0027
(accepted) and ADR-0028 to ADR-0031 (proposed; each is finalized in the
slice that implements it). This document holds the detail the ADRs refer
to: trust boundaries, state machines, persistence, and the record of what
was checked against the pinned nixpkgs.

Specification: CLAUDE.md v0.3.0, §2.1 ("Host lifecycle is a first-class
responsibility"), §6.1, §8, §9, §17.

## Contents

1. [Scope and principles](#1-scope-and-principles)
2. [Components and trust boundaries](#2-components-and-trust-boundaries)
3. [Operations](#3-operations)
4. [Deployment eligibility](#4-deployment-eligibility)
5. [Facts, inferences and reports](#5-facts-inferences-and-reports)
6. [Host plan](#6-host-plan)
7. [Approvals and acknowledgements](#7-approvals-and-acknowledgements)
8. [NixOS activation semantics](#8-nixos-activation-semantics)
9. [Activation, revert and persistence](#9-activation-revert-and-persistence)
10. [Reboot and boot recovery](#10-reboot-and-boot-recovery)
11. [GC retention](#11-gc-retention)
12. [Executor persistence and crash recovery](#12-executor-persistence-and-crash-recovery)
13. [Storage policy](#13-storage-policy)
14. [API and CLI](#14-api-and-cli)
15. [Records: what lives where](#15-records-what-lives-where)
16. [Pinned-source verification record](#16-pinned-source-verification-record)
17. [Decision register](#17-decision-register)
18. [Hardware validation required](#18-hardware-validation-required)
19. [Recommended CLAUDE.md changes](#19-recommended-claudemd-changes)

## 1. Scope and principles

Host lifecycle management deploys the NixOS configuration of a Strata
host. It is a subsystem of its own, separate from workload orchestration
(Incus), and coordinated with it:

- It never uses `internal/incus` or `internal/workload`, and it is not a
  `reconciler.Provider`. Workload information reaches it only as a
  report from the controller, which it labels as such (§5).
- Evaluate, build, plan, activate, reboot, verify and roll back are
  separate operations with their own records and outcomes. There is no
  opaque "rebuild".
- Building never activates. Activating never implies a reboot. A reboot
  needs its own authorization.
- Git rollback (deploying an older commit) and generation rollback
  (returning to a system profile generation) are different operations.
  Neither reverses persistent data or external effects.
- The privileged component trusts no other Strata component for
  approval, eligibility or the host plan.
- Single node first: safe deployment and recovery, no rolling upgrades,
  no promises of workload availability or migration.

## 2. Components and trust boundaries

```
                 operator session (strata-operators; local, e.g. over SSH)
                 │ strata host plan / approve / acknowledge / confirm / resolve
                 │ (reads the executor's plan; sends approvals to the executor)
                 ▼
            /run/strata/host-approve.sock ◀───────────────────────────────┐
                                                                          │
Git remote ─▶ controller (unprivileged) ─▶ agent (unprivileged) ─▶ strata-host exec (root)
  signed      sync, workload pipeline,      relays requests,          closed operation set,
  commits     builds, display plans,        observed state            eligibility, own plan,
              history                                                 approvals, journal,
                                                                      transient step units
Git remote ─▶ strata-host-fetch (own identity, network, no evaluation) ─▶ executor mirror (read-only to others)
                                                                              │
                                     strata-host-eval@ (own identity, no network) ◀┘
```

| Component | Identity | Network | Trusted for | Not trusted for |
|---|---|---|---|---|
| Controller | `strata-controller` | Yes (Git) | Scheduling, workload pipeline, builds, history, display | Approval, eligibility, host plan, anything the executor acts on |
| Controller evaluation service | `strata-controller` (`nix/modules/gitops/default.nix`) | No | The controller's own needs | Provenance: it runs as the controller and reads the controller's repository |
| Agent | `strata-agent` (DynamicUser) | Loopback only | Relaying, observed state for display | Approval, hardware facts used by the executor |
| `strata-host-fetch` | Own DynamicUser, deploy key by `LoadCredential` | Yes | Fetching the configured repository and ref into the executor's mirror | Evaluating anything |
| `strata-host-eval@` | Own DynamicUser, socket-activated | No | Evaluating `outPath` and manifest from the mirror | Anything else |
| `strata-host exec` | root, socket-activated per request | No | Eligibility, the host plan, approvals, activation, revert, persistence, reboot | — |
| `strata-host-recover.service` | root, oneshot at boot and after executor restarts | No | Finding and settling unfinished deployments | Moving a deployment forward to a new target |
| `strata-host incus-setup` | root (existing, ADR-0017/0018) | No | Pool and network creation rules at activation | — |

Sockets:

- `/run/strata/host-exec.sock`: reachable only by the agent's group.
  Registration, activation and other requests the controller relays.
- `/run/strata/host-approve.sock`: group `strata-operators`. Describe,
  approve, acknowledge, confirm, revoke and resolve. The controller and
  the agent are not members.

The executor identifies callers by kernel peer credentials
(`SO_PEERCRED`, and `SO_PEERPIDFD` where a process must be inspected).
Every request is audited in the executor's journal and the system
journal, with caller, deployment, operation and result.

Residual risk, stated plainly: members of `strata-operators` can approve,
and `strata-admin` has passwordless sudo. Approvals protect against
compromised components, not against compromised operators. A target
configuration is root code once activated: protections against a
misbehaving target (§9.3) are best effort.

## 3. Operations

| Operation | Performed by | Privilege | Changes the host |
|---|---|---|---|
| Evaluate | Controller (display), `strata-host-eval@` (authoritative) | None | No |
| Build | Controller through nix-daemon | None | Store only |
| Plan | Executor (authoritative), controller (display) | Root, read-only inspection | No |
| Activate (`test`) | Executor, in a transient unit | Root | Yes, temporarily (§8) |
| Verify | Executor (host checks), controller (workload report) | Root / none | No |
| Revert | Executor, transient revert unit | Root | Yes |
| Persist (profile + boot entry) | Executor | Root | Yes |
| Reboot | Executor | Root | Yes |
| Generation rollback | Executor (a deployment whose target is an existing generation) | Root | Yes |
| Inspect activation (`dry-activate`) | Executor, only on explicit operator authorization (§6.4) | Root | Not intended, not guaranteed |

## 4. Deployment eligibility

A valid signature does not make a commit deployable. The executor checks
four things independently, from root-controlled policy and its own
mirror, and records the result as an **eligibility decision**.

### 4.1 Root-controlled policy

`/etc/strata/host-deployment.json`, generated from NixOS options of the
**running** system (root-owned, in the system closure):

- `node`: this node's ID.
- `repository`: the deployment repository URL.
- `ref`: the permitted ref (one branch in M4).
- `trustedSigners`: allowed SSH signing keys.
- Policy values: approval TTL, workload-report maximum age, boot recovery
  attestation, revert-failure policy.

The controller cannot choose the URL, ref, node or policy. A change takes
effect only by activating a configuration that contains it, which itself
goes through the current policy. The executor records the SHA-256 of the
policy file as `policyDigest`.

### 4.2 The four checks

1. **Signature validity.** `git verify-commit` on the executor's mirror
   with an allowed-signers file built from `trustedSigners`. Only SSH
   signatures are accepted (as in `internal/gitops`).
2. **Repository and ref eligibility.** `strata-host-fetch` fetches
   `repository`'s `ref` into the mirror (`transfer.fsckObjects=true`, so
   object IDs are recomputed on receipt). The commit is eligible only if it
   is the fetched tip of `ref` or an ancestor of it
   (`git merge-base --is-ancestor`). The record holds the URL, ref, tip and
   fetch time. Commits only reachable from other branches, or supplied
   only by the controller, are not eligible.
3. **Forward-only.** The executor keeps a root-owned anchor,
   `anchor.json`: the commit of the last deployment it **persisted** as
   healthy. A commit is forward if it equals or descends from the anchor.
4. **Node eligibility.** The executor evaluates only
   `nixosConfigurations.<node>` with `node` from its own policy, and
   requires the target manifest's node ID to equal it.

The eligibility decision is
`{signer, repository, ref, refTip, fetchedAt, anchor, relation, node, commit, tree, flakeLock}`
with `relation` one of `initial`, `forward` or `rollback`. Its SHA-256,
`eligibilityDigest`, is part of the plan digest (§7).

### 4.3 Cases

| Case | Rule |
|---|---|
| **Initial registration** (no anchor) | Relation `initial`. Approval needs the explicit flag `--initial`, recorded. The anchor is set when that deployment persists as healthy. The running system's revision is shown for information, but it doesn't become the anchor implicitly. |
| **Normal forward deployment** | Relation `forward`. Approved as usual. The anchor moves forward only when the deployment persists as healthy. |
| **Cancellation** | The deployment becomes CANCELLED. Its approvals are invalidated. The anchor is unchanged. |
| **Re-registration** | Same deployment ID with the same commit and target: idempotent, returns the existing entry. A different commit or target: refused; a new deployment ID is required. A new registration for the same commit gets a new ID, and its own plan and approvals. |
| **Recovery after `unknown`** | No new deployment is accepted until an operator resolves it (§12.7). Resolution records one of two decisions: "the running system is accepted as deployed", which moves the anchor only if that system's revision is eligible and forward; or "failed", which leaves the anchor unchanged. |
| **Git rollback** (an older eligible commit) | Relation `rollback`. Needs the explicit flag `--rollback-below-anchor`, recorded in the approval. The anchor does **not** move backwards: the record notes "running commit X, below anchor A". Later forward deployments must still descend from A; the usual fix is a new commit on top of A. |
| **Generation rollback** | The target is an existing system profile generation, so no fetch or evaluation is needed. The relation is `rollback` against the anchor, unless the generation's revision is forward. It is a deployment with its own plan and approval (flag `--rollback-below-anchor` where needed). |

Forward-only is therefore never weakened silently: every exception is an
explicit, recorded, operator-approved deployment.

### 4.4 Invalidation

A change to `policyDigest` (repository, ref, signers, any policy value),
to the anchor, or to the eligibility decision changes the plan digest,
which invalidates outstanding approvals (§7.3).

## 5. Facts, inferences and reports

Everything in a host plan belongs to one of three categories. The CLI
shows them separately and records them separately.

### 5.1 Verified host facts

Established by the executor's own inspection, with the source recorded:

- Current, booted and profile systems, and the target closure.
- Target kernel, initrd and kernel-modules paths, compared with the
  booted ones (a reboot is needed for them to take effect).
- Differences between the current and target manifests: declared
  network configuration, firewall, SSH, NIC pins, pools, Incus package,
  boot loader settings.
- Boot loader and ESP changes (systemd-boot package and configuration).
- Unit **file** differences: units added, removed or changed between
  `/etc/systemd/system` of the current system and `etc/systemd/system`
  of the target (a static comparison; no target code runs).
- Hardware facts of §5.4, each with source, time and confidence.

### 5.2 Inferred disruption risks

Reasoned assessments derived from facts, not guarantees:

- The unit actions `switch-to-configuration` is likely to take (restart,
  reload, stop, start), inferred from the unit file differences and the
  `X-RestartIfChanged`, `X-StopIfChanged` and `X-OnlyManualStart` keys.
  This mirrors the pinned `switch-to-configuration-ng` logic only
  partially and is labelled "inferred".
- A guest uplink or bridge change is likely to interrupt guest
  networking.
- A change to the Incus service may affect Incus; with
  `virtualisation.incus.softDaemonRestart` (true in the pinned nixpkgs)
  instances are expected to keep running.
- A package, Incus or `stateVersion` change may run persistent
  migrations.
- A reboot will interrupt every running workload.

### 5.3 Controller-reported workload information

From the controller's view of Incus, **not verified by the executor**:
running instances, autostart, placement, storage and network
relationships, dependencies. The executor stores the exact report it
received (bytes, SHA-256, timestamp, sending identity) and never claims
to have verified it.

### 5.4 Hardware facts

Executor-owned inspection is more trustworthy than the agent's report,
but not every fact is unambiguous. Each fact is recorded as
`{value, source, observedAt, confidence: certain | ambiguous | unavailable, recheckedBeforeOperation}`.

| Fact | Source | If it can't be established reliably |
|---|---|---|
| NIC name and MAC for the management interface and pins | `/sys/class/net/<if>/address`, `addr_assign_type`, permanent address through ethtool | If a permanent MAC can't be obtained for a pinned or management interface: `unavailable`. Deployments that change management or pins are refused. |
| Disk for a new pool | `/dev/disk/by-id`, sysfs, udev database (`internal/blockdev`) | If the ID resolves differently than at planning, or the disk gained a partition, signature or use: refused. |
| Booted kernel modules (e.g. ZFS) | `/run/booted-system/kernel-modules`, `/proc/modules` | `unavailable`: pool creation needing the module is refused until after a reboot. |
| ESP systemd-boot version, entries and counters | `bootctl status`, `LoaderInfo`/`LoaderEntrySelected` EFI variables, `/boot/loader/` | `unavailable`: no boot recovery (console required). |
| Watchdog | `/sys/class/watchdog`, systemd `RuntimeWatchdogSec` | Absent: hang recovery (§10.2 B) is manual. |
| Firmware identity | `/sys/class/dmi/id/bios_*`, EFI `BootOrder` | Part of the attestation fingerprint (§10.4). |

All safety-critical facts are **rechecked immediately before** the step
that depends on them (activation, pool creation, reboot). A fact that
differs from the plan changes the digest, which invalidates the approval.
A safety-critical fact that can't be established causes a refusal that
names the fact and the reason.

## 6. Host plan

### 6.1 Change classes

| Class | Examples | Required flags |
|---|---|---|
| `routine` | Packages, service settings | — |
| `services` | Unit file changes; Incus, agent, controller, sshd | — |
| `management` | Management interface, addressing, firewall, SSH, NIC pins | `--allow-management-change`; confirmation (§9.4); console until validated |
| `kernel` | Kernel, initrd, kernel parameters, modules; `switch-to-configuration` exit 100 | Reboot approval; boot recovery attested or `--console-access` |
| `bootloader` | systemd-boot package, its configuration, the ESP | Reboot approval; `--console-access` always |
| `storage-create` | New or adopted pool | — (validated twice; §13) |
| `storage-refused` | Pool modification, undeclaration; `fileSystems`, LUKS, root device, disko | Refused, with no flag to override |
| `side-effects` | Incus version, `stateVersion`, pools or networks to create, accounts removed, major package versions | Shown as irreversible; `--accept-irreversible` |
| `workload-interruption` | Reboot; inferred guest network or Incus interruption | A workload acknowledgement (§7.2) |

### 6.2 Plan digest

`planDigest` is the SHA-256 of the canonical plan document produced by
the executor: node, eligibility digest, commit, tree, flake lock blob,
evaluation target (`nixosConfigurations.<node>.config.system.build.toplevel`),
target closure, current, booted and source systems, source generation,
classes with their facts and inferences, hardware facts used, policy
digest, boot recovery attestation fingerprint, and the relevant Nix
settings (`trusted-users`, `require-sigs`, substituters). Workload
reports are **not** part of it (§7.2).

### 6.3 Presentation

```
Host plan for hv-02 (executor plan, digest sha256:…)
Verified host facts (inspected by the executor):
  commit       ac572fa… on main (fetched 10:02Z), forward from anchor 91f0375
  kernel       /nix/store/…-linux-6.18.55 -> …-linux-6.18.60   (reboot needed to take effect)
  units        changed: incus.service, strata-agent.service
Inferred risks (assessments, not guarantees):
  services     incus is likely to restart (soft restart: instances expected to keep running)
  workloads    a reboot will interrupt all running workloads
  side effects reverting will not undo: Incus 6.0 -> 6.1 database upgrade
Workload report (from the controller, NOT verified by the executor; sha256:…, 10:03Z):
  running      git-01 (autostart: yes), db-01 (autostart: NO: stays stopped after a reboot)
Strata cannot keep workloads available, migrate them, or guarantee that they start again.
Requires: reboot approval; --accept-irreversible; workload acknowledgement of report sha256:…
```

### 6.4 Inspecting activation with `dry-activate`

Default plans never run target code. `dry-activate` runs the target's
`dry-activate` script as root, which executes every activation script
marked `supportsDryActivation`. A signed target is not automatically safe
to run during planning. So:

- It is a separate operation, `inspect-activation`, only on the approval
  socket, only with `--run-target-code`, after a warning that target code
  runs as root. It is audited, its approval is bound to the deployment and
  target, and it is single use.
- Its output is shown as "reported by the target's dry-activate
  scripts". It is never merged into verified facts.
- Its limits are shown with it: activation scripts not marked
  `supportsDryActivation` are skipped, so their effects are unknown; it
  doesn't show what services do when they restart (migrations, data
  writes); runtime conditions differ at the real activation; and a script
  claiming dry support may still have effects.

## 7. Approvals and acknowledgements

### 7.1 Host-plan approval

Sent by the operator's CLI to `/run/strata/host-approve.sock`, never
through the controller:

```json
{
  "apiVersion": "approval.strata.dev/v1alpha1",
  "scope": "activate | reboot | rollback | inspect-activation",
  "deployment": "20261126T101500Z-3fa1c2",
  "node": "hv-02",
  "repository": "ssh://git@github.com/…/hv-01.git", "ref": "main",
  "commit": "…", "tree": "…", "flakeLock": "…",
  "evaluation": "nixosConfigurations.hv-02.config.system.build.toplevel",
  "target": "/nix/store/…-nixos-system-hv-02-…",
  "current": "/nix/store/…", "booted": "/nix/store/…", "sourceGeneration": 41,
  "eligibilityDigest": "sha256:…", "planDigest": "sha256:…",
  "hardwareDigest": "sha256:…", "policyDigest": "sha256:…",
  "attestation": "sha256:… | unverified",
  "accepted": ["management", "side-effects", "rollback-below-anchor"],
  "approver": "uid:1000",
  "issuedAt": "…", "expiresAt": "…", "nonce": "…"
}
```

The executor stores it only if every field matches its own registry
entry and plan, and `accepted` covers every flag the plan requires.
`approver` comes from peer credentials, not from the request. Before
activation the executor **recomputes** eligibility, the plan, hardware
facts, policy, attestation and Nix settings, and refuses if any digest
differs.

### 7.2 Workload-interruption acknowledgement

A separate object, `scope: workload-ack`, required when the plan has a
`workload-interruption` class:

- Bound to the deployment, `planDigest`, the **report digest** and the
  report timestamp, and to the identity that sent the report.
- The CLI displays the report **as stored by the executor** (fetched
  from the approval socket), so the digest acknowledged is the report
  shown.
- A report older than the policy's maximum age (default 10 minutes)
  can't be acknowledged. A new report from the controller invalidates
  earlier acknowledgements.
- An acknowledgement states that the operator accepts the interruption
  described in an unverified report. It is not a verification of
  workload state, and it guarantees nothing about availability, migration
  or restart.

Approval and acknowledgement are recorded separately. An approval
without a required acknowledgement can't be consumed.

### 7.3 Lifetime and invalidation

| State | Cause |
|---|---|
| `ISSUED → CONSUMED` | Its operation started (atomic rename, §12.3) |
| `ISSUED → EXPIRED` | TTL passed. Default 30 minutes, policy maximum 24 hours |
| `ISSUED → INVALIDATED` | Any digest changed: eligibility (fetch result, anchor), plan, hardware facts, policy (signers, repository, ref, TTLs), Nix settings, attestation, current, booted or source system; another deployment started; the deployment was cancelled or re-registered |
| `ISSUED → REVOKED` | An operator revoked it |

A reboot approval is separate (`scope: reboot`), bound to the deployment,
target, booted system and attestation, and invalidated by any activation
in between. In M6, approvals for a remote controller become the same
object signed with an operator's SSH key; the format above is designed
for that.

## 8. NixOS activation semantics

Checked against the pinned nixpkgs (§16).

| Mode | Effect |
|---|---|
| `test` | **A real, temporary activation, not a dry run.** Pre-switch checks, a sync, the init-interface check (exit 100 if incompatible: nothing takes effect before a reboot), then it works out unit actions, stops units, runs the target's `activate` script as root (updates `/etc` and `/run/current-system`, accounts, setuid wrappers, all `system.activationScripts`), reloads or re-executes systemd, and restarts and starts units. It does not install the boot loader. |
| `boot` | Pre-switch checks and boot loader installation, then exit. No activation. |
| `switch` | Boot loader installation, then the same activation as `test`. |
| `dry-activate` | Prints planned unit actions and runs the target's `dry-activate` script (§6.4). |
| `check` | Pre-switch checks only. |

- Lock: a non-blocking exclusive `flock` on
  `/run/nixos/switch-to-configuration.lock`; a concurrent run fails.
- Exit codes recorded as distinct outcomes: `2` (activate script failed),
  `4` (units failed to start or restart), `100` (init incompatible).
- Units: actions are only taken for active units that have a unit file
  under `/etc/systemd/system` of the current configuration. Transient
  units (in `/run/systemd/transient`, with no file in either
  configuration and no matching template) are not stopped or restarted by
  it.
- Boot entries are generated from the system profile generations, and the
  default is the generation matching the toplevel passed in. Persisting
  therefore means `nix-env -p /nix/var/nix/profiles/system --set <target>`,
  then `<target>/bin/switch-to-configuration boot`.

**What re-activating the previous system restores, and what it doesn't:**

| Restored | Not restored |
|---|---|
| `/etc` and `/run/current-system`; the unit set and unit configuration; running services (restarted as before); the nftables ruleset; sysctls applied by `systemd-sysctl`; setuid wrappers; declared accounts | Data services wrote (Incus database schema upgrades, migrations run by new package versions); `/var` state, including tmpfiles creations and removals; pools and networks `incus-setup` created (kept by design); loaded kernel modules and netdevs (until a reboot); UID/GID allocations in `/var/lib/nixos`; generated keys; timers and jobs that ran; firmware updates; external effects; workload interruptions and connections already dropped |

## 9. Activation, revert and persistence

### 9.1 Sequence

```
consume approval ─▶ arm revert timer ─▶ ACTIVATING: <target>/bin/switch-to-configuration test (transient unit)
  exit 0 ─▶ VERIFYING (host checks; confirmation for `management`) ─▶ pass ─▶ PERSISTING ─▶ done
  exit 2/4, verification fails, or the deadline passes ─▶ REVERTING: <current-before>/bin/switch-to-configuration test
  exit 100 ─▶ nothing activated: the deployment continues only on the reboot path (§10), with its requirements
```

The boot default is unchanged until PERSISTING, so a crash or hang before
then returns to the previous system on the next boot.

### 9.2 The revert timer

- **Installation.** Before activation, the executor creates a transient
  timer and service in PID 1 with `systemd-run`, named
  `strata-host-revert-<deployment>.{timer,service}`: no `@` and no unit
  file in any configuration. `switch-to-configuration` takes no action on
  such units (§8). The deadline is an absolute UTC time (`OnCalendar=`),
  also recorded in the journal.
- **The rollback executable comes from the previous system.** The
  service's command uses absolute store paths from the **current (soon
  previous) system**: that system's `strata-host` binary and
  `<previous>/bin/switch-to-configuration test`. Both are GC-rooted
  (§11). It doesn't use `PATH`, `/etc`, the network, the agent or the
  controller, so a target that changes networking, systemd configuration,
  the agent or the controller doesn't change the revert path.
- **Restarts.** The timer lives in PID 1, so it survives restarts of the
  executor, agent and controller. Whether it survives a systemd
  re-execution (which `switch-to-configuration` triggers when the systemd
  package or `system.conf` changes) is systemd behaviour, **to validate in
  a VM test**. `strata-host-recover.service` re-arms a missing timer if
  the deadline hasn't passed, and reverts at once if it has. A host reboot
  before persisting boots the previous system anyway.
- **The timer fires while activation is still running.** The revert unit
  takes the state lock (§12.6), finds the in-flight activation step, stops
  its unit (SIGTERM, then SIGKILL after a grace period, which also
  releases `switch-to-configuration`'s lock), records the activation as
  `aborted, outcome uncertain`, then reverts.
- **Partial activation with an error** (exit 2 or 4) reverts immediately,
  without waiting for the timer.
- **Revert fails** (non-zero exit, or the current system isn't the
  previous one afterwards): the deployment ends FAILED with outcome
  `revert-failed`. New deployments are blocked until an operator resolves
  it. `strata host status` shows it first, along with the console recovery
  path: reboot into the unchanged boot default. With an explicit Git
  policy `onRevertFailure = "reboot"` (default `"report"`), the executor
  reboots instead, which is an authorized automatic reboot by policy.
- **Limits.** A target's activation runs as root, so it can defeat these
  protections (stopping or masking units, breaking PID 1). The last layer
  is the unchanged boot default, reached by a watchdog or a manual reset
  (§10.2). Reverting restores configuration, not the effects listed in §8.

### 9.3 Health verification

Executor checks after activation and after a reboot: the required units
from the target manifest are active; the management interface has its
declared addresses and is routable; the agent answers on its socket; the
Incus API answers (through `incus-setup`'s root client, read-only); pools
are online; no units failed that weren't failed before. Each result is
recorded separately. The workload report is recorded next to them as
controller-reported.

### 9.4 Confirming a new SSH management session (experimental)

The confirmation is named narrowly: **"operator confirmed a new SSH
management session"**. It is evidence that a new SSH connection to the
management address worked. It is not proof that management services are
healthy; health is checked separately (§9.3).

The mechanism (unproven; console or out-of-band access stays required for
`management` changes until it passes on both the VM test and hv-02):

1. The confirming process is identified with `SO_PEERPIDFD`.
2. Its logind session (D-Bus `GetSessionByPID`) must be `Remote=yes`,
   `Service=sshd`, `State=active`, and must have started after the
   activation intent.
3. The per-connection `sshd` process leading the session must have
   started after the activation intent, so that a new channel over an old
   multiplexed connection doesn't count.
4. The confirming operator's identity (uid, session) is recorded. It
   may differ from the approver's.

Spike acceptance criteria:

| # | Case | Expected |
|---|---|---|
| 1 | New SSH connection after activation | Accepted |
| 2 | Session from before activation | Refused |
| 3 | New channel over a pre-activation `ControlMaster` connection | Refused |
| 4 | Local console session | Refused as remote evidence; accepted only with `--console`, recorded as a console override |
| 5 | `sudo`; `tmux` started before or after activation | Same result as the underlying session, documented |
| 6 | PID reuse; race between connecting and the lookup | No false acceptance |
| 7 | A change that breaks SSH | Confirmation impossible; the revert timer restores the previous system; outcome `reverted` |
| 8 | Strata's sshd (PAM with `pam_systemd`) | Session properties as assumed |
| 9 | SSH works but the agent is unhealthy | Confirmation accepted as SSH evidence; health verification fails; the deployment reverts |
| 10 | SSH works but the controller can't reconnect | Same: health fails, reverts; the outcome names the failed check |
| 11 | A different authorized operator confirms than the one who approved | Accepted; both identities recorded |
| 12 | Identity recorded | The record holds the confirmer's uid, session ID, remote host and session and connection start times |

If case 3 can't be met reliably, the confirmation is reduced to "a remote
session started after activation", with the multiplexing caveat
documented, and console access stays required.

### 9.5 Persistence

1. `nix-env -p /nix/var/nix/profiles/system --set <target>` (a new
   generation; not run again if the profile's newest generation is
   already the target).
2. `<target>/bin/switch-to-configuration boot` (repeatable: the builder
   rewrites the entries and `loader.conf`).
3. Check: an entry for the target exists, and `loader.conf` selects it.

## 10. Reboot and boot recovery

### 10.1 Four separate levels

1. **The boot loader selected and started a system** (`LoaderEntrySelected`,
   `/run/booted-system`).
2. **systemd reached `boot-complete.target`**, so `systemd-bless-boot`
   marked the entry good (with boot counting).
3. **Strata's post-boot health verification** passed (§9.3).
4. **Workloads are healthy and available**: controller-reported only.

Each level is recorded separately. Reaching `boot-complete.target` is not
evidence that Strata or its workloads are healthy, only that the units
ordered before it succeeded. Strata may order a minimal health unit
before `boot-complete.target`, so that a boot without management isn't
blessed; the full verification (level 3) still runs separately.

### 10.2 Failure classes

| Class | What happens | Recovery |
|---|---|---|
| **A. The boot fails and something reboots the host** | Kernel panic with `panic=<s>`; an initrd failure set to panic (to verify per initrd type); a boot that fails `boot-complete.target` followed by a reboot (by policy or by an operator) | With boot counting, each try decrements the entry's counter. At zero, systemd-boot falls back to the **newest entry not marked bad**. A failed `boot-complete.target` alone does not reboot the host. |
| **B. The host hangs** | No panic and no progress (deadlock, an initrd waiting for input, an emergency shell) | A hardware watchdog (`RuntimeWatchdogSec`, `/dev/watchdog`; not every host has one, and it may not cover the initrd) or a **manual power cycle** (console or out-of-band). Then as in A. |
| **C. The boot loader fails** | systemd-boot or the ESP broken; firmware won't start it; an interrupted ESP write (FAT has no atomic rename) | **Boot counting can't help.** Firmware boot menu, rescue media, console or out-of-band. |

Because fallback picks the newest entry not marked bad, every rollback
creates a **new** generation (`nix-env --set`), never
`--switch-generation`.

### 10.3 Requirements

| Change | Ordinary activation | Reboot approval | Boot recovery | Console / out-of-band |
|---|---|---|---|---|
| `routine`, `services` | ✓ | — | — | — |
| `management` | ✓, with confirmation and revert timer | — | — | Required until the revert timer and confirmation pass hardware validation on that host |
| `kernel` | Userspace only | ✓ | Boot counting, **only if attested and not stale** | Required without valid attestation; always for class B unless a watchdog is attested |
| `bootloader` | Persist only | ✓ | Not possible (class C) | Always |
| `storage-create` | ✓ | — | — | — |

### 10.4 Attestation

Declared in Git per host
(`strata.host.boot.recovery = { mechanism = "boot-counting"; validated = "<date>"; watchdog = <bool>; fingerprint = "<sha256>"; }`).
It states that hardware validation on **that host** passed for the
classes it claims. The executor computes the **attestation fingerprint**
at every plan and before every reboot, from:

- the ESP's systemd-boot binary (hash and version, which must be at least
  260 for `preferred`);
- firmware identity (DMI BIOS vendor, version and date) and the EFI
  `BootOrder`;
- the kernel and initrd arrangement (systemd or scripted initrd, kernel
  parameters including `panic=`);
- the boot loader configuration (`bootCounting.tries`, `timeout`,
  `configurationLimit`);
- the watchdog device and `RuntimeWatchdogSec`;
- the boot recovery policy version.

If the fingerprint differs from the attested one, the attestation is
**stale** and treated as absent: `boot recovery: unverified`, and console
access is required, until the host is validated again and the new
fingerprint declared. No mechanism is described as automatic before its
hardware validation passes on the host in question. A VM test is never
counted as hardware validation.

### 10.5 Reboot state machine

```
REBOOT_REQUIRED ─(reboot approval)─▶ REBOOTING: intent records the boot ID, booted and target systems
on start (strata-host-recover.service):
  boot ID unchanged      ─▶ the reboot didn't happen: REBOOT_REQUIRED again (a new reboot approval is needed)
  booted == target       ─▶ level 2 (blessed?) and level 3 (health) ─▶ SUCCEEDED | FAILED (outcome: unhealthy)
  booted == previous     ─▶ FAILED (outcome: boot-fallback); the profile's newest ≠ running system is recorded
  booted == other system ─▶ FAILED (outcome: unknown) ─▶ operator resolution
  host never returns     ─▶ stays REBOOTING: a single node can't observe its own absence (console or out-of-band)
```

## 11. GC retention

A deployment record doesn't protect a store path. The executor keeps
**direct GC roots** in a root-owned directory,
`/nix/var/nix/gcroots/strata-host/`, created with
`nix-store --realise <path> --add-root <root>`, so the root is added by
the daemon without a window for the collector.

| Must be retained | Root |
|---|---|
| Running system | NixOS's tmpfiles root `gcroots/current-system` (when `nix.enable`), plus Strata's `current` root, kept independent of the target's settings |
| Booted system | NixOS's `gcroots/booted-system`, plus Strata's `booted` root |
| Target of an unfinished deployment | `<deployment>-target`, created at registration |
| Previous system (rollback source) | `<deployment>-previous`, created before the activation intent |
| Last known good | `last-good`, the target of the last healthy persisted deployment; moved only when the next one persists as healthy |
| Systems referenced by unresolved records (`unknown`, `revert-failed`) | Their roots stay until the operator resolves them |
| Systems involved in a pending reboot or boot recovery | The target and the fallback system, until the reboot outcome is recorded |

Roots are removed only when the deployment is final and resolved, and the
path is not current, booted, last-good, or referenced by another
unresolved deployment. System profile generations are roots through Nix's
profile mechanism (Nix behaviour, not nixpkgs; to verify in a VM test),
but `nix-collect-garbage -d` or `--delete-older-than` deletes generations,
and with them their **boot entries**. Strata therefore relies on its own
roots for closures, and before a reboot checks that the fallback system
still has a boot entry. If not, the reboot needs `--console-access`.

Recovery when a closure is missing or invalid (`nix-store --check-validity`):

- Target missing before activation: FAILED, outcome `closure-missing`.
- Previous system missing during revert: no revert by activation is
  possible; outcome `unknown`, console path (reboot into the boot default,
  if its entry exists).
- Fallback entry missing before a reboot: reboot refused without
  `--console-access`.

## 12. Executor persistence and crash recovery

### 12.1 Layout

`/var/lib/strata-host/` (root, 0700):

- `state.lock`: the state lock (§12.6).
- `anchor.json`: the forward-only anchor (§4).
- `approvals/<nonce>.json`: issued approvals.
- `deployments/<id>/`:
  - `registration.json`, written once;
  - `journal`;
  - `head.json`;
  - `results/<step>-<attempt>.json`;
  - `consumed/` (approvals moved here when consumed).
- `mirror.git`: owned by `strata-host-fetch`, read-only to everything else.

Every file is written to a temporary name, fsynced, renamed, and its
directory fsynced.

### 12.2 Journal encoding and hash chain

One record per line: `<body> <SP> sha256:<hex> <LF>`, where `<hex>` is the
SHA-256 of the exact bytes of `<body>`.

- `<body>` is UTF-8 JSON with no insignificant whitespace, keys in this
  order: `seq`, `prev`, `at`, `bootId`, `type`, `step`, `attempt`,
  `payload` (whose keys are sorted). Integers only, no floats.
- One encoder produces it. Validation hashes the stored bytes, so it
  never depends on re-encoding.
- `seq` starts at 1 and increases by exactly 1.
- `prev` is the hash of the previous record's body. The first record's
  `prev` is the SHA-256 of `registration.json`, which binds the journal to
  its registration.
- `head.json` (`{seq, hash}`) is rewritten atomically after each append
  has been fsynced.

| Damage | Detected by |
|---|---|
| Modified record | Its hash; and the next record's `prev` |
| Missing record | A gap in `seq`, or a `prev` mismatch |
| Reordered or duplicated record | `seq` not increasing by 1, or a `prev` mismatch |
| Lost trailing records | `head.json` points beyond the last valid record (head may lag the journal by at most one record, the crash window between append and head update) |

This protects against accidental damage and crashes, not against root
rewriting the files.

### 12.3 Torn tail

Recovery reads the journal from the start. A record is valid if its line
ends in LF, has the `<body> sha256:<hex>` form, its hash matches, and its
`seq` and `prev` continue the chain. The first invalid line marks the
**offset** where the tail starts.

- If nothing valid follows that offset, the bytes from it to the end of
  the file are a torn tail. They are copied to
  `journal.tail-<time>.quarantine` (fsynced), then the journal is
  truncated to the offset (`ftruncate`, fsync).
- If valid records follow an invalid one, it isn't a torn tail. Nothing
  is truncated, and the deployment's outcome is `unknown`.

Appends only happen after recovery has established that the file ends
with LF at the expected offset. The writer keeps the expected file size
and refuses to append if the actual size differs, so a new record can't be
joined to a fragment.

### 12.4 Step outcomes

The **intent** for a step is made durable before the step starts. A step
is in one of three states:

- **Known not started:** no durable intent.
- **Known completed:** a durable `DONE` record, or a durable result file
  (`results/<step>-<attempt>.json`) written by the step process itself.
- **Outcome uncertain:** a durable intent with no `DONE` record and no
  result file. A process may finish a step and crash before recording it.

The step process (`strata-host exec run-step`, run in a transient unit)
writes its result file and the `DONE` record itself, so results don't
depend on systemd keeping transient-unit results. Systemd's result is only
secondary evidence.

| Step | Evidence inspected for an uncertain step | Decision |
|---|---|---|
| Consume approval | The approval file's location (an atomic rename) | Never uncertain |
| Activate | `/run/current-system`, failed units, the transient unit's result if retained | Not re-run (it isn't idempotent). Revert to the previous system; outcome `reverted-after-uncertain`. |
| Revert | `/run/current-system` == previous? | If yes: done. If at the target: run the revert once more (re-activating the previous system is the timer's own action). Otherwise: `unknown`. |
| Persist | The profile's newest generation; the boot entry for the target; `loader.conf` | Run only the missing parts (no second `nix-env --set` if the profile's newest is already the target). |
| Reboot | Boot ID | Changed: the reboot happened. Unchanged: it didn't. |
| Pool creation (inside `incus-setup`) | Incus pool list, through `incus-setup` | Reported, never repeated by the executor |

### 12.5 Crash points

| Crash point | Observed on recovery | Decision |
|---|---|---|
| Approval consumed, no activation intent | current == previous | FAILED, outcome `not-started`. The approval isn't reused. |
| During activation, host still up | Result file, current system | Result present: continue (verify or revert). Missing: uncertain, so revert (§12.4). |
| During activation, host crashed | Boot ID changed, booted == previous (boot default unchanged) | FAILED, outcome `reverted-by-reboot`. Irreversible effects may have happened, and the record says so. |
| After activation, before the health result | current == target, deadline not passed | Run verification again (read-only). Deadline passed: check that the timer reverted; if not, revert. |
| During revert | current system, boot ID | As in §12.4. |
| During persistence | Profile, entries, `loader.conf` | Complete the missing parts. If the host doesn't boot: class C. |
| Reboot approved, host didn't go down | Boot ID unchanged | REBOOT_REQUIRED; a new reboot approval is needed. |
| After the reboot | Boot ID changed | §10.5. |

### 12.6 Locking

`state.lock` is an exclusive `flock`. Its lifetime is the file
descriptor, so a process that dies releases it.

**Held** for: registration; approval issue, consumption and revocation;
acknowledgement; cancellation; every state transition and its journal
append, including the decision before it; starting each step (activation,
revert, persistence, reboot), up to the step's intent being durable and
its transient unit being started; the reboot transition up to calling
`systemctl reboot`; the recovery scan and its decisions; operator
resolution.

**Not held** while waiting for an approval, a confirmation or an
acknowledgement (a deployment waits in a state, not in a lock), nor while
a long step runs. A running step is represented by its durable intent,
which records the unit name and boot ID. Other mutations check it under
the lock: they refuse, except a revert, which pre-empts an in-flight
activation (§9.2). Waiting for the lock is bounded (30 s), after which a
request fails as busy. Read-only requests take no lock and read the
journal up to `head.json`.

`switch-to-configuration`'s own lock is a second guard against two
activations.

### 12.7 Discovery and resolution

Socket activation doesn't resume anything by itself.
`strata-host-recover.service` (root, oneshot,
`WantedBy=multi-user.target`, `After=local-fs.target`,
`Before=strata-agent.service strata-controller.service`) runs at every
boot and is also started after an executor failure. It:

1. Takes the state lock.
2. For each unfinished deployment: validates the journal (§12.2–12.3),
   then inspects the boot ID, the current, booted and profile systems, the
   boot entries and counters, result files and GC roots.
3. Records `RECOVERY{observed, decision}` and applies only the decisions
   of §12.4–12.5. It never moves a deployment forward to a new target.

**`unknown` outcomes** (the system isn't the previous one or the target,
the journal is corrupt, an uncertain step's evidence is ambiguous, a
closure is missing): the observed facts are recorded, new host deployments
are blocked, and `strata host status` shows it. `strata host resolve ID`,
over the approval socket, records the operator's identity and decision
(§4.3). The controller's records follow the executor's state, which is
authoritative for host deployments.

## 13. Storage policy

| Case | Policy | Enforcement |
|---|---|---|
| New pool, or adopting an existing one | Needs approval and validation | Planner class `storage-create`. The disk's facts are checked at planning, **again by the executor immediately before activation**, and finally by `strata-host incus-setup` at creation (an empty whole disk, not a system disk, no partitions or signatures, unused; `source.wipe` never set; ADR-0018). Any change in the facts refuses. |
| Changing an existing pool's declaration | Unsupported | Refused by the planner. `incus-setup` already leaves such pools alone and reports them. |
| Removing a pool declaration | Not automated | Refused by the planner (undeclaring closes the pool to the project). A manual procedure retires pools. |
| Disk reinitialization, destructive filesystem changes, root-device changes, `fileSystems`, LUKS, disko | Prohibited | Refused by the planner; disko runs only in the installer. |

## 14. API and CLI

| Resource | Methods | Served by |
|---|---|---|
| `/api/v1/nodes`, `/api/v1/nodes/{node}` | GET | Controller |
| `/api/v1/nodes/{node}/configuration` | GET: desired host configuration, evaluated commit, target, drift | Controller |
| `/api/v1/nodes/{node}/system` | GET: current and booted systems, revision, reboot pending, boot recovery status | Agent |
| `/api/v1/nodes/{node}/system/generations[/{n}]` | GET | Agent, with records |
| `/api/v1/nodes/{node}/system/health` | GET | Agent |
| `/api/v1/nodes/{node}/builds[/{id}]` | GET, POST | Controller |
| `/api/v1/nodes/{node}/plan` | GET: the controller's view and the executor's plan digest | Controller, executor |
| `/api/v1/deployments[/{id}]` | GET, POST (`kind: workload | host`) | Controller |
| `/api/v1/deployments/{id}/approvals` | POST | **Executor** (approval socket in M4; signed, through the API, in M6) |
| `/api/v1/deployments/{id}/acknowledgements` | POST | Executor |
| `/api/v1/deployments/{id}/confirmation` | POST | Executor |
| `/api/v1/deployments/{id}/resolution` | POST | Executor |
| `/api/v1/operations/{id}` | GET | Controller, agent |

Implemented so far (M3 slice 5a): `GET /api/v1/nodes`,
`GET /api/v1/nodes/{node}/configuration` and
`POST /api/v1/nodes/{node}/configuration/evaluate`, on the controller's
socket ([host configuration](../administration/host-deployment.md)).

`/api/v1/config` and `/api/v1/config/plan` (workloads) are unchanged. The
agent's `/api/v1/host` (the Incus context) becomes `/api/v1/storage/pools`,
`/api/v1/storage/volumes` and `/api/v1/networks` in M4 slice 1.

CLI: `strata host status | plan | build | history | show | approve |
acknowledge | confirm | revert | reboot | rollback | resolve |
inspect-activation`.

## 15. Records: what lives where

| Information | Git (desired) | Records (files; PostgreSQL in M5) | Executor journal | Agent (observed) |
|---|---|---|---|---|
| Repository, ref, commit, tree, signer | Source; policy (repository, ref) in the running configuration | ✓ | Eligibility decision | Revision of the running system |
| `flake.lock` | Source | Blob ID | ✓ | — |
| Strata version | `flake.lock` | Controller's; target manifest's | ✓ | Running manifest's |
| Node | `strata.node.id` | ✓ | ✓ | ✓ |
| Previous and target generations | — | ✓ | ✓ (authoritative) | Profile, current, booted |
| Build result | — | ✓ | — | — |
| Activation, revert, persistence, reboot results | — | Mirrored | ✓ (authoritative) | — |
| Health per level | Policy (required units) | ✓ | Levels 1–3 | Raw facts |
| Initiator, approver, acknowledger, confirmer, resolver | — | ✓ | ✓ (peer credentials) | — |
| Policies | ✓ | Copied per record | Policy digest | — |

PostgreSQL (M5) holds history only, never desired configuration.

## 16. Pinned-source verification record

Checked on 2026-11-26 against **`NixOS/nixpkgs` revision
`2786a2a72088ecf32aea5b3ec52099e6a7cff78f`** (`.version` 26.11), as locked
in Strata's `flake.lock`. Re-check on every nixpkgs update; the ADRs
depending on these facts name this section.

| Claim | Source (path in nixpkgs) | Detail |
|---|---|---|
| `switch-to-configuration` modes and their effects | `pkgs/by-name/sw/switch-to-configuration-ng/src/main.rs`: `enum Action`; `usage()`; `do_system_switch()` | `boot` installs the boot loader and exits; `test` activates without the boot loader; `switch` does both. The Perl implementation is removed (`nixos/modules/system/activation/switchable-system.nix`, `mkRemovedOptionModule [ "system" "switch" "enableNg" ]`). |
| Lock | Same file, `do_system_switch()` | `Flock::lock(..., LockExclusiveNonblock)` on `/run/nixos/switch-to-configuration.lock` |
| Exit codes | Same file | `100`: init-interface version differs; `2`: the `activate` script failed; `4`: unit failures |
| Unit actions only for units with a configuration file | Same file, the loop over active units (`current_base_unit_file.exists()`; `X-StopOnRemoval`, `X-RestartIfChanged`, `X-StopIfChanged`, `X-OnlyManualStart`) | Transient units without a file in `/etc/systemd/system` are not acted on |
| systemd re-execution | Same file (`restart_systemd`: `/proc/1/exe` or `system.conf` differ; `systemd.reexecute()`) | Survival of transient units across re-execution is systemd behaviour: **not verified here** |
| Boot entries from profile generations; default by toplevel | `nixos/modules/system/boot/loader/systemd-boot/systemd-boot-builder.py`: `get_generations()`, `main()` (`default_config`) | No matching generation: `default nixos-*` |
| Boot counting option | `nixos/modules/system/boot/loader/systemd-boot/systemd-boot.nix`: `boot.loader.systemd-boot.bootCounting.{enable,tries}` (`tries` default 3) | — |
| Counters and `preferred` | `systemd-boot-builder.py`: `BootFile.from_entry()` (new entries get `+<tries>`); `write_loader_conf()` (`preferred <entry>` then `default nixos-*`; comment: needs systemd-boot ≥ 260) | Pinned `pkgs.systemd.version` is 262. The version on each host's ESP is **not verified here**. |
| Blessing | `nixos/modules/system/boot/systemd.nix`: `systemd-bless-boot.service`, `boot-complete.target` in the upstream units; `systemd-bless-boot.restartIfChanged = false` | — |
| Boot counting tests | `nixos/tests/systemd-boot.nix`: `bootCounting` (`systemd-boot-counting`), `bootCountingSpecialisation`, `defaultEntryWithBootCounting`, `garbageCollectEntryWithBootCounting` | Fallback to the **newest entry not marked bad**; the test reboots explicitly (`machine.reboot()`). A failed boot doesn't reboot by itself. Uses `nix-env -p /nix/var/nix/profiles/system --set` before `switch-to-configuration boot`. |
| GC roots for the running and booted systems | `nixos/modules/system/activation/activation-script.nix` (tmpfiles `L+ /nix/var/nix/gcroots/current-system`, when `nix.enable`); `nixos/modules/config/nix.nix` (`L+ /nix/var/nix/gcroots/booted-system`) | — |
| Incus soft restart | `virtualisation.incus.softDaemonRestart` default `true` (evaluated at this revision) | Instances keeping running across a daemon restart: **to validate on hardware** |
| `nixos-rebuild` runs activation in a transient unit | `pkgs/by-name/ni/nixos-rebuild-ng/src/nixos_rebuild/nix.py` (`--unit=nixos-rebuild-switch-to-configuration`) | Precedent for running steps in transient units |

**Not verifiable from nixpkgs** (Nix, Git, systemd, kernel or firmware
behaviour), to be established by tests and spikes:

- Profile generations being GC roots.
- `nix-env --set` always creating a new generation.
- `nix-store --realise --add-root` being race-free against a running GC.
- Transient timers surviving `systemctl daemon-reexec`.
- `transfer.fsckObjects` rejecting tampered objects on a local fetch.
- `nix flake archive` not evaluating flake outputs.
- `SO_PEERPIDFD` and the logind session properties.
- FAT write behaviour on interruption.
- Firmware honouring boot entries and counters.

## 17. Decision register

**Settled:**

- A separate host lifecycle subsystem; host state in a separate,
  versioned manifest, not the workload IR.
- The agent stays unprivileged; root work only in `strata-host exec`, with
  a closed operation set.
- Separate operations: evaluate, build, plan, activate, reboot, verify,
  roll back. Git rollback and generation rollback are distinct.
- Test, verify, then persist. Reboots are approved separately.
- Approvals: local peer credentials, sent directly to the executor (M4);
  signed remote approvals (M6). TTL 30 minutes by default, configurable up
  to 24 hours.
- Eligibility from root-controlled policy: the executor's own fetch and
  mirror, the ref, forward-only against its own anchor, node; explicit
  flags for initial and rollback deployments; the anchor never moves
  backwards.
- An isolated evaluation identity for provenance; `strata-controller` is
  not in nix `trusted-users`.
- No target code runs during default planning; `dry-activate` only as an
  explicit, audited, approved operation.
- Three plan categories: verified facts, inferred risks, controller
  report. A separate workload acknowledgement bound to the report's
  digest and timestamp.
- A revert timer from the previous system, in PID 1, pre-empting
  activation; `revert-failed` and `unknown` outcomes with operator
  resolution.
- GC roots per deployment, previous system and last known good.
- A hash-chained journal with torn-tail handling, three step states and
  a bounded state lock.
- Storage policy (§13). Boot recovery only with a valid, unstale,
  hardware-based attestation.
- Pool undeclaration refused. Input fetching deferred to M4, with the M3
  limitation stated.

**Open questions** (to settle in the slice named):

1. Whether `strata-host-fetch` uses the controller's deploy key or its
   own (recommended: its own read-only key). M4 slice 1.
2. The exact Strata health checks ordered before `boot-complete.target`.
   M4 slice 4.
3. The maximum age of a workload report (proposed 10 minutes). M4 slice 2.
4. How much of `switch-to-configuration`'s unit logic the static inference
   mirrors, and how divergence is detected (proposed: a VM test comparing
   inference with the real `dry-activate` output of test configurations).
   M4 slice 1.
5. The exact contents of the attestation fingerprint, and their stability
   across reboots on hardware. M4 slice 4.
6. Whether `inspect-activation` is offered at all in M4 (proposed: yes,
   off by default). M4 slice 2.

**Deferred:**

- Automatic host activation (none in M4).
- Maintenance windows, workload-aware multi-node maintenance and
  migration (M6).
- Signed remote approvals (M6).
- Boot recovery hardening and automatic recovery policies (M7).
- One-shot boot through `bootctl` (only if boot counting fails on
  hardware).
- PostgreSQL history (M5). Pool retirement automation (not planned).
  Webhooks (M5).

## 18. Hardware validation required

None of these are claimed until they pass on physical hardware. A VM test
is not counted.

- The revert timer when a management change breaks connectivity
  (hv-02), including a change of the systemd package (re-execution).
- The SSH confirmation spike criteria (§9.4) on hv-02.
- Boot counting class A on hv-02: a deliberately unbootable entry with
  `panic=`; a target failing `boot-complete.target` with a reboot; fallback
  to the previous system; the ESP's systemd-boot version ≥ 260.
- Class B only if a watchdog is present and claimed.
- The attestation fingerprint staying stable across ordinary reboots.
- Incus soft restart with running VMs.
- GC roots surviving `nix-collect-garbage -d`, and the boot entry check
  before a reboot.
- Host evaluation and build time and memory (M3 slice 5).
- Class C is not tested deliberately (it would risk the machine); it stays
  console-only.

## 19. Recommended CLAUDE.md changes

CLAUDE.md is the project owner's specification; these are proposals.

1. **§9:** "Final outcomes (healthy, degraded, reverted, revert-failed,
   boot-fallback, not-started, closure-missing, unknown) are a result
   field of SUCCEEDED or FAILED, not states."
2. **§6.1.2:** "Activation uses test, verify and persist; the boot default
   changes only after verification." "The privileged executor establishes
   deployment eligibility (signature, repository and ref, forward-only,
   node) and verifies approvals itself; components that request operations
   can't approve them."
3. **§6.1.4:** distinguish boot-loader success, `boot-complete.target`,
   Strata health verification and workload health.
4. **§6.1.5:** "Workload impact reported by the controller is labelled as
   unverified; operators acknowledge the exact report shown."
5. **§12:** under `/nodes`: `configuration`, `system`,
   `system/generations`, `system/health`, `builds`, `plan`; under
   `/deployments`: `approvals`, `acknowledgements`, `confirmation`,
   `resolution`.
6. **§17:** "Do not run target-provided code as root during planning."
7. **§19 M3:** "… evaluated and built independently of activation
   (commits whose inputs are already on the host; fetching new inputs is
   M4)."
8. **§19 M4:** "Host deployment slices come before storage and container
   work."
9. **§12, host resources:** CLAUDE.md v0.3.0 lists `/hosts`,
   `/hosts/{id}/configuration`, `/hosts/{id}/generations` and
   `/hosts/{id}/deployments` as proposed resources. The accepted structure
   (ADR-0027, §14) keeps host resources under `/nodes`: one node resource
   per machine, with `configuration`, `system`, `system/generations`,
   `system/health`, `builds` and `plan` beneath it, and host deployments in
   the shared `/deployments` collection (`kind: host`). Recommended
   wording: replace the four `/hosts` lines with
   `/nodes/{id}/configuration`, `/nodes/{id}/system`,
   `/nodes/{id}/system/generations` and `/deployments?kind=host`. M3
   slice 5 implements `/nodes`, `/nodes/{id}/configuration` and
   `/nodes/{id}/configuration/evaluate`.
