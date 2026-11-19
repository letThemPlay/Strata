# NixOS VM test: a Strata host with the agent enabled boots, publishes its
# identity and a hardware report, and the agent runs unprivileged.
{ self, pkgs }:
pkgs.testers.runNixOSTest {
  name = "strata-agent";

  # The node name must equal strata.node.id: the test framework also sets
  # networking.hostName (to the node name) at mkDefault priority.
  nodes.hv01 = {
    imports = [ self.nixosModules.default ];

    virtualisation.cores = 2;

    strata = {
      enable = true;
      node.id = "hv01";
      agent.enable = true;
    };
  };

  testScript = ''
    import json

    hv01.wait_for_unit("strata-agent.service")
    hv01.wait_for_file("/var/lib/strata/hardware.json")

    with subtest("node identity"):
        node = json.loads(hv01.succeed("cat /etc/strata/node.json"))
        assert node == {
            "apiVersion": "node.strata.dev/v1alpha1",
            "id": "hv01",
            "role": "hypervisor",
        }, node
        hostname = hv01.succeed("hostname").strip()
        assert hostname == "hv01", hostname

    with subtest("hardware report"):
        report = json.loads(hv01.succeed("cat /var/lib/strata/hardware.json"))
        assert report["apiVersion"] == "hardware.strata.dev/v1alpha1", report
        nproc = int(hv01.succeed("nproc").strip())
        assert report["cpu"]["logicalCount"] == nproc, (report["cpu"], nproc)
        assert report["memory"]["totalBytes"] > 0, report["memory"]
        names = [n["name"] for n in report["networkInterfaces"]]
        assert "eth1" in names, names
        assert "lo" not in names, names

    with subtest("discover command matches report schema"):
        out = json.loads(hv01.succeed("strata-agent discover"))
        assert out["apiVersion"] == report["apiVersion"]

    with subtest("agent is unprivileged"):
        pid = hv01.succeed("systemctl show -p MainPID --value strata-agent").strip()
        uid = int(hv01.succeed(f"ps -o uid= -p {pid}").strip())
        assert uid != 0, f"agent runs as uid {uid}"
        caps = hv01.succeed(f"grep CapEff /proc/{pid}/status").split()[1]
        assert int(caps, 16) == 0, f"agent has effective capabilities {caps}"

    with subtest("structured logs"):
        hv01.succeed("journalctl -u strata-agent -o cat | grep -F '\"msg\":\"agent started\"'")

    with subtest("reload re-runs discovery"):
        hv01.succeed("rm /var/lib/strata/hardware.json")
        hv01.succeed("systemctl reload strata-agent")
        hv01.wait_for_file("/var/lib/strata/hardware.json")
        hv01.succeed("systemctl is-active strata-agent")

    with subtest("graceful shutdown"):
        hv01.succeed("systemctl stop strata-agent")
        result = hv01.succeed("systemctl show -p Result --value strata-agent").strip()
        assert result == "success", result
        hv01.succeed("journalctl -u strata-agent -o cat | grep -F '\"msg\":\"agent stopping\"'")
  '';
}
