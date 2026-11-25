# NixOS VM test: the Strata controller synchronizes a host's configuration
# repository and plans it (M3 slice 3a, ADR-0025).
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
  hostV2 = hostNix "host-v2.nix" "{ ${vm "web-01"} ${vm "web-02"} }";
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
        assert f"Evaluated:   {v1} (signed by alice)" in out, out
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
        assert "Plan:        (none)" in out, out
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

    with subtest("requests are audited; nothing failed"):
        uid = hv01.succeed("id -u strata-admin").strip()
        audit = hv01.succeed("journalctl -u strata-controller -o cat | grep -F '\"msg\":\"audit\"'")
        assert f'"uid":{uid}' in audit and '"path":"/api/v1/config/sync"' in audit, audit
        failed = hv01.succeed("systemctl list-units --failed --plain --no-legend").strip()
        assert failed == "", failed
  '';
}
