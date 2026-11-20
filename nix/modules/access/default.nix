# Administrative access: key-only SSH for a dedicated admin account.
{ config, lib, ... }:
let
  cfg = config.strata;
  acfg = cfg.access;
  inherit (lib)
    mkEnableOption
    mkIf
    mkOption
    types
    ;

  # Excludes root: the admin must be an ordinary, attributable account.
  userName = types.addCheck (types.strMatching "[a-z_][a-z0-9_-]{0,31}") (n: n != "root") // {
    description = "POSIX user name other than root";
  };

  sshPublicKey =
    types.strMatching "(ssh-(ed25519|rsa)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh\\.com) [A-Za-z0-9+/]+=*( .*)?"
    // {
      description = "OpenSSH public key";
    };
in
{
  options.strata.access = {
    enable = mkEnableOption "Strata administrative SSH access";

    admin = {
      name = mkOption {
        type = userName;
        default = "strata-admin";
        description = ''
          Administrator account. It may use sudo without a password, since
          password authentication is disabled: its SSH keys are therefore
          root-equivalent.
        '';
      };

      authorizedKeys = mkOption {
        type = types.listOf sshPublicKey;
        default = [ ];
        example = [ "ssh-ed25519 AAAAC3Nza... admin@workstation" ];
        description = "SSH public keys allowed to log in as the administrator. At least one is required.";
      };
    };
  };

  config = mkIf acfg.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.access.enable requires strata.enable.";
      }
      {
        assertion = acfg.admin.authorizedKeys != [ ];
        message = "strata.access.admin.authorizedKeys must contain at least one key, otherwise nobody can log in.";
      }
    ];

    users.users.${acfg.admin.name} = {
      isNormalUser = true;
      extraGroups = [ "wheel" ];
      # Locked: no password login is possible.
      hashedPassword = "!";
      openssh.authorizedKeys.keys = acfg.admin.authorizedKeys;
    };

    security.sudo.extraRules = [
      {
        users = [ acfg.admin.name ];
        commands = [
          {
            command = "ALL";
            options = [ "NOPASSWD" ];
          }
        ];
      }
    ];

    services.openssh = {
      enable = true;
      # Port is opened on the management interface only (strata.networking).
      openFirewall = false;
      settings = {
        PasswordAuthentication = false;
        KbdInteractiveAuthentication = false;
        PermitRootLogin = "no";
        X11Forwarding = false;
        AllowUsers = lib.mkDefault [ acfg.admin.name ];
      };
    };
  };
}
