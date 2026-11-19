# NixOS VM test: a fully configured Strata host (compute, access,
# networking, monitoring, agent) on a management network (VLAN 1) and a
# non-management network (VLAN 2).
#
#   admin    (VLAN 1, 192.168.1.1)  ── management ──  hv01 eth1 (192.168.1.2)
#   outsider (VLAN 2, 192.168.2.3)  ── other ───────  hv01 eth2 (192.168.2.2)
#
# Node numbers (and so the framework-assigned addresses) follow the sorted
# node names: admin=1, hv01=2, outsider=3.
{ self, pkgs }:
let
  keys = import "${pkgs.path}/nixos/tests/ssh-keys.nix" pkgs;
in
pkgs.testers.runNixOSTest {
  name = "strata-host";

  nodes = {
    hv01 = {
      imports = [ self.nixosModules.default ];

      virtualisation = {
        vlans = [
          1
          2
        ];
        cores = 2;
        memorySize = 2048;
      };

      strata = {
        enable = true;
        node.id = "hv01";
        agent.enable = true;
        compute.enable = true;
        access = {
          enable = true;
          admin.authorizedKeys = [ keys.snakeOilEd25519PublicKey ];
        };
        networking = {
          enable = true;
          management = {
            interface = "eth1";
            addresses = [ "192.168.1.2/24" ];
          };
        };
        monitoring.enable = true;
      };

      # Root holds the same key so that a refused root login proves
      # PermitRootLogin/AllowUsers, not merely a missing key.
      users.users.root.openssh.authorizedKeys.keys = [ keys.snakeOilEd25519PublicKey ];
    };

    admin =
      { pkgs, ... }:
      {
        virtualisation.vlans = [ 1 ];
        environment.systemPackages = [
          pkgs.curl
          pkgs.netcat
        ];
      };

    outsider =
      { pkgs, ... }:
      {
        virtualisation.vlans = [ 2 ];
        environment.systemPackages = [ pkgs.netcat ];
      };
  };

  testScript = ''
    ssh = "ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=5"
    mgmt = "192.168.1.2"
    other = "192.168.2.2"

    start_all()
    hv01.wait_for_unit("multi-user.target")
    hv01.wait_for_unit("sshd.service")
    hv01.wait_for_unit("prometheus-node-exporter.service")
    hv01.wait_for_unit("strata-agent.service")
    admin.wait_for_unit("multi-user.target")
    outsider.wait_for_unit("multi-user.target")

    admin.succeed(
        "install -D -m 600 ${keys.snakeOilEd25519PrivateKey} /root/.ssh/id_ed25519"
    )

    with subtest("management interface is configured by the strata networkd unit"):
        status = hv01.succeed("networkctl status eth1")
        assert "10-strata-management.network" in status, status
        assert "192.168.1.2" in status, status
        admin.wait_until_succeeds(f"ping -c 1 -W 2 {mgmt}")

    with subtest("admin logs in with a key and can use sudo"):
        out = admin.succeed(f"{ssh} strata-admin@{mgmt} 'sudo -n id -un'").strip()
        assert out == "root", out

    with subtest("password and keyboard-interactive authentication are not offered"):
        err = admin.fail(
            f"{ssh} -o PubkeyAuthentication=no strata-admin@{mgmt} true 2>&1"
        )
        assert "Permission denied (publickey)" in err, err

    with subtest("root login is refused even with an authorized key"):
        admin.fail(f"{ssh} root@{mgmt} true")

    with subtest("node exporter is reachable on the management network"):
        metrics = admin.succeed(f"curl -sf http://{mgmt}:9100/metrics")
        assert "node_boot_time_seconds" in metrics
        assert "node_systemd_unit_state" in metrics

    with subtest("management services are not reachable from other networks"):
        outsider.wait_until_succeeds(f"ping -c 1 -W 2 {other}")  # path works
        outsider.fail(f"nc -z -w 3 {other} 22")
        outsider.fail(f"nc -z -w 3 {other} 9100")
        admin.succeed(f"nc -z -w 3 {mgmt} 22")  # positive control

    with subtest("libvirt runs unprivileged QEMU and no libvirt network is active"):
        hv01.wait_for_unit("libvirtd.service")
        hv01.succeed("virsh -c qemu:///system version")
        hv01.succeed("grep -Fx 'user = \"qemu-libvirtd\"' /var/lib/libvirt/qemu.conf")
        # NixOS defines libvirt's "default" NAT network but does not
        # autostart it. Guest networking is managed by Strata (M2), so no
        # libvirt network may be active or autostarted, and virbr0 must not exist.
        active = hv01.succeed("virsh -c qemu:///system net-list --name").strip()
        assert active == "", f"active libvirt networks: {active}"
        autostart = hv01.succeed("virsh -c qemu:///system net-list --all --autostart --name").strip()
        assert autostart == "", f"autostarted libvirt networks: {autostart}"
        hv01.fail("ip link show virbr0")

    with subtest("KVM is available when the CPU exposes virtualization"):
        ext = hv01.succeed("grep -oEm1 '\\b(vmx|svm)\\b' /proc/cpuinfo || true").strip()
        if ext:
            hv01.succeed("test -c /dev/kvm")
            hv01.succeed("virsh -c qemu:///system domcapabilities --virttype kvm > /dev/null")
            hv01.succeed("grep -q '\"kvmDevice\": true' /var/lib/strata/hardware.json")
            print(f"KVM check ran ({ext})")
        else:
            print("SKIPPED KVM check: no vmx/svm in the test VM (no nested virtualization)")
  '';
}
