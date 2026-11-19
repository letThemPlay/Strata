# Strata NixOS modules. Import through the flake's `nixosModules.default`,
# which also sets `strata.package`.
{
  imports = [
    ./base
    ./agent
  ];
}
