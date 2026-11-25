# strata.gitops: the Strata controller (M3, ADR-0025). It synchronizes the
# host's configuration repository, verifies commit signatures, evaluates
# the IR of `nixosConfigurations.<node>.config.strata.ir` offline (in a
# separate service without network) and plans
# it against the node through the agent. In M3 slice 3a it only plans;
# applying arrives in slice 3b.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  inherit (lib) mkOption types;
  cfg = config.strata;
  g = cfg.gitops;
  isSSH = lib.hasPrefix "ssh://" g.repository;
  inStore = p: p != null && lib.hasPrefix "/nix/store" (toString p);

  knownHosts = pkgs.writeText "strata-gitops-known-hosts" (lib.concatLines g.knownHosts);
  settings = {
    node = cfg.node.id;
    inherit (g) repository branch trustedSigners;
    intervalSeconds = g.interval;
    autoApply = "none";
    evalTimeoutSeconds = g.evalTimeout;
  }
  // lib.optionalAttrs isSSH {
    knownHostsFile = "${knownHosts}";
    sshKey = "gitops-ssh-key";
  }
  // lib.optionalAttrs (g.httpsTokenFile != null) { token = "gitops-token"; };
  controllerConfig = pkgs.writeText "strata-controller.json" (builtins.toJSON g.settings);
in
{
  options.strata.gitops = {
    enable = lib.mkEnableOption "the Strata controller: declarative workloads from a Git repository";

    repository = mkOption {
      type = types.strMatching "(https|ssh)://[^ ]+";
      example = "ssh://git@git.example.org/infra/hv-01.git";
      description = "The host's configuration repository (https:// or ssh://; read only).";
    };
    branch = mkOption {
      type = types.strMatching "[A-Za-z0-9][A-Za-z0-9._/-]{0,199}";
      default = "main";
      description = "Branch to track.";
    };
    sshKeyFile = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "/etc/strata/gitops-key";
      description = ''
        Private SSH key (a read-only deploy key) for an ssh:// repository.
        A file on the host, outside the Nix store; passed to the controller
        with systemd's LoadCredential.
      '';
    };
    knownHosts = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [ "git.example.org ssh-ed25519 AAAA..." ];
      description = "known_hosts lines pinning the SSH host key of the repository's server (required for ssh://).";
    };
    httpsTokenFile = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "File with an access token for an https:// repository (outside the Nix store; LoadCredential).";
    };
    trustedSigners = mkOption {
      type = types.listOf (
        types.strMatching "(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh.com|sk-ecdsa-sha2-nistp256@openssh.com) [A-Za-z0-9+/]+=*( .*)?"
      );
      example = [ "ssh-ed25519 AAAA... alice" ];
      description = ''
        SSH public keys ("TYPE KEY [NAME]") that sign commits. Only a commit
        signed by one of them is evaluated (git commit -S with
        gpg.format=ssh).
      '';
    };
    interval = mkOption {
      type = types.ints.between 10 86400;
      default = 300;
      description = "Seconds between synchronizations (`strata config sync` synchronizes at once).";
    };
    evalTimeout = mkOption {
      type = types.ints.between 10 3600;
      default = 300;
      description = "Seconds one evaluation of the configuration may take.";
    };
    settings = mkOption {
      type = types.attrs;
      internal = true;
      readOnly = true;
      default = settings;
      description = "The controller's configuration (for tests).";
    };
  };

  config = lib.mkIf g.enable {
    assertions = [
      {
        assertion = cfg.agent.enable && cfg.compute.enable;
        message = "strata.gitops requires strata.agent.enable and strata.compute.enable.";
      }
      {
        assertion = g.trustedSigners != [ ];
        message = "strata.gitops.trustedSigners: at least one key is required; unsigned commits are never evaluated.";
      }
      {
        assertion = !isSSH || (g.sshKeyFile != null && g.knownHosts != [ ]);
        message = "strata.gitops: an ssh:// repository needs sshKeyFile and knownHosts (the server's host key).";
      }
      {
        assertion = isSSH -> g.httpsTokenFile == null;
        message = "strata.gitops.httpsTokenFile is for https:// repositories.";
      }
      {
        assertion = !isSSH -> g.sshKeyFile == null;
        message = "strata.gitops.sshKeyFile is for ssh:// repositories.";
      }
      {
        assertion = !(inStore g.sshKeyFile) && !(inStore g.httpsTokenFile);
        message = "strata.gitops: credential files must not be in the Nix store (it is world-readable).";
      }
    ];

    users.users.strata-controller = {
      isSystemUser = true;
      group = "strata-controller";
      # Reaches the agent's API socket.
      extraGroups = [ "strata-operators" ];
      home = "/var/lib/strata-controller";
    };
    users.groups.strata-controller = { };

    systemd = {
      sockets.strata-controller = {
        description = "Strata controller local API";
        wantedBy = [ "sockets.target" ];
        socketConfig = {
          ListenStream = "/run/strata/controller.sock";
          SocketUser = "root";
          SocketGroup = "strata-operators";
          SocketMode = "0660";
          DirectoryMode = "0755";
          Service = "strata-controller.service";
        };
      };

      # Evaluation runs in its own service per request (socket activation),
      # with no network: whatever the configuration contains, Nix cannot
      # download anything (ADR-0025). Only the controller may connect.
      sockets.strata-controller-eval = {
        description = "Strata controller evaluation service";
        wantedBy = [ "sockets.target" ];
        socketConfig = {
          ListenStream = "/run/strata/controller-eval.sock";
          SocketUser = "strata-controller";
          SocketGroup = "strata-controller";
          SocketMode = "0600";
          DirectoryMode = "0755";
          Accept = true;
          MaxConnections = 2;
        };
      };

      services."strata-controller-eval@" = {
        description = "Strata controller evaluation (one commit)";
        after = [ "nix-daemon.socket" ];
        path = [
          config.nix.package
          pkgs.git
        ];
        serviceConfig = {
          User = "strata-controller";
          Group = "strata-controller";
          ExecStart = "${lib.getExe' cfg.package "strata-controller"} eval-service --state-dir /var/lib/strata-controller --cache-dir /var/cache/strata-controller-eval --timeout ${toString g.evalTimeout}";
          StandardInput = "socket";
          StandardOutput = "socket";
          StandardError = "journal";
          CacheDirectory = "strata-controller-eval";
          CacheDirectoryMode = "0700";
          UMask = "0077";
          RuntimeMaxSec = g.evalTimeout + 60;
          # Evaluating a NixOS configuration needs memory; bound it.
          MemoryMax = "4G";
          TasksMax = 64;

          PrivateNetwork = true;
          IPAddressDeny = "any";
          # The nix-daemon socket only.
          RestrictAddressFamilies = [ "AF_UNIX" ];
          # The repository is read only.
          ProtectSystem = "strict";
          ProtectHome = true;
          PrivateTmp = true;
          PrivateDevices = true;
          CapabilityBoundingSet = "";
          AmbientCapabilities = "";
          NoNewPrivileges = true;
          ProtectKernelTunables = true;
          ProtectKernelModules = true;
          ProtectKernelLogs = true;
          ProtectControlGroups = true;
          ProtectClock = true;
          ProtectHostname = true;
          ProtectProc = "invisible";
          RestrictNamespaces = true;
          RestrictRealtime = true;
          RestrictSUIDSGID = true;
          LockPersonality = true;
          SystemCallArchitectures = "native";
          SystemCallFilter = [
            "@system-service"
            "~@privileged"
            "~@resources"
          ];
        };
      };

      services.strata-controller = {
        description = "Strata controller (configuration from Git)";
        wantedBy = [ "multi-user.target" ];
        after = [
          "network-online.target"
          "strata-agent.socket"
          "strata-controller-eval.socket"
          "strata-controller.socket"
        ];
        wants = [
          "network-online.target"
          "strata-controller-eval.socket"
        ];
        requires = [ "strata-controller.socket" ];
        path = [
          pkgs.git
          pkgs.openssh
        ];
        serviceConfig = {
          Type = "simple";
          User = "strata-controller";
          Group = "strata-controller";
          ExecStart = "${lib.getExe' cfg.package "strata-controller"} run --config ${controllerConfig} --state-dir /var/lib/strata-controller --credentials %d";
          Restart = "on-failure";
          RestartSec = 10;
          Sockets = [ "strata-controller.socket" ];
          StateDirectory = "strata-controller";
          StateDirectoryMode = "0700";
          UMask = "0077";
          LoadCredential =
            lib.optional (g.sshKeyFile != null) "gitops-ssh-key:${g.sshKeyFile}"
            ++ lib.optional (g.httpsTokenFile != null) "gitops-token:${g.httpsTokenFile}";

          MemoryMax = "1G";
          TasksMax = 64;

          CapabilityBoundingSet = "";
          AmbientCapabilities = "";
          NoNewPrivileges = true;
          # Git over https/ssh; the agent's and the evaluation service's sockets.
          RestrictAddressFamilies = [
            "AF_UNIX"
            "AF_INET"
            "AF_INET6"
          ];
          ProtectSystem = "strict";
          ProtectHome = true;
          PrivateTmp = true;
          PrivateDevices = true;
          ProtectKernelTunables = true;
          ProtectKernelModules = true;
          ProtectKernelLogs = true;
          ProtectControlGroups = true;
          ProtectClock = true;
          ProtectHostname = true;
          ProtectProc = "invisible";
          RestrictNamespaces = true;
          RestrictRealtime = true;
          RestrictSUIDSGID = true;
          LockPersonality = true;
          SystemCallArchitectures = "native";
          SystemCallFilter = [
            "@system-service"
            "~@privileged"
            "~@resources"
          ];
        };
      };
    };
  };
}
