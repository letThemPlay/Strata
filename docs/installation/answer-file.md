# strata-install answer file

**Schema:** `install.strata.dev/v1alpha1` (experimental; may change
incompatibly before the first release).

An answer file is a single JSON object describing one host.
`strata-install init` writes one interactively; you can also write it by
hand. `strata-install`
rejects unknown fields, so typos are errors rather than silently ignored
settings. Every problem is reported at once.

```json
{
  "apiVersion": "install.strata.dev/v1alpha1",
  "node": { "id": "hv-01" },
  "admin": {
    "name": "strata-admin",
    "authorizedKeys": ["ssh-ed25519 AAAAC3Nza... admin@workstation"]
  },
  "disk": { "device": "/dev/disk/by-id/nvme-Samsung_SSD_980_1TB_S64ANS0T123456", "layout": "ext4" },
  "network": {
    "interface": "eno1",
    "addressing": "static",
    "addresses": ["192.0.2.10/24"],
    "gateway4": "192.0.2.1",
    "dns": ["192.0.2.53"]
  }
}
```

| Field | Required | Description | NixOS option |
|---|---|---|---|
| `apiVersion` | yes | Must be `install.strata.dev/v1alpha1`. | |
| `node.id` | yes | Node ID and hostname: lowercase letters, digits and `-`, 1–63 characters, not starting or ending with `-`. | `strata.node.id` |
| `admin.name` | no | Admin account name. Default `strata-admin`; `root` is rejected. | `strata.access.admin.name` |
| `admin.authorizedKeys` | yes | One or more OpenSSH public keys (`ssh-ed25519`, `ssh-rsa`, `ecdsa-sha2-nistp*`, `sk-*@openssh.com`), one line each, without `authorized_keys` options. The key data must be valid base64 of a key of the named type. | `strata.access.admin.authorizedKeys` |
| `admin.consolePasswordHash` | no | crypt(3) hash of a password for local console login (recovery); SSH never accepts passwords. Create it with `mkpasswd -m yescrypt` (yescrypt, bcrypt, SHA-512 and similar are accepted; plain passwords are rejected). The installer writes it to `/etc/strata/admin-console-password` (root-only) on the host, not into the configuration. `strata-install init` asks for the password and stores only the hash. | `strata.access.admin.consolePasswordFile` |
| `disk.device` | yes | The disk to erase and install on. Use a `/dev/disk/by-id/` path. | `strata.disk.device` |
| `disk.layout` | no | Default and only value: `ext4`. See [disk layout](disk-layout.md). | `strata.disk.layout` |
| `network.interface` | yes | Management interface name (`ip link`). | `strata.networking.management.interface` |
| `network.addressing` | yes | `static` or `dhcp`. | `strata.networking.management.addressing` |
| `network.addresses` | static only | Addresses with prefix length, IPv4 or IPv6. At least one with `static`. | `…management.addresses` |
| `network.gateway4` | no, static only | IPv4 default gateway. | `…management.gateway4` |
| `network.gateway6` | no, static only | IPv6 default gateway. | `…management.gateway6` |
| `network.dns` | no, static only | DNS server addresses. | `…management.dns` |

The installer writes these values into the host configuration's `host.nix`;
the answer file itself is not kept. Values are validated against the same
rules as the NixOS options (stricter where the options are looser, such as
device paths), and the generated configuration is then evaluated and built
before the disk is touched.

`strata-install plan` additionally checks the file against the machine: the
disk must exist, be a whole disk of at least 16 GiB, and have nothing
mounted, used as swap or built on it (LVM, RAID); the interface must exist
and have a permanent MAC address. The generated `host.nix` pins the
interface's name to that address (`strata.networking.interfaces.<name>.macAddress`),
so adding hardware later cannot rename it.
It lists existing data on the disk (filesystems, inactive ZFS, LVM or RAID
members, as found by udev), which installing destroys.

Treat an answer file with a console password hash like a password file:
the hash can be attacked offline.
