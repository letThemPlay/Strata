# NixOS options

Only options listed here are implemented and tested. Everything is
**experimental** until the first release.

Import the module through the flake:

```nix
{
  inputs.strata.url = "github:letThemPlay/Strata";

  outputs = { nixpkgs, strata, ... }: {
    nixosConfigurations.hv-01 = nixpkgs.lib.nixosSystem {
      modules = [
        strata.nixosModules.default
        ./hardware-configuration.nix
        {
          strata = {
            enable = true;
            node.id = "hv-01";
            agent.enable = true;
          };
        }
      ];
    };
  };
}
```

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.enable` | bool | `false` | Enable Strata on this host. Installs the Strata binaries and writes `/etc/strata/node.json`. |
| `strata.package` | package | flake's `strata` package | Package providing the binaries. Set automatically by `nixosModules.default`. |
| `strata.node.id` | null or RFC 1123 label | `null` | Stable node identifier: lowercase letters, digits and `-`, 1–63 characters, not starting or ending with `-`. Required when `strata.enable` is set. Becomes the default `networking.hostName`. |
| `strata.node.role` | `"hypervisor"` | `"hypervisor"` | Node role. No other roles exist yet. |
| `strata.agent.enable` | bool | `false` | Run `strata-agent.service`. Requires `strata.enable`. |

## Files and services

| Path / unit | Content |
|---|---|
| `/etc/strata/node.json` | `{"apiVersion": "node.strata.dev/v1alpha1", "id": ..., "role": ...}` |
| `strata-agent.service` | Discovers hardware at start and on `systemctl reload`. Unprivileged, no network access. |
| `/var/lib/strata/hardware.json` | Hardware report, `apiVersion: hardware.strata.dev/v1alpha1`. Root-readable only. |

`strata-agent discover` prints the same report to stdout without writing
anything.

## Tests

- `checks.<system>.module-eval`: option validation and assertions.
- `checks.<system>.nixos-agent`: VM test of a host with the agent enabled.
