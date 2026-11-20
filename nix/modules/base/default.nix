# Core Strata options: enablement, package and node identity.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.strata;
  inherit (lib)
    mkEnableOption
    mkIf
    mkOption
    types
    ;

  # RFC 1123 hostname label: lowercase alphanumerics and '-', 1-63 chars,
  # not starting or ending with '-'. The node ID doubles as the hostname.
  nodeId = types.strMatching "[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?" // {
    description = "RFC 1123 hostname label (lowercase, 1-63 characters)";
  };
in
{
  options.strata = {
    enable = mkEnableOption "the Strata virtualization platform on this host";

    package = mkOption {
      type = types.package;
      description = ''
        Package providing the Strata binaries. Set automatically when the
        module is imported through the flake's `nixosModules.default`.
      '';
    };

    node = {
      id = mkOption {
        type = types.nullOr nodeId;
        default = null;
        example = "hv-01";
        description = ''
          Stable identifier of this node. Required when `strata.enable` is
          set. Also used as the default hostname.
        '';
      };

      role = mkOption {
        type = types.enum [ "hypervisor" ];
        default = "hypervisor";
        description = "Role of this node. Only `hypervisor` is supported.";
      };
    };
  };

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.node.id != null;
        message = "strata.node.id must be set when strata.enable is true.";
      }
    ];

    networking.hostName = mkIf (cfg.node.id != null) (lib.mkDefault cfg.node.id);

    environment.etc."strata/node.json".text = builtins.toJSON {
      apiVersion = "node.strata.dev/v1alpha1";
      inherit (cfg.node) id role;
    };

    environment.systemPackages = [
      cfg.package
      # The host configuration in /etc/nixos is a Git repository (ADR-0013).
      pkgs.git
    ];

    # Host configurations are flakes (ADR-0013); enable flakes so they can be
    # evaluated and rebuilt on the host itself.
    nix.settings.experimental-features = [
      "nix-command"
      "flakes"
    ];
  };
}
