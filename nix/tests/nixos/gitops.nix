# NixOS VM test: the Strata controller synchronizes a host's configuration
# repository, plans it and applies approved plans (M3 slice 3, ADR-0025,
# ADR-0026).
#
#   git  - sshd; the repository /srv/git/hv01.git, served to the deploy key
#   hv01 - Strata host with a dir pool, a guest network and strata.gitops
#
# The repository holds a host flake whose flake.lock pins this Strata
# source (as the installer's does), so it evaluates offline on hv01: its
# inputs are in hv01's store, kept by strata.gitops.
#
# Test-only keys from nixpkgs: the ed25519 key is both the deploy key and
# the trusted signing key; the ECDSA key is the server's host key.
{ self, pkgs }:
let
  inherit (pkgs) lib;
  keys = import (pkgs.path + "/nixos/tests/ssh-keys.nix") pkgs;
  system = pkgs.stdenv.hostPlatform.system;
  hostLock = import ../../../installer/iso/host-lock.nix { inherit lib self; };

  # The configuration repository's files.
  flake = pkgs.writeText "flake.nix" ''
    {
      inputs = {
        strata.url = "github:letThemPlay/Strata";
        nixpkgs.follows = "strata/nixpkgs";
      };
      outputs = { nixpkgs, strata, ... }: {
        nixosConfigurations.hv01 = nixpkgs.lib.nixosSystem {
          modules = [ strata.nixosModules.default ./host.nix ];
        };
      };
    }
  '';
  lock = pkgs.writeText "flake.lock" (builtins.toJSON hostLock);
  # host.nix with the given workloads (a Nix expression).
  hostNix =
    name: workloads:
    pkgs.writeText name ''
      {
        nixpkgs.hostPlatform = "${system}";
        boot.loader.grub.enable = false;
        fileSystems."/" = { device = "/dev/null"; fsType = "ext4"; };
        fileSystems."/srv/vmpool" = { device = "/dev/disk/by-id/virtio-vmpool"; fsType = "ext4"; };
        system.stateVersion = "26.05";
        strata = {
          enable = true;
          node.id = "hv01";
          agent.enable = true;
          compute.enable = true;
          storage = {
            defaultPool = "vmpool";
            pools.vmpool = { driver = "dir"; path = "/srv/vmpool"; };
          };
          networking = {
            enable = true;
            management = { interface = "eth1"; addresses = [ "192.168.1.2/24" ]; };
            guest.networks.lan.uplink = "eth2";
          };
          workloads.vms = ${workloads};
        };
      }
    '';
  vm = name: ''
    ${name} = {
      resources = { cpu = 1; memory = "256MiB"; };
      disks.root.size = "1GiB";
      networks.eth0.network = "lan";
    };
  '';
  hostV1 = hostNix "host-v1.nix" "{ ${vm "web-01"} }";
  # web-01 (cpu, extra settings), web-02 with a data disk, cli-vm as
  # `strata vm` creates it in the test, web-03.
  web01 = cpu: extra: ''
    web-01 = {
      resources = { cpu = ${toString cpu}; memory = "256MiB"; };
      disks.root.size = "1GiB";
      networks.eth0.network = "lan";
      ${extra}
    };
  '';
  web02 = ''
    web-02 = {
      resources = { cpu = 1; memory = "256MiB"; };
      disks = { root.size = "1GiB"; data.size = "1GiB"; };
      networks.eth0.network = "lan";
    };
  '';
  cliVM = cpu: ''
    cli-vm = {
      resources = { cpu = ${toString cpu}; memory = "256MiB"; };
      disks.root.size = "1GiB";
    };
  '';
  protected = "lifecycle.protected = true;";
  hostV2 = hostNix "host-v2.nix" "{ ${web01 1 ""} ${web02} }";
  hostV3 = hostNix "host-v3.nix" "{ ${web01 2 ""} ${web02} }";
  hostV4 = hostNix "host-v4.nix" "{ ${web01 2 protected} ${web02} }";
  hostV5 = hostNix "host-v5.nix" "{ ${web01 2 protected} }";
  hostV6 = hostNix "host-v6.nix" "{ ${web01 2 protected} ${cliVM 1} }";
  hostV7 = hostNix "host-v7.nix" "{ ${web01 2 "${protected} autostart = true;"} ${cliVM 1} }";
  hostV8 = hostNix "host-v8.nix" "{ ${web01 2 "${protected} autostart = true;"} ${cliVM 2} }";
  hostV9 = hostNix "host-v9.nix" "{ ${web01 2 "${protected} autostart = true;"} ${cliVM 2} ${vm "web-03"} }";
  hostBadPool = hostNix "host-bad.nix" ''{ web-01 = { resources = { cpu = 1; memory = "256MiB"; }; disks.root = { size = "1GiB"; pool = "nopool"; }; }; }'';
  hostThrows = pkgs.writeText "host-throws.nix" ''throw "this commit was evaluated"'';

  # The key without its comment, named alice.
  signerPublic =
    lib.concatStringsSep " " (lib.take 2 (lib.splitString " " keys.snakeOilEd25519PublicKey))
    + " alice";
in
pkgs.testers.runNixOSTest {
  name = "strata-gitops";

  nodes = {
    git =
      { pkgs, ... }:
      {
        virtualisation.vlans = [ 1 ];
        networking.interfaces.eth1.ipv4.addresses = [
          {
            address = "192.168.1.10";
            prefixLength = 24;
          }
        ];
        environment.systemPackages = [
          pkgs.git
          pkgs.openssh
          pkgs.jq
        ];
        environment.etc."ssh/test_host_key" = {
          source = keys.snakeOilPrivateKey;
          mode = "0600";
        };
        services.openssh = {
          enable = true;
          hostKeys = [
            {
              type = "ecdsa";
              path = "/etc/ssh/test_host_key";
            }
          ];
        };
        users.users.git = {
          isSystemUser = true;
          group = "git";
          home = "/var/lib/git";
          createHome = true;
          shell = "${pkgs.git}/bin/git-shell";
          openssh.authorizedKeys.keys = [ keys.snakeOilEd25519PublicKey ];
        };
        users.groups.git = { };
      };

    hv01 =
      { lib, pkgs, ... }:
      {
        imports = [ self.nixosModules.default ];
        environment.systemPackages = [
          pkgs.jq
          pkgs.git
        ];
        users.users.mallory.isNormalUser = true;
        virtualisation = {
          vlans = [
            1
            2
          ];
          cores = 2;
          memorySize = 4096;
          diskSize = 4096;
          emptyDiskImages = [
            {
              size = 4096;
              driveConfig.deviceExtraOpts.serial = "vmpool";
            }
          ];
          fileSystems."/srv/vmpool" = {
            device = "/dev/disk/by-id/virtio-vmpool";
            fsType = "ext4";
            autoFormat = true;
          };
        };
        networking.interfaces.eth2 = {
          ipv4.addresses = lib.mkForce [ ];
          ipv6.addresses = lib.mkForce [ ];
        };
        networking.hosts."192.168.1.10" = [ "git" ];
        # The deploy key, as an administrator would install it (root only).
        environment.etc."strata/gitops-key" = {
          source = keys.snakeOilEd25519PrivateKey;
          mode = "0600";
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
          storage = {
            defaultPool = "vmpool";
            pools.vmpool = {
              driver = "dir";
              path = "/srv/vmpool";
            };
          };
          networking = {
            enable = true;
            management = {
              interface = "eth1";
              addresses = [ "192.168.1.2/24" ];
            };
            guest.networks.lan.uplink = "eth2";
          };
          gitops = {
            enable = true;
            repository = "ssh://git@git/srv/git/hv01.git";
            sshKeyFile = "/etc/strata/gitops-key";
            knownHosts = [ "git ${keys.snakeOilPublicKey}" ];
            trustedSigners = [ signerPublic ];
            interval = 15;
          };
        };
        # The same host applying safe plans by itself, keeping 3 records.
        specialisation.auto.configuration.strata.gitops = {
          autoApply = "safe";
          keepDeployments = 3;
        };
      };
  };

  testScript = ''
    import json

    start_all()
    git.wait_for_unit("sshd.service")

    # The repository and a work tree, owned by the git user.
    def as_git(cmd):
        return git.succeed(f"runuser -u git -- env HOME=/var/lib/git GIT_CONFIG_NOSYSTEM=1 sh -c '{cmd}'")

    git.succeed(
        "mkdir -p /srv/git && chown git:git /srv/git",
        "install -o git -m 600 ${keys.snakeOilEd25519PrivateKey} /var/lib/git/signer",
    )
    as_git("git init --quiet --bare -b main /srv/git/hv01.git && git init --quiet -b main ~/work")
    as_git("git -C ~/work config user.name Alice && git -C ~/work config user.email alice@example.org"
           " && git -C ~/work config gpg.format ssh")

    def commit(files, key="/var/lib/git/signer", message="change"):
        """Commit files ({name: store path}) and push; return the commit ID."""
        for name, src in files.items():
            git.succeed(f"install -o git -m 644 {src} /var/lib/git/work/{name}")
        sign = f"-c user.signingkey={key} commit -S" if key else "commit"
        as_git(f"cd ~/work && git add -A && git {sign} --quiet --allow-empty -m {message} && git push --quiet /srv/git/hv01.git main")
        return as_git("git -C ~/work rev-parse HEAD").strip()

    hv01.wait_for_unit("strata-agent.service")
    hv01.wait_for_unit("strata-incus-setup.service")

    def config(cmd):
        return f"su strata-admin -c 'strata config {cmd}'"

    def status():
        return json.loads(hv01.succeed(config("status --json")))

    def journal_has(text):
        return hv01.execute(f"journalctl -u strata-controller -o cat | grep -qF '{text}'")[0] == 0

    with subtest("the controller runs unprivileged; its API is for strata-operators"):
        hv01.wait_for_unit("strata-controller.service")
        user = hv01.succeed("ps -o user= -C strata-controller").strip()
        assert user == "strata-controller", user
        assert hv01.succeed("stat -c '%a %U %G' /run/strata/controller.sock").strip() == "660 root strata-operators"
        out = hv01.fail("su mallory -c 'strata config status' 2>&1")
        assert "permission denied" in out, out
        # Evaluation runs in a separate service without network.
        assert hv01.succeed("systemctl show -p PrivateNetwork strata-controller-eval@x.service").strip() == "PrivateNetwork=yes"
        assert hv01.succeed("stat -c '%a %U' /run/strata/controller-eval.sock").strip() == "600 strata-controller"
        # Its credential is only readable by the service.
        hv01.succeed("test \"$(stat -c %a /var/lib/strata-controller)\" = 700")

    with subtest("a signed commit is verified, evaluated and planned; nothing is applied"):
        v1 = commit({"flake.nix": "${flake}", "flake.lock": "${lock}", "host.nix": "${hostV1}"}, message="v1")
        out = hv01.succeed(config("sync"))
        print(out)
        assert f"Evaluated:    {v1} (signed by alice)" in out, out
        assert "create          vm/web-01" in out, out
        st = status()
        assert st["head"] == v1 and st["evaluated"] == v1 and st["plan"]["items"] == 1 and st["plan"]["safe"], st
        plan = json.loads(hv01.succeed(config("plan --json")))
        assert plan["plan"]["items"][0]["resource"] == "vm/web-01", plan
        assert hv01.succeed("incus list --project strata --format csv").strip() == "", "the controller applied something"

    with subtest("the plan reflects the node: CLI VMs are not touched"):
        blank = '{"apiVersion":"vm.strata.dev/v1alpha1","name":"NAME","resources":{"cpu":1,"memory":"256MiB"},"disks":[{"name":"root","size":"1GiB","pool":"vmpool"}]}'
        for name in ["web-01", "manual"]:
            hv01.succeed(f"echo '{blank.replace('NAME', name)}' > /tmp/{name}.json && chmod 644 /tmp/{name}.json")
            hv01.succeed(f"su strata-admin -c 'strata vm create -f /tmp/{name}.json'")
        out = hv01.succeed(config("sync"))
        assert "unreconcilable  vm/web-01" in out and "created with `strata vm`" in out, out
        assert "Not managed through Git (created with strata vm): manual" in out, out
        for name in ["web-01", "manual"]:
            hv01.succeed(f"su strata-admin -c 'strata vm delete {name} --confirm {name}'")

    with subtest("an unsigned commit is refused and never evaluated"):
        unsigned = commit({"host.nix": "${hostThrows}"}, key=None, message="unsigned")
        out = hv01.fail(config("sync"))
        assert "not signed by a trusted key" in out and unsigned[:12] in out, out
        assert "Plan:         (none)" in out, out
        assert not journal_has("this commit was evaluated"), "an unsigned commit was evaluated"

    with subtest("a commit signed by another key is refused"):
        git.succeed("runuser -u git -- ssh-keygen -q -t ed25519 -N \"\" -f /var/lib/git/mallory")
        foreign = commit({"host.nix": "${hostThrows}"}, key="/var/lib/git/mallory", message="foreign")
        out = hv01.fail(config("sync"))
        assert "not signed by a trusted key" in out and foreign[:12] in out, out
        assert not journal_has("this commit was evaluated")

    with subtest("an invalid workload is reported with the assertion"):
        commit({"host.nix": "${hostBadPool}"}, message="bad-pool")
        out = hv01.fail(config("sync"))
        assert 'is not declared in strata.storage.pools' in out, out

    with subtest("a flake.lock change is reported as a host change"):
        commit({"host.nix": "${hostV1}"}, message="v1-again")
        hv01.succeed(config("sync"))
        git.succeed("jq '.nodes.disko.locked.narHash = \"sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"' ${lock} > /tmp/lock2")
        commit({"flake.lock": "/tmp/lock2"}, message="lock")
        out = hv01.fail(config("sync"))
        print(out)
        assert "changes flake.lock: the host configuration changed; apply it with nixos-rebuild first" in out, out
        # The evaluation service has no network: the download fails at once.
        assert "unable to download" in out and "Timeout was reached" not in out, out
        assert "remote: Counting" not in out, out
        commit({"flake.lock": "${lock}"}, message="lock-back")

    with subtest("the branch is synchronized periodically"):
        v2 = commit({"host.nix": "${hostV2}"}, message="v2")
        hv01.wait_until_succeeds(
            f"su strata-admin -c 'strata config status --json' | jq -e '.evaluated == \"{v2}\" and .plan.items == 2'",
            timeout=180,
        )
        out = hv01.succeed(config("plan"))
        assert "create          vm/web-01" in out and "create          vm/web-02" in out, out

    # ---- applying (slice 3b) ----

    def plan_id():
        return status()["plan"]["id"]

    def apply(*flags):
        out = hv01.succeed(config(f"apply --plan {plan_id()} --yes " + " ".join(flags)))
        print(out)
        assert "SUCCEEDED" in out, out
        return out

    def inst(name):
        return json.loads(hv01.succeed(f"incus query '/1.0/instances/{name}?project=strata'"))

    def force_stop(name):
        # Blank VMs (no OS) ignore the ACPI power button: stop them with
        # --force before changes that would otherwise restart them.
        hv01.succeed(f"su strata-admin -c 'strata vm stop {name} --force --timeout 0'")

    with subtest("approving a plan creates the VMs, owned by Git, and records it"):
        hv01.succeed(config("sync"))
        apply()
        for name in ["web-01", "web-02"]:
            i = inst(name)
            assert i["status"] == "Running" and i["config"]["user.strata.owner"] == "gitops", i["config"]
        hv01.succeed("incus storage volume show vmpool web-02-data --project strata")
        st = status()
        assert st["lastApplied"] == v2 and st["plan"]["converged"] and "awaiting" not in st, st
        history = json.loads(hv01.succeed(config("history --json")))
        rec = history[0]
        uid = hv01.succeed("id -u strata-admin").strip()
        assert rec["kind"] == "plan" and rec["state"] == "SUCCEEDED" and rec["commit"] == v2 and rec["approvedBy"] == f"uid:{uid}", rec
        d = json.loads(hv01.succeed(config(f"show {rec['id']} --json")))
        assert d["flakeLock"] and d["strataVersion"] and d["irGenerator"] and d["signer"] == "alice", d
        assert [t["state"] for t in d["transitions"]] == ["AWAITING_APPROVAL", "PENDING", "VALIDATING", "PLANNING", "APPLYING", "VERIFYING", "SUCCEEDED"], d
        assert d["result"]["converged"], d["result"]

    with subtest("strata vm cannot change Git-owned VMs, but may stop them"):
        out = hv01.fail("su strata-admin -c 'strata vm delete web-01 --confirm web-01 --stop' 2>&1")
        assert "managed by Git" in out, out
        force_stop("web-01")
        force_stop("web-02")
        assert "nothing to do" in hv01.succeed(config("sync")), "the power state is not declared"

    with subtest("an edit is applied as an update"):
        v3 = commit({"host.nix": "${hostV3}"}, message="v3")
        out = hv01.succeed(config("sync"))
        assert "update          vm/web-01" in out and "resources.cpu: 1 -> 2" in out, out
        apply()
        assert inst("web-01")["config"]["limits.cpu"] == "2"

    with subtest("a force-pushed branch is refused"):
        as_git(f"cd ~/work && git reset --quiet --hard {v2}")
        git.succeed("install -o git -m 644 ${hostV1} /var/lib/git/work/host.nix")
        as_git("cd ~/work && git add -A && git -c user.signingkey=/var/lib/git/signer commit -S --quiet -m rewrite"
               " && git push --quiet --force /srv/git/hv01.git main")
        rewritten = as_git("git -C ~/work rev-parse HEAD").strip()
        out = hv01.fail(config("sync"))
        assert "does not descend from the last applied commit" in out and rewritten[:12] in out, out
        assert not journal_has(f"evaluated\",\"commit\":\"{rewritten}")
        as_git(f"cd ~/work && git reset --quiet --hard {v3} && git push --quiet --force /srv/git/hv01.git main")
        hv01.succeed(config("sync"))

    with subtest("drift is shown and restored with approval"):
        hv01.succeed("incus config set web-02 limits.memory 512MiB --project strata")
        out = hv01.succeed(config("sync"))
        assert "restore         vm/web-02" in out and "Drift:        vm/web-02" in out, out
        apply()
        assert inst("web-02")["config"]["limits.memory"] == "256MiB"

    with subtest("a protected VM is not deleted; deleting needs --allow-delete and keeps data"):
        commit({"host.nix": "${hostV4}"}, message="v4")
        hv01.succeed(config("sync"))
        apply()
        assert inst("web-01")["config"]["user.strata.protected"] == "true"
        commit({"host.nix": "${hostV5}"}, message="v5")
        out = hv01.succeed(config("sync"))
        assert "delete          vm/web-02" in out and "keeps data volume vmpool/web-02-data" in out, out
        out = hv01.fail(config(f"apply --plan {plan_id()} --yes") + " 2>&1")
        assert "add --allow-delete" in out, out
        apply("--allow-delete")
        hv01.fail("incus query '/1.0/instances/web-02?project=strata'")
        hv01.succeed("incus storage volume show vmpool web-02-data --project strata")
        # web-01 is protected: removing it from the configuration would not delete it.
        assert inst("web-01")["status"] == "Stopped"

    with subtest("a VM created with strata vm is adopted"):
        hv01.succeed(
            "echo '{\"apiVersion\":\"vm.strata.dev/v1alpha1\",\"name\":\"cli-vm\",\"resources\":{\"cpu\":1,\"memory\":\"256MiB\"},"
            "\"disks\":[{\"name\":\"root\",\"size\":\"1GiB\",\"pool\":\"vmpool\"}]}' > /tmp/cli-vm.json && chmod 644 /tmp/cli-vm.json",
            "su strata-admin -c 'strata vm create -f /tmp/cli-vm.json'",
        )
        commit({"host.nix": "${hostV6}"}, message="v6")
        out = hv01.succeed(config("sync"))
        assert "unreconcilable  vm/cli-vm" in out, out
        hv01.succeed(config("adopt cli-vm"))
        assert inst("cli-vm")["config"]["user.strata.owner"] == "gitops"
        out = hv01.succeed(config("plan"))
        assert "vm/cli-vm" in out.split("In sync:")[1], out

    with subtest("pin rolls back to a signed commit; unpin follows the branch again"):
        v6 = as_git("git -C ~/work rev-parse HEAD").strip()
        v7 = commit({"host.nix": "${hostV7}"}, message="v7")
        hv01.succeed(config("sync"))
        apply()
        assert inst("web-01")["config"]["boot.autostart"] == "true"
        out = hv01.succeed(config(f"pin {v6[:10]}"))
        assert f"Pinned:       {v6}" in out, out
        apply()
        assert inst("web-01")["config"]["boot.autostart"] == "false"
        v8 = commit({"host.nix": "${hostV8}"}, message="v8")
        hv01.succeed(config("sync"))
        st = status()
        assert st["head"] == v8 and st["evaluated"] == v6 and st["lastApplied"] == v6, st
        hv01.succeed(config("unpin"))
        assert status()["evaluated"] == v8
        apply()
        assert inst("web-01")["config"]["boot.autostart"] == "true" and inst("cli-vm")["config"]["limits.cpu"] == "2"
        kinds = [d["kind"] for d in json.loads(hv01.succeed(config("history --json")))]
        assert "pin" in kinds and "unpin" in kinds and "adopt" in kinds, kinds

    with subtest("with autoApply = safe, safe plans apply by themselves; deletions wait"):
        base = hv01.succeed("readlink -f /run/current-system").strip()
        hv01.succeed(f"{base}/specialisation/auto/bin/switch-to-configuration test")
        hv01.wait_for_unit("strata-controller.service")
        commit({"host.nix": "${hostV9}"}, message="v9")
        hv01.wait_until_succeeds("incus query '/1.0/instances/web-03?project=strata' | jq -e '.config[\"user.strata.owner\"] == \"gitops\"'", timeout=180)
        # The VM exists before the deployment has verified it.
        hv01.wait_until_succeeds(config("history --json") + " | jq -e '.[0].approvedBy == \"auto\" and .[0].state == \"SUCCEEDED\"'", timeout=120)
        force_stop("web-03")
        commit({"host.nix": "${hostV8}"}, message="v10")  # removes web-03
        hv01.succeed(config("sync"))
        st = status()
        assert st["plan"]["deletes"] and st["awaiting"]["planId"] == st["plan"]["id"], st
        hv01.succeed("incus query '/1.0/instances/web-03?project=strata'")  # not deleted

    with subtest("old records are pruned to keepDeployments"):
        records = hv01.succeed("ls /var/lib/strata-controller/deployments").split()
        finished = [r for r in records if json.loads(hv01.succeed(f"cat /var/lib/strata-controller/deployments/{r}"))["state"] in ("SUCCEEDED", "FAILED", "CANCELLED")]
        assert len(finished) <= 3, records

    with subtest("after a reboot the state and history are kept"):
        last = status()["lastApplied"]
        hv01.shutdown()
        hv01.start()
        hv01.wait_for_unit("strata-controller.service")
        st = status()
        assert st["lastApplied"] == last and st.get("pinned") is None, st
        assert json.loads(hv01.succeed(config("history --json"))), "history lost"

    with subtest("requests are audited; nothing failed"):
        uid = hv01.succeed("id -u strata-admin").strip()
        audit = hv01.succeed("journalctl -u strata-controller -o cat | grep -F '\"msg\":\"audit\"'")
        assert f'"uid":{uid}' in audit and '"path":"/api/v1/config/sync"' in audit, audit
        failed = hv01.succeed("systemctl list-units --failed --plain --no-legend").strip()
        assert failed == "", failed
  '';
}
