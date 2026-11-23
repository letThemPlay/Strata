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
          # mkDefault: strata.disk defines "/" in its own cases.
          fileSystems."/" = lib.mkDefault {
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

  testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@strata";

  # A fully configured host; `extra` modules are merged on top.
  hostWith =
    extra:
    eval (
      [
        {
          strata = {
            enable = true;
            node.id = "hv-01";
            agent.enable = true;
            compute.enable = true;
            access = {
              enable = true;
              admin.authorizedKeys = [ testKey ];
            };
            networking = {
              enable = true;
              management.interface = "eno1";
            };
            monitoring.enable = true;
          };
        }
      ]
      ++ extra
    );
  static = {
    strata.networking.management = {
      addresses = [
        "192.0.2.10/24"
        "2001:db8::10/64"
      ];
      gateway4 = "192.0.2.1";
      dns = [ "192.0.2.53" ];
    };
  };
  host = hostWith [ static ];
  hc = host.config;
  dhcpHost = hostWith [ { strata.networking.management.addressing = "dhcp"; } ];
  mgmtUnit = hc.systemd.network.networks."10-strata-management";
  diskHost = hostWith [
    static
    {
      strata.disk = {
        enable = true;
        device = "/dev/disk/by-id/test-disk";
      };
    }
  ];
  templateModules = [
    ../../templates/host/hardware-configuration.nix
    ../../templates/host/configuration.nix
    ../../templates/host/host.nix
  ];
  # Option value evaluation throws on type errors.
  rejects = sys: path: throws (lib.attrByPath path null sys.config);

  # Each host module must refuse to work without strata.enable.
  requiresEnable =
    module: settings:
    hasFailure "strata.${module}.enable requires strata.enable" (eval [
      {
        strata.${module} = {
          enable = true;
        }
        // settings;
      }
    ]);

  cases = {
    # ---- full host ----
    "full static host has no failed assertions" = failedAssertions host == [ ];
    "full DHCP host has no failed assertions" = failedAssertions dhcpHost == [ ];

    # ---- compute ----
    "compute enables libvirtd with host-only QEMU" =
      hc.virtualisation.libvirtd.enable
      && hc.virtualisation.libvirtd.qemu.package.outPath == pkgs.qemu_kvm.outPath;
    "compute runs QEMU unprivileged" = !hc.virtualisation.libvirtd.qemu.runAsRoot;
    "compute shuts guests down on host shutdown" = hc.virtualisation.libvirtd.onShutdown == "shutdown";
    "compute does not hardcode vendor KVM modules" =
      !(lib.elem "kvm-intel" hc.boot.kernelModules) && !(lib.elem "kvm-amd" hc.boot.kernelModules);
    "compute requires strata.enable" = requiresEnable "compute" { };

    # ---- access ----
    "access hardens sshd" =
      let
        s = hc.services.openssh.settings;
      in
      hc.services.openssh.enable
      && !s.PasswordAuthentication
      && !s.KbdInteractiveAuthentication
      && s.PermitRootLogin == "no"
      && s.AllowUsers == [ "strata-admin" ];
    "access creates a locked wheel admin with keys" =
      let
        u = hc.users.users.strata-admin;
      in
      u.isNormalUser
      && lib.elem "wheel" u.extraGroups
      && u.hashedPassword == "!"
      && u.openssh.authorizedKeys.keys == [ testKey ];
    "access grants passwordless sudo to the admin only" = lib.any (
      r: r.users == [ "strata-admin" ] && lib.any (c: lib.elem "NOPASSWD" c.options) r.commands
    ) hc.security.sudo.extraRules;
    "access without keys fails an assertion" =
      hasFailure "authorizedKeys must contain at least one key"
        (eval [
          {
            strata = {
              enable = true;
              node.id = "hv-01";
              access.enable = true;
            };
          }
        ]);
    "access rejects root as admin" = rejects (eval [ { strata.access.admin.name = "root"; } ]) [
      "strata"
      "access"
      "admin"
      "name"
    ];
    "access accepts a custom admin name" =
      (hostWith [
        static
        { strata.access.admin.name = "ops"; }
      ]).config.users.users
        ? ops;
    "access rejects invalid admin names" =
      rejects
        (eval [
          { strata.access.admin.name = "Admin User"; }
        ])
        [
          "strata"
          "access"
          "admin"
          "name"
        ];
    "access rejects malformed keys" =
      rejects
        (eval [
          { strata.access.admin.authorizedKeys = [ "not-a-key" ]; }
        ])
        [
          "strata"
          "access"
          "admin"
          "authorizedKeys"
        ];
    "access requires strata.enable" = requiresEnable "access" { admin.authorizedKeys = [ testKey ]; };
    "access sets a console password from a file, SSH stays key-only" =
      let
        c =
          (hostWith [
            static
            { strata.access.admin.consolePasswordFile = "/etc/strata/admin-console-password"; }
          ]).config;
        u = c.users.users.strata-admin;
      in
      u.hashedPasswordFile == "/etc/strata/admin-console-password"
      && u.hashedPassword == null
      && !c.services.openssh.settings.PasswordAuthentication
      && !c.services.openssh.settings.KbdInteractiveAuthentication;
    "access rejects a relative console password file" =
      rejects
        (eval [
          { strata.access.admin.consolePasswordFile = "admin-console-password"; }
        ])
        [
          "strata"
          "access"
          "admin"
          "consolePasswordFile"
        ];

    # ---- networking ----
    "networking uses systemd-networkd and nftables" =
      hc.networking.useNetworkd && !hc.networking.useDHCP && hc.networking.nftables.enable;
    "networking generates a static management unit" =
      mgmtUnit.matchConfig.Name == "eno1"
      && mgmtUnit.networkConfig.DHCP == "no"
      &&
        mgmtUnit.address == [
          "192.0.2.10/24"
          "2001:db8::10/64"
        ]
      && mgmtUnit.gateway == [ "192.0.2.1" ]
      && mgmtUnit.networkConfig.DNS == [ "192.0.2.53" ];
    "networking generates a DHCP management unit" =
      let
        u = dhcpHost.config.systemd.network.networks."10-strata-management";
      in
      u.networkConfig.DHCP == "yes" && u.address == [ ];
    "networking opens SSH and exporter only on the management interface" =
      hc.networking.firewall.interfaces.eno1.allowedTCPPorts == [
        22
        9100
      ]
      && hc.networking.firewall.allowedTCPPorts == [ ];
    "networking without access/monitoring opens nothing" =
      (eval [
        {
          strata = {
            enable = true;
            node.id = "hv-01";
            networking = {
              enable = true;
              management.interface = "eno1";
              management.addressing = "dhcp";
            };
          };
        }
      ]).config.networking.firewall.interfaces.eno1.allowedTCPPorts == [ ];
    "networking rejects globally opened ports" = hasFailure "Globally open: tcp/8080" (hostWith [
      static
      { networking.firewall.allowedTCPPorts = [ 8080 ]; }
    ]);
    "networking requires a management interface" = hasFailure "management.interface must be set" (eval [
      {
        strata = {
          enable = true;
          node.id = "hv-01";
          networking.enable = true;
        };
      }
    ]);
    "networking static without addresses fails" = hasFailure "addresses must not be empty" (
      hostWith [ ]
    );
    "networking DHCP with static settings fails" =
      hasFailure "only allowed with static addressing"
        (hostWith [
          static
          { strata.networking.management.addressing = "dhcp"; }
        ]);
    "networking rejects invalid interface names" =
      rejects
        (eval [
          { strata.networking.management.interface = "this-name-is-too-long"; }
        ])
        [
          "strata"
          "networking"
          "management"
          "interface"
        ];
    "networking rejects addresses without prefix" =
      rejects
        (eval [
          { strata.networking.management.addresses = [ "192.0.2.10" ]; }
        ])
        [
          "strata"
          "networking"
          "management"
          "addresses"
        ];
    "networking requires strata.enable" = requiresEnable "networking" {
      management.interface = "eno1";
      management.addressing = "dhcp";
    };

    # ---- monitoring ----
    "monitoring enables node exporter without opening the firewall globally" =
      hc.services.prometheus.exporters.node.enable
      && hc.services.prometheus.exporters.node.port == 9100
      && !hc.services.prometheus.exporters.node.openFirewall;
    "monitoring requires networking" = hasFailure "requires strata.networking.enable" (eval [
      {
        strata = {
          enable = true;
          node.id = "hv-01";
          monitoring.enable = true;
        };
      }
    ]);
    "monitoring requires strata.enable" = requiresEnable "monitoring" { };

    # ---- disk ----
    "disk generates an ESP and a root partition filling the device" =
      let
        d = diskHost.config.disko.devices.disk.system;
        p = d.content.partitions;
      in
      d.device == "/dev/disk/by-id/test-disk"
      && d.content.type == "gpt"
      && p.ESP.type == "EF00"
      && p.ESP.size == "1G"
      && p.root.size == "100%";
    "disk mounts root and /boot by partition label" =
      let
        fs = diskHost.config.fileSystems;
      in
      fs."/".fsType == "ext4"
      && fs."/".device == "/dev/disk/by-partlabel/disk-system-root"
      && fs."/boot".fsType == "vfat"
      && fs."/boot".device == "/dev/disk/by-partlabel/disk-system-ESP"
      && lib.elem "umask=0077" fs."/boot".options;
    "disk boots with systemd-boot, not GRUB" =
      let
        l = diskHost.config.boot.loader;
      in
      l.systemd-boot.enable && l.systemd-boot.configurationLimit == 10 && !l.grub.enable;
    "full disk host has no failed assertions" = failedAssertions diskHost == [ ];
    "disk without a device fails an assertion" = hasFailure "strata.disk.device must be set" (hostWith [
      static
      { strata.disk.enable = true; }
    ]);
    "disk rejects device paths outside /dev" =
      rejects
        (eval [
          { strata.disk.device = "sda"; }
        ])
        [
          "strata"
          "disk"
          "device"
        ];
    "disk rejects unknown layouts" =
      rejects
        (eval [
          { strata.disk.layout = "zfs"; }
        ])
        [
          "strata"
          "disk"
          "layout"
        ];
    "disk requires strata.enable" = requiresEnable "disk" { device = "/dev/vda"; };
    "disabled disk defines no partitions" = hc.disko.devices.disk == { };

    # ---- host template ----
    "unfilled template fails on exactly the values left to the user" =
      let
        failures = failedAssertions (eval templateModules);
        mentions = needle: lib.any (lib.hasInfix needle) failures;
      in
      lib.length failures == 3
      && mentions "authorizedKeys"
      && mentions "strata.disk.device"
      && mentions "management.interface";
    "reference host has no failed assertions" =
      failedAssertions (eval (templateModules ++ [ ../hosts/reference.nix ])) == [ ];
    # strata-install output (Go golden files) is a valid host.nix.
    "installer-generated host.nix files evaluate" =
      lib.all
        (
          golden:
          failedAssertions (eval [
            ../../templates/host/hardware-configuration.nix
            ../../templates/host/configuration.nix
            golden
          ]) == [ ]
        )
        [
          ../../internal/install/testdata/host-static.nix
          ../../internal/install/testdata/host-dhcp.nix
        ];

    # ---- base and agent ----
    "valid config has no failed assertions" = failedAssertions valid == [ ];
    "strata enables flakes" = lib.all (f: lib.elem f valid.config.nix.settings.experimental-features) [
      "nix-command"
      "flakes"
    ];
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
