# Evaluation tests for the Strata NixOS modules: option validation,
# assertions and generated configuration. No VM is built.
{
  self,
  nixpkgs,
  pkgs,
  lib,
}:
let
  eval =
    modules:
    nixpkgs.lib.nixosSystem {
      modules = [
        self.nixosModules.default
        {
          nixpkgs.hostPlatform = pkgs.stdenv.hostPlatform.system;
          boot.loader.grub.enable = false;
          fileSystems."/" = {
            device = "/dev/null";
            fsType = "ext4";
          };
          system.stateVersion = "26.05";
        }
      ]
      ++ modules;
    };

  failedAssertions = sys: map (a: a.message) (lib.filter (a: !a.assertion) sys.config.assertions);
  hasFailure = needle: sys: lib.any (lib.hasInfix needle) (failedAssertions sys);
  throws = v: !(builtins.tryEval (builtins.deepSeq v true)).success;

  valid = eval [
    {
      strata = {
        enable = true;
        node.id = "hv-01";
        agent.enable = true;
      };
    }
  ];
  disabled = eval [ ];
  withNode = id: eval [ { strata.node.id = id; } ];
  agentService = valid.config.systemd.services.strata-agent.serviceConfig;

  cases = {
    "valid config has no failed assertions" = failedAssertions valid == [ ];
    "node.id becomes the hostname" = valid.config.networking.hostName == "hv-01";
    "node.json contains identity" =
      builtins.fromJSON valid.config.environment.etc."strata/node.json".text == {
        apiVersion = "node.strata.dev/v1alpha1";
        id = "hv-01";
        role = "hypervisor";
      };
    "agent runs as a dynamic user without capabilities or network" =
      agentService.DynamicUser
      && agentService.CapabilityBoundingSet == ""
      && agentService.RestrictAddressFamilies == [ "AF_UNIX" ]
      && agentService.IPAddressDeny == "any";
    "disabled strata changes nothing" =
      !(disabled.config.systemd.services ? strata-agent)
      && !(disabled.config.environment.etc ? "strata/node.json");
    "missing node.id fails an assertion" = hasFailure "strata.node.id must be set" (eval [
      { strata.enable = true; }
    ]);
    "agent without strata.enable fails an assertion" =
      hasFailure "strata.agent.enable requires strata.enable"
        (eval [
          {
            strata = {
              node.id = "hv-01";
              agent.enable = true;
            };
          }
        ]);
    "node.id accepts a 63-character label" =
      !(throws (withNode (lib.strings.replicate 63 "a")).config.strata.node.id);
    "node.id rejects uppercase and underscores" = throws (withNode "Bad_ID").config.strata.node.id;
    "node.id rejects a leading hyphen" = throws (withNode "-hv").config.strata.node.id;
    "node.id rejects 64 characters" = throws (withNode (lib.strings.replicate 64 "a"))
      .config.strata.node.id;
    "node.role rejects unknown roles" =
      throws
        (eval [ { strata.node.role = "storage"; } ]).config.strata.node.role;
  };

  failed = lib.attrNames (lib.filterAttrs (_: ok: !ok) cases);
in
assert lib.assertMsg (failed == [ ]) (
  "Strata module evaluation tests failed:\n  - " + lib.concatStringsSep "\n  - " failed
);
pkgs.writeText "strata-module-eval-results" (
  lib.concatMapStringsSep "\n" (name: "PASS ${name}") (lib.attrNames cases) + "\n"
)
