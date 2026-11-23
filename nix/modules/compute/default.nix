# Virtualization host: Incus manages QEMU/KVM VMs and LXC containers
# (ADR-0015).
#
# - The Incus API listens on loopback only; it is never opened on a network
#   interface.
# - Strata works in one Incus project that is restricted (no privileged or
#   nested containers, no host devices or paths, no low-level settings).
#   strata-incus-setup enforces the project's configuration on every boot.
# - With the agent enabled, strata-incus-setup also creates the agent's TLS
#   identity and has Incus trust it for that project only (ADR-0017). The
#   agent never gets the Incus Unix socket.
#
# Storage pools and networks are not configured yet (later M2 slices).
#
# KVM kernel modules are not listed here: udev autoloads kvm-intel or
# kvm-amd from the CPU's feature flags, so no vendor is hardcoded.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.strata;
  inherit (cfg.compute.incus) project address;

  # Restrictions of the Strata project. restricted=true already implies
  # most of them; they are listed explicitly so that a change of Incus's
  # defaults cannot loosen them.
  projectConfig = {
    restricted = "true";
    "restricted.containers.privilege" = "unprivileged";
    "restricted.containers.nesting" = "block";
    "restricted.containers.lowlevel" = "block";
    "restricted.containers.interception" = "block";
    "restricted.virtual-machines.lowlevel" = "block";
    "restricted.virtual-machines.nesting" = "block";
    "restricted.devices.disk" = "managed";
    "restricted.devices.disk.paths" = "";
    "restricted.devices.nic" = "managed";
    "restricted.devices.gpu" = "block";
    "restricted.devices.pci" = "block";
    "restricted.devices.usb" = "block";
    "restricted.devices.unix-char" = "block";
    "restricted.devices.unix-block" = "block";
    "restricted.devices.unix-hotplug" = "block";
    "restricted.devices.infiniband" = "block";
    "restricted.devices.proxy" = "block";
    "restricted.idmap.uid" = "";
    "restricted.idmap.gid" = "";
    "restricted.cluster.target" = "block";
    # Networks stay in the default project, where a restricted identity can
    # only view them; profiles, images and volumes are per project.
    "features.networks" = "false";
    "features.networks.zones" = "false";
    "features.profiles" = "true";
    "features.images" = "true";
    "features.storage.volumes" = "true";
    "features.storage.buckets" = "true";
  };
  projectJSON = pkgs.writeText "strata-incus-project.json" (
    builtins.toJSON {
      description = "Strata workloads (managed by NixOS; changes are reverted)";
      config = projectConfig;
    }
  );

  # The agent's identity; read by the agent through systemd credentials.
  identityDir = "/var/lib/strata-incus";
  certName = "strata-agent";

  setupScript = pkgs.writeShellApplication {
    name = "strata-incus-setup";
    runtimeInputs = [
      config.virtualisation.incus.clientPackage
      pkgs.jq
      pkgs.openssl
      pkgs.coreutils
    ];
    text = ''
      project=${lib.escapeShellArg project}

      # --- Project: create it, or replace its configuration exactly. ---
      if incus query "/1.0/projects/$project" >/dev/null 2>&1; then
        incus query -X PUT "/1.0/projects/$project" --data "$(cat ${projectJSON})" >/dev/null
        echo "project $project: configuration enforced"
      else
        jq --arg name "$project" '. + {name: $name}' ${projectJSON} |
          incus query -X POST /1.0/projects --data "$(cat)" >/dev/null
        echo "project $project: created"
      fi
    ''
    + lib.optionalString cfg.agent.enable ''

      # --- Agent identity: a key that never leaves this host. ---
      dir=${identityDir}
      if [ ! -s "$dir/client.key" ] || [ ! -s "$dir/client.crt" ]; then
        umask 077
        openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:secp384r1 -nodes \
          -keyout "$dir/client.key.tmp" -out "$dir/client.crt.tmp" -days 3650 \
          -subj "/CN=${certName}@${cfg.node.id}" \
          -addext "extendedKeyUsage=clientAuth" 2>/dev/null
        mv "$dir/client.key.tmp" "$dir/client.key"
        mv "$dir/client.crt.tmp" "$dir/client.crt"
        echo "agent identity: created"
      fi
      fp=$(openssl x509 -in "$dir/client.crt" -noout -fingerprint -sha256 |
        cut -d= -f2 | tr -d : | tr '[:upper:]' '[:lower:]')

      # --- Trust: exactly one entry, restricted to the project. ---
      # Entries with this name and another fingerprint are stale; an entry
      # with this fingerprint but other permissions is replaced.
      ok=false
      while read -r entry; do
        efp=$(jq -r .fingerprint <<<"$entry")
        if [ "$efp" = "$fp" ] &&
          jq -e --arg p "$project" '.restricted == true and .projects == [$p] and .type == "client"' <<<"$entry" >/dev/null; then
          ok=true
        else
          incus config trust remove "$efp"
          echo "trust: removed $efp"
        fi
      done < <(incus query '/1.0/certificates?recursion=1' |
        jq -c --arg name ${certName} --arg fp "$fp" '.[] | select(.name == $name or .fingerprint == $fp)')
      if [ "$ok" = false ]; then
        incus config trust add-certificate "$dir/client.crt" \
          --name ${certName} --restricted --projects "$project"
        echo "trust: added $fp, restricted to $project"
      else
        echo "trust: $fp restricted to $project"
      fi
    '';
  };
in
{
  options.strata.compute = {
    enable = lib.mkEnableOption "the Strata compute host role (Incus: QEMU/KVM VMs and LXC containers)";

    incus = {
      project = lib.mkOption {
        type = lib.types.strMatching "[a-z][a-z0-9-]{0,62}";
        default = "strata";
        description = ''
          Incus project that holds Strata's workloads. It is restricted, and
          its configuration is enforced on every boot. Must not be
          "default".
        '';
      };
      address = lib.mkOption {
        type = lib.types.str;
        default = "127.0.0.1:8443";
        readOnly = true;
        description = "Loopback address of the Incus API (not configurable).";
      };
    };
  };

  config = lib.mkIf cfg.compute.enable {
    assertions = [
      {
        assertion = cfg.enable;
        message = "strata.compute.enable requires strata.enable.";
      }
      {
        assertion = project != "default";
        message = "strata.compute.incus.project must not be \"default\" (it cannot be restricted).";
      }
    ];

    # Incus on NixOS requires the nftables firewall.
    networking.nftables.enable = true;

    virtualisation.incus = {
      enable = true;
      package = pkgs.incus-lts;
      ui.enable = false;
      # incusd starts at boot so that instances autostart.
      socketActivation = false;
      # Restarting incusd does not stop instances.
      softDaemonRestart = true;
      preseed.config."core.https_address" = address;
    };

    systemd.services.strata-incus-setup = {
      description = "Strata Incus project and agent identity";
      wantedBy = [ "multi-user.target" ];
      requires = [ "incus.service" ];
      after = [
        "incus.service"
        "incus-preseed.service"
      ];
      before = lib.optional cfg.agent.enable "strata-agent.service";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = lib.getExe setupScript;
        StateDirectory = "strata-incus";
        StateDirectoryMode = "0700";
        UMask = "0077";
      };
    };
  };
}
