# Configuration from Git

**Status: experimental, M3.** The Strata controller follows a Git
repository with the host's configuration, verifies each new commit,
evaluates the workloads it declares, shows the plan to bring the host to
them and applies it **once an operator approves it**. Every plan and every
operator action is recorded. VMs that are not declared stay managed with
`strata vm` ([VMs](vms.md)).

## How it works

1. Every `interval` seconds (and on `strata config sync`) the controller
   fetches the tracked branch, read only, into its own bare repository.
2. A new head commit must be **signed by one of `trustedSigners`**, and
   once a commit has been applied, later commits must **descend from it**
   (a rewound or force-pushed branch is refused). A commit that fails
   either check is **never evaluated**.
3. The commit's `nixosConfigurations.<node>.config.strata.ir`
   ([workloads](workloads.md)) is evaluated **in a separate service without
   network**, offline, in pure mode and without import-from-derivation:
   evaluation cannot download or build anything.
4. The IR is validated and planned against the host as the agent reports it
   (see [how declarations are applied](workloads.md#how-declarations-are-applied)).
   An unchanged commit is planned again on every synchronization, so
   changes on the host (drift) show up without a new commit.
5. A plan with changes is **recorded as a deployment awaiting approval**.
   A newer plan (a new commit, or a change on the host) replaces it: the
   old record is marked `CANCELLED` and can no longer be approved.
6. `strata config apply --plan ID` approves it. The controller checks the
   commit again (signature, descent), plans again and **applies only if the
   plan is unchanged**; otherwise nothing is applied and the deployment
   fails ("plan and approve again"). It applies the changes through the
   agent, then plans once more: the deployment **succeeds only if the host
   now matches the configuration** (apart from items that cannot be
   applied, which stay in every plan).

The controller never writes to the repository.

Deployment states: `AWAITING_APPROVAL` → `PENDING` → `VALIDATING` (commit
checks) → `PLANNING` (plan unchanged?) → `APPLYING` → `VERIFYING` →
`SUCCEEDED`, `FAILED` or `CANCELLED`. One deployment runs at a time.

## Setting it up

The configuration repository is the host flake: the repository
`strata-install` created in `/etc/nixos`, pushed to a Git server. The
controller evaluates `nixosConfigurations.<node id>` from it.

**1. A deploy key** (read only) for the controller, on the host:

```sh
sudo install -d -m 700 /etc/strata
sudo ssh-keygen -t ed25519 -N '' -C "strata-controller@hv-01" -f /etc/strata/gitops-key
sudo cat /etc/strata/gitops-key.pub   # add as a read-only deploy key on the Git server
ssh-keyscan git.example.org           # the server's host key, for knownHosts
```

The key stays on the host, outside the Nix store; systemd passes it to the
controller (`LoadCredential`). For an `https://` repository, put an access
token in a file instead (`httpsTokenFile`).

**2. Signed commits.** Sign the commits you push with an SSH key:

```sh
git config gpg.format ssh
git config user.signingkey ~/.ssh/id_ed25519.pub
git config commit.gpgsign true
```

**3. The options**, in `host.nix`:

```nix
strata.gitops = {
  enable = true;
  repository = "ssh://git@git.example.org/infra/hv-01.git";
  sshKeyFile = "/etc/strata/gitops-key";
  knownHosts = [ "git.example.org ssh-ed25519 AAAA..." ];   # from ssh-keyscan
  trustedSigners = [ "ssh-ed25519 AAAA... alice" ];          # your signing key, named
  # autoApply = "safe";      # apply safe plans without approval
  # keepDeployments = 100;   # prune old records (default: keep all)
};
```

Then `nixos-rebuild switch` and `strata config sync`. See
[options](options.md#gitops) for all of them.

**Host changes are still applied with `nixos-rebuild`.** The controller
only plans and applies workloads. A commit that changes `flake.lock` (or
anything else the evaluation needs that is not on the host yet) is
reported as a host change: apply it with `nixos-rebuild` first. The
flake's inputs (Strata, nixpkgs, disko) are kept in the store while the
system generation exists, so `nix-collect-garbage` does not break
evaluation.

## Commands

Members of `strata-operators` use the controller through
`/run/strata/controller.sock`; every request is audited in its journal,
and approvals are recorded with the approver's uid.

```console
$ strata config status
Repository:   ssh://git@git.example.org/infra/hv-01.git (branch main)
Node:         hv-01
Head:         9c1e2a7b6d4c10...
Evaluated:    9c1e2a7b6d4c10... (signed by alice)
Last applied: 51d0e3f8a2c7b9...
Last sync:    2026-11-25T09:30:00Z
Auto-apply:   none
Plan:         77178b16e02503ac: 2 items (restarts VMs)
Awaiting:     deployment 20261125T093000Z-3fa1c2, approve with: strata config apply --plan 77178b16e02503ac

$ strata config sync      # fetch, verify, evaluate and plan now
$ strata config plan      # the current plan, item by item
$ strata config apply --plan 77178b16e02503ac
...
Apply this plan? [y/N] y
Deployment 20261125T093000Z-3fa1c2: PENDING
  VALIDATING
  PLANNING
  APPLYING
  VERIFYING
  SUCCEEDED

Deployment 20261125T093000Z-3fa1c2 (plan): SUCCEEDED
Repository: ssh://git@git.example.org/infra/hv-01.git (branch main), node hv-01
Commit:     9c1e2a7b6d4c10... (signed by alice), flake.lock c16112c7...
...
$ strata config history   # deployments and operator actions, newest first
$ strata config show 20261125T093000Z-3fa1c2
```

`--json` prints the status, plan, history or a record as JSON. `apply`
shows the plan and asks first; `--yes` skips the question.

### Deleting VMs

A VM managed through Git that is removed from the configuration is
deleted only when the plan is approved with **`--allow-delete`**. Its root
disk is deleted; **data volumes are kept** (listed in the result), and
are not reused by a new VM of that name until M4 brings volume management.
A **protected** VM (`lifecycle.protected = true`) is never deleted or
restarted: set the flag to false, apply, then remove it.

### Automatic application

With `autoApply = "safe"`, a plan that restarts, deletes and restores
nothing and has no item that cannot be applied is applied without
approval (recorded as approved by `auto`). Other plans wait for approval
as usual. The default, `"none"`, applies nothing without approval.

### Rolling back: pin and unpin

```console
$ strata config pin 51d0e3f     # evaluate this commit instead of the branch
$ strata config plan            # review, then apply it as usual
$ strata config apply --plan ...
$ strata config unpin           # follow the branch again
```

A pinned commit must be **signed by a trusted key** and be in the fetched
history; it **may be older** than the last applied commit (that is the
point). While pinned, the branch is still fetched but not planned. To
unpin, the branch head must descend from the last applied commit: usually
push a fix (or a revert) on top of the branch first. Pinning changes no
repository.

### Adopting a VM created with `strata vm`

A declared VM whose name is taken by a VM created with `strata vm` cannot
be applied (the plan says so). `strata config adopt NAME` moves it to Git:
it must be declared in the evaluated commit and not drifted. Only its
owner changes; the next plan shows any difference from the declaration.

### Deployment records

Records are JSON files in `/var/lib/strata-controller/deployments`
(readable only by the controller; `strata config history` and `show`
print them). Each records the repository, branch, node, commit and signer,
the `flake.lock` blob, the Strata version, the plan, who approved it, every
state change with its time, the result of each item and any error. By
default all records are kept; `keepDeployments = N` keeps the newest `N`
finished ones (records awaiting approval or in progress are never pruned).

If the controller stops during a deployment (a reboot), the record is
marked `FAILED` ("interrupted") when it starts again; a plan awaiting
approval is `CANCELLED`, and a new plan is recorded on the next
synchronization. The last applied and pinned commits are kept in
`state.json` next to the records.

## Troubleshooting

| Message | Meaning |
|---|---|
| `refused: commit ... not signed by a trusted key` | The head commit is unsigned or signed by a key not in `trustedSigners`. It was not evaluated. Push a signed commit. |
| `does not descend from the last applied commit` | The branch was rewound or force-pushed. Push a commit on top of the last applied one, or pin a commit. |
| `changes flake.lock: the host configuration changed; apply it with nixos-rebuild first` | The commit needs inputs that are not on the host. Rebuild the host from it. |
| `the commit's flake inputs are not on this host` | As above, for the first commit the controller evaluates. |
| `evaluation failed: ... is not declared in strata.storage.pools` | The configuration's assertions failed; the message is Nix's. |
| `no plan with this ID awaiting approval` | The plan was replaced (a new commit or a change on the host). Review `strata config plan` and approve the current one. |
| `the plan deletes VMs (their root disks; data volumes are kept): add --allow-delete` | See [deleting VMs](#deleting-vms). |
| `the node or the configuration changed since plan ... was made` | The host changed between approval and applying. Nothing was applied; approve the new plan. |
| `a deployment is in progress` | Wait for it (`strata config status`). |
| `vm ... was changed outside Strata; revert that first` | Adoption refuses a drifted VM. |
| `fetch: git fetch: ... Host key verification failed` | `knownHosts` does not match the server's host key. |
| `fetch: ... Permission denied (publickey)` | The deploy key is not registered on the Git server. |
| `observe the node: ...` | The agent is not reachable (`systemctl status strata-agent`). |
