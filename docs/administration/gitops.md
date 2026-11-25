# Configuration from Git

**Status: experimental, M3 slice 3a.** The Strata controller follows a Git
repository with the host's configuration, verifies each new commit,
evaluates the workloads it declares and shows the plan to bring the host to
them. **It does not apply plans yet**: approval, applying, deployment
records, pinning and adoption arrive in slice 3b. Until then, manage VMs
with `strata vm` ([VMs](vms.md)).

## How it works

1. Every `interval` seconds (and on `strata config sync`) the controller
   fetches the tracked branch, read only, into its own bare repository.
2. A new head commit must be **signed by one of `trustedSigners`**; once a
   commit has been applied (slice 3b), later commits must descend from it.
   A commit that fails either check is **never evaluated**.
3. The commit's `nixosConfigurations.<node>.config.strata.ir`
   ([workloads](workloads.md)) is evaluated **in a separate service without
   network**, offline, in pure mode and without import-from-derivation:
   evaluation cannot download or build anything.
4. The IR is validated and planned against the host as the agent reports it
   (see [how declarations are applied](workloads.md#how-declarations-will-be-applied)).
   An unchanged commit is planned again on every synchronization, so
   changes on the host (drift) show up without a new commit.

The controller never writes to the repository.

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
};
```

Then `nixos-rebuild switch` and `strata config sync`.

**Host changes are still applied with `nixos-rebuild`.** The controller
only plans workloads. A commit that changes `flake.lock` (or anything else
the evaluation needs that is not on the host yet) is reported as a host
change: apply it with `nixos-rebuild` first. The flake's inputs (Strata,
nixpkgs, disko) are kept in the store while the system generation exists,
so `nix-collect-garbage` does not break evaluation.

## Commands

Members of `strata-operators` use the controller through
`/run/strata/controller.sock`; every request is audited in its journal.

```console
$ strata config status
Repository:  ssh://git@git.example.org/infra/hv-01.git (branch main)
Node:        hv-01
Head:        9c1e2a7b6d4c10...
Evaluated:   9c1e2a7b6d4c10... (signed by alice)
Last applied: (none)
Last sync:   2026-11-25T09:30:00Z
Plan:        77178b16e02503ac: 2 items (restarts VMs)

$ strata config sync      # fetch, verify, evaluate and plan now
$ strata config plan      # the current plan, item by item
```

`--json` prints the status or plan as JSON.

## Troubleshooting

| Message | Meaning |
|---|---|
| `refused: commit ... not signed by a trusted key` | The head commit is unsigned or signed by a key not in `trustedSigners`. It was not evaluated. Push a signed commit. |
| `does not descend from the last applied commit` | The branch was rewound or force-pushed (slice 3b). |
| `changes flake.lock: the host configuration changed; apply it with nixos-rebuild first` | The commit needs inputs that are not on the host. Rebuild the host from it. |
| `the commit's flake inputs are not on this host` | As above, for the first commit the controller evaluates. |
| `evaluation failed: ... is not declared in strata.storage.pools` | The configuration's assertions failed; the message is Nix's. |
| `fetch: git fetch: ... Host key verification failed` | `knownHosts` does not match the server's host key. |
| `fetch: ... Permission denied (publickey)` | The deploy key is not registered on the Git server. |
| `observe the node: ...` | The agent is not reachable (`systemctl status strata-agent`). |
