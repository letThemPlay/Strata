# Guest networks

Workloads connect to **guest networks**: Incus bridges declared in the host
configuration and created by `strata-incus-setup.service` (ADR-0019).
Options: [options.md](options.md#guest-networks).

A guest network is bridged to an **uplink**, a host NIC that is never the
management interface, either untagged or as a VLAN on it, or it is
**isolated** (guests on this host only). The host has **no address** on
guest networks, so guests have no path to the host's management services
(SSH, node exporter, the Incus API): management isolation holds by
construction, not only by firewall rules.

## Declaring networks

1. Find the NICs (`ip -br link`, `networkctl list`). The management
   interface (`strata.networking.management.interface`) cannot be an
   uplink.

   **Names can change when hardware changes.** PCI-path names (`enp3s0`)
   include the bus number: adding a drive renamed `enp3s0` to `enp4s0` on a
   test machine (onboard `eno1` kept its name). Check `ip -br link` after
   adding or moving hardware and before rebuilding; a missing uplink is
   refused (`uplink X does not exist`).
2. Decide per uplink:
   - **Untagged** (an access port, or the native VLAN of a trunk): exactly
     one network uses the NIC.
   - **VLANs** (a trunk port): one network per VLAN ID; none untagged.
3. Add them to `/etc/nixos/host.nix`:

   ```nix
   strata.networking.guest = {
     defaultNetwork = "lan";               # eth0 of new instances
     networks = {
       lan.uplink = "enp3s0";              # untagged
       dmz = { uplink = "enp4s0"; vlan = 20; };
       web = { uplink = "enp4s0"; vlan = 30; };
       lab = { };                          # isolated
     };
   };
   ```

   Network names become bridge names: at most 15 characters, letters,
   digits, `-` and `_`. For a VLAN network, `<uplink>.<vlan>` (the VLAN
   interface Incus creates) must also fit in 15 characters.

4. Apply: `sudo nixos-rebuild switch --flake /etc/nixos#$(hostname)`, then
   check:

   ```sh
   systemctl status strata-incus-setup   # failed = a network was refused
   sudo incus network list
   bridge link show                      # uplinks and guests attached to bridges
   ip -br addr                           # no address on uplinks or bridges
   ```

Guests get addresses from your network (your DHCP server or static
configuration); the host runs no DHCP or DNS on guest networks.

## What the host does

- Uplinks are brought up by systemd-networkd with no address of any kind
  (no DHCP, no IPv6 link-local or router advertisements); the same applies
  to the VLAN interfaces Incus creates on them.
- Guest bridges have `ipv4.address=none` and `ipv6.address=none`: no NAT, no
  DHCP, no host address.
- The host firewall opens nothing on guest networks. Guest bridges are only
  exempt from the reverse-path filter, which would otherwise drop bridged
  guest traffic.

## VLANs and trunks

Guests choose a network, not a VLAN tag. Incus lets an instance set a VLAN
tag on its NIC, but on an untagged network the bridge only forwards the
uplink's own (untagged) traffic: the `nixos-networks` test proves that a
guest tagging VLAN 20 on an untagged network does not reach VLAN 20 on the
trunk. Still, prefer access ports for untagged uplinks, and give each VLAN
its own network.

## When a network is refused

| Message | Meaning and what to do |
|---|---|
| `uplink X is the management interface` | Choose another NIC. (Also refused at evaluation.) |
| `uplink X does not exist` | Check the name (`ip -br link`); names can change with firmware or slot changes. |
| `uplink X has IP addresses (...)` | Something else configures the NIC. Remove that configuration: the host must not be on guest networks. |
| `a host interface or a non-bridge network of this name exists` | Rename the network. |

## Changing and removing networks

- **Changes are applied**: when a declaration changes (another uplink or
  VLAN), `strata-incus-setup` updates the network and logs the old and new
  values. Guests on it move with it.
- **Removing a network never deletes it.** It stays open to the Strata
  project while instances or the default profile use it (`strata-incus-setup`
  logs a warning), and is closed once they are gone. Delete it yourself
  with `sudo incus network delete NAME` when it is no longer needed.

## Not yet supported

NAT or routed guest networks (the host would need an address, DHCP/DNS and
forwarding rules that keep guests away from the management network), bonds,
and VLAN or bonded management interfaces are planned for later slices.
