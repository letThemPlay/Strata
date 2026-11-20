# NixOS options

Only options listed here are implemented and tested. Everything is
**experimental** until the first release.

The easiest start is the installer ([installation](../installation/README.md)),
which generates the host configuration from the host template. To start
from the template by hand (machine-specific values go in `host.nix`):

```sh
mkdir hv-01 && cd hv-01
nix flake init -t github:letThemPlay/Strata#host
```

Written by hand, a host flake looks like this:

```nix
{
  inputs = {
    strata.url = "github:letThemPlay/Strata";
    nixpkgs.follows = "strata/nixpkgs";
  };

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
            compute.enable = true;
            access = {
              enable = true;
              admin.authorizedKeys = [ "ssh-ed25519 AAAA... admin@workstation" ];
            };
            networking = {
              enable = true;
              management = {
                interface = "eno1";
                addresses = [ "192.0.2.10/24" ];
                gateway4 = "192.0.2.1";
                dns = [ "192.0.2.53" ];
              };
            };
            monitoring.enable = true;
            disk = {
              enable = true;
              device = "/dev/disk/by-id/nvme-EXAMPLE";
            };
          };
          system.stateVersion = "26.05";
        }
      ];
    };
  };
}
```

Every `strata.<module>.enable` below requires `strata.enable`.

## Base

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.enable` | bool | `false` | Enable Strata on this host. Installs the Strata binaries and writes `/etc/strata/node.json`, installs Git and enables Nix flakes (host configurations are flakes, ADR-0013). |
| `strata.package` | package | flake's `strata` package | Package providing the binaries. Set automatically by `nixosModules.default`. |
| `strata.node.id` | null or RFC 1123 label | `null` | Stable node identifier: lowercase letters, digits and `-`, 1–63 characters, not starting or ending with `-`. Required. Becomes the default `networking.hostName`. |
| `strata.node.role` | `"hypervisor"` | `"hypervisor"` | Node role. No other roles exist yet. |

## Agent

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.agent.enable` | bool | `false` | Run `strata-agent.service` (hardware discovery). Unprivileged, no network access. |

## Compute

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.compute.enable` | bool | `false` | Enable libvirtd with host-architecture QEMU (`qemu_kvm`). QEMU runs as the unprivileged `qemu-libvirtd` user, swtpm is available, and guests are shut down cleanly when the host shuts down. |

KVM modules are not configured: udev loads `kvm-intel` or `kvm-amd` from the
CPU's features. libvirt's built-in `default` NAT network is defined by NixOS
but never started; guest networking will be managed by Strata (M2).

## Access

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.access.enable` | bool | `false` | Enable key-only SSH for the admin account. |
| `strata.access.admin.name` | POSIX user name, not `root` | `"strata-admin"` | Administrator account (member of `wheel`, password locked). |
| `strata.access.admin.authorizedKeys` | list of OpenSSH public keys | `[ ]` | Keys allowed to log in as the admin. At least one is required. |

sshd is configured with `PasswordAuthentication no`,
`KbdInteractiveAuthentication no`, `PermitRootLogin no`, `X11Forwarding no`
and `AllowUsers <admin>` (override with `mkForce` if more users need SSH).

> **Warning:** the admin has passwordless sudo, because password
> authentication is disabled. Its SSH keys are therefore root-equivalent.
> Protect them accordingly (hardware keys via `sk-ssh-ed25519@openssh.com`
> are supported).

## Networking

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.networking.enable` | bool | `false` | Manage the management interface with systemd-networkd and enable the nftables firewall. |
| `strata.networking.management.interface` | interface name | `null` | Management interface. Required. |
| `strata.networking.management.addressing` | `"static"` or `"dhcp"` | `"static"` | Addressing mode. |
| `strata.networking.management.addresses` | list of CIDR | `[ ]` | Static addresses. Required with `static`; not allowed with `dhcp`. |
| `strata.networking.management.gateway4` / `gateway6` | null or address | `null` | Default gateways (static only). |
| `strata.networking.management.dns` | list of addresses | `[ ]` | DNS servers (static only). |

Firewall model:

- Nothing is open on any interface by default.
- SSH (with `strata.access`) and the node exporter (with `strata.monitoring`)
  are opened **only on the management interface**.
- Opening ports globally (`networking.firewall.allowedTCPPorts` etc.) is
  rejected by an assertion. Use `networking.firewall.interfaces.<if>`.

### Recovering from a bad network change

Strata does not yet roll back network changes automatically (planned with
the M3 executor). Until then:

- Try changes with `nixos-rebuild test`: they are not made the boot default,
  so a reboot restores the previous configuration.
- Every `nixos-rebuild switch` creates a generation; select an older one in
  the boot menu, or run `nixos-rebuild switch --rollback` from the console.
- Keep out-of-band console access (IPMI/BMC or physical) for hypervisors.

## Disk

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.disk.enable` | bool | `false` | Partition the system disk with disko and boot with systemd-boot (UEFI only). |
| `strata.disk.device` | null or path under `/dev/` | `null` | Disk to install on. Required. Prefer `/dev/disk/by-id/...`; it is used only when formatting. |
| `strata.disk.layout` | `"ext4"` | `"ext4"` | Partition layout; see [disk layout](../installation/disk-layout.md). |

With `strata.disk` enabled, do not declare `fileSystems."/"` or
`fileSystems."/boot"` yourself (generate hardware configuration with
`nixos-generate-config --no-filesystems`); conflicting definitions fail
evaluation. systemd-boot keeps at most 10 generations
(`boot.loader.systemd-boot.configurationLimit`, overridable).

> **Warning:** formatting destroys all data on `strata.disk.device`.

## Monitoring

| Option | Type | Default | Description |
|---|---|---|---|
| `strata.monitoring.enable` | bool | `false` | Run the Prometheus node exporter (with the `systemd` collector). Requires `strata.networking.enable`. |
| `strata.monitoring.nodeExporter.port` | port | `9100` | Exporter port, opened on the management interface only. |

## Files and services

| Path / unit | Content |
|---|---|
| `/etc/strata/node.json` | `{"apiVersion": "node.strata.dev/v1alpha1", "id": ..., "role": ...}` |
| `strata-agent.service` | Discovers hardware at start, every minute, and on `systemctl reload`. Changes are logged at info level. |
| `/var/lib/strata/hardware.json` | Hardware report, `apiVersion: hardware.strata.dev/v1alpha1`. Root-readable only. |
| `/etc/systemd/network/10-strata-management.network` | Management interface configuration. |

## Tests

- `checks.<system>.module-eval`: option validation, assertions and generated configuration.
- `checks.<system>.nixos-agent`: VM test of the agent.
- `checks.<system>.nixos-host`: three-node VM test of a full host (SSH, sudo, firewall isolation, node exporter, libvirt, KVM).
- `checks.<system>.reference-host`: builds the reference host (the `host` template plus test values).
- `checks.x86_64-linux.nixos-install-ext4`: formats an empty disk with the `ext4` layout, installs the reference host and boots it under UEFI.
- `checks.x86_64-linux.nixos-installer`: installs a host with the installer ISO, offline, and boots it.
