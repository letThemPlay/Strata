{
  description = "Strata host configuration";

  inputs = {
    strata.url = "github:letThemPlay/Strata";
    # Use the nixpkgs revision Strata is tested with.
    nixpkgs.follows = "strata/nixpkgs";
  };

  outputs =
    { nixpkgs, strata, ... }:
    {
      # The attribute name is the host's node ID (strata.node.id).
      nixosConfigurations.hv-01 = nixpkgs.lib.nixosSystem {
        modules = [
          strata.nixosModules.default
          ./hardware-configuration.nix
          ./configuration.nix
        ];
      };
    };
}
