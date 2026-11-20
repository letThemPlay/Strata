# The Strata installer ISO (ADR-0014).
#
# A NixOS minimal installation image with strata-install, the host
# template, a flake.lock pinning this Strata revision, and everything
# needed to build and install a Strata host without network access:
#
#   - the Strata, nixpkgs and disko sources (the host flake's inputs);
#   - the closure of the reference host, which contains nearly every
#     package an installed host needs;
#   - the tools needed to build what differs per machine (/etc, the
#     initrd, systemd units, the disk formatting script, ...).
#
# Arguments are supplied by flake.nix.
{
  self,
  nixpkgs,
  disko,
  referenceHost,
}:
{
  lib,
  pkgs,
  modulesPath,
  ...
}:
let
  inherit (pkgs.stdenv.hostPlatform) system;

  hostLock = import ./host-lock.nix { inherit lib self; };

  # The Strata package as an installed host evaluates it: its commit
  # comes from the host lock's revision, which differs from this
  # build's only for builds from a dirty tree (placeholder revision).
  # Shipping it avoids building Go offline in that case.
  hostStrata = self.packages.${system}.strata.override {
    commit = builtins.substring 0 7 hostLock.nodes.strata.locked.rev;
  };

  ref = referenceHost.config.system.build;
in
{
  imports = [ (modulesPath + "/installer/cd-dvd/installation-cd-minimal.nix") ];

  isoImage = {
    edition = "strata";
    appendToMenuLabel = " Strata installer";
  };
  image.baseName = lib.mkForce "strata-installer-${self.packages.${system}.strata.version}-${system}";

  networking.hostName = "strata-installer";

  environment.systemPackages = [
    self.packages.${system}.strata
    pkgs.git
  ];

  environment.etc = {
    "strata-install/template".source = "${self}/templates/host";
    "strata-install/flake.lock".text = builtins.toJSON hostLock;
  };

  services.getty.helpLine = lib.mkAfter ''

    To install Strata:  strata-install plan ANSWER-FILE
                        sudo strata-install apply ANSWER-FILE
    See docs/installation in the Strata repository.
  '';

  # Installation works offline; never wait for a binary cache.
  nix.settings = {
    experimental-features = [
      "nix-command"
      "flakes"
    ];
    substituters = lib.mkForce [ ];
  };

  system.extraDependencies = [
    # Host flake inputs.
    self.outPath
    nixpkgs.outPath
    disko.outPath

    # Runtime closure of a Strata host.
    ref.toplevel
    ref.destroyFormatMount
    hostStrata
  ]
  # Build-time dependencies of the per-machine derivations.
  ++ (with pkgs; [
    stdenv
    stdenvNoCC
    bash.dev
    bintools
    desktop-file-utils
    dieHook
    findXMLCatalogs
    getconf
    jq
    kmod.dev
    libarchive.dev
    libcap-text-verifier
    libxslt.bin
    lndir
    makeBinaryWrapper
    makeShellWrapper
    nukeReferences
    shellcheck-minimal
    switch-to-configuration-ng
    systemd.out
    texinfo
  ])
  # CPU microcode is prepended to the initrd at build time, so it is not in
  # any system's runtime closure; nixos-generate-config enables it for the
  # detected vendor on physical machines.
  ++ lib.optionals pkgs.stdenv.hostPlatform.isx86 (
    with pkgs;
    [
      microcode-intel
      microcode-amd
    ]
  )
  # networking.nftables.checkRuleset validates the ruleset at build time in
  # a Linux Kernel Library sandbox (nixos/modules/services/networking/nftables.nix).
  ++ (with pkgs.buildPackages; [
    lklWithFirewall.lib
    libredirect
  ]);
}
