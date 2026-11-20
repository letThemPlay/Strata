# System disk: partitioning (disko) and boot loader (ADR-0012).
#
# The layout is declared, not scripted: disko generates both the
# formatting script used by the installer and the `fileSystems` of the
# installed system from the same description, so they cannot drift apart.
#
# Requires the disko NixOS module, which the flake's `nixosModules.default`
# imports.
{
  config,
  lib,
  ...
}:
let
  cfg = config.strata;

  layouts = {
    ext4 = import ../../../installer/partitioning/ext4.nix;
  };
in
{
  options.strata.disk = {
    enable = lib.mkEnableOption "the Strata-managed system disk layout and boot loader";

    device = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "/dev/.+");
      default = null;
      example = "/dev/disk/by-id/nvme-Samsung_SSD_980_1TB_S64ANS0T123456";
      description = ''
        The disk to install on. Prefer a stable `/dev/disk/by-id/` path:
        `/dev/sdX` names can change between boots and installations.
        The installed system mounts its filesystems by partition label, so
        this path is used only when formatting.

        Formatting destroys all data on this disk.
      '';
    };

    layout = lib.mkOption {
      type = lib.types.enum (lib.attrNames layouts);
      default = "ext4";
      description = ''
        Partition layout. `ext4`: 1 GiB EFI system partition on `/boot`
        and an ext4 root filesystem on the rest of the disk.
      '';
    };
  };

  config = lib.mkIf cfg.disk.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.disk.enable requires strata.enable.";
      }
      {
        assertion = cfg.disk.device != null;
        message = "strata.disk.device must be set when strata.disk.enable is true.";
      }
    ];

    disko.devices = lib.mkIf (cfg.disk.device != null) (
      layouts.${cfg.disk.layout} { inherit (cfg.disk) device; }
    );

    # UEFI only (ADR-0012).
    boot.loader = {
      systemd-boot = {
        enable = true;
        # Bound the number of generations on the 1 GiB ESP.
        configurationLimit = lib.mkDefault 10;
      };
      # Register the boot entry in the firmware during installation.
      efi.canTouchEfiVariables = lib.mkDefault true;
      grub.enable = false;
    };
  };
}
