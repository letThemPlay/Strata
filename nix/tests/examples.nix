# The examples check: evaluates examples/standalone/host.nix with the
# Strata modules, validates the IR it produces with `strata-controller
# validate-ir`, and requires it to equal internal/ir/testdata/standalone.json,
# which the Go tests parse too. Changing the Nix or the Go side of the IR
# alone therefore fails here. Regenerate the golden file with
# `make ir-golden`.
{
  self,
  nixpkgs,
  pkgs,
}:
let
  example = nixpkgs.lib.nixosSystem {
    modules = [
      self.nixosModules.default
      ../../examples/standalone/host.nix
      {
        nixpkgs.hostPlatform = pkgs.stdenv.hostPlatform.system;
        boot.loader.grub.enable = false;
        fileSystems."/" = {
          device = "/dev/null";
          fsType = "ext4";
        };
        system.stateVersion = "26.05";
      }
    ];
  };
  ir = pkgs.writeText "standalone-ir.json" (builtins.toJSON example.config.strata.ir);
  strata = self.packages.${pkgs.stdenv.hostPlatform.system}.strata;
in
pkgs.runCommand "strata-examples"
  {
    nativeBuildInputs = [
      strata
      pkgs.jq
      pkgs.diffutils
    ];
    passthru = { inherit ir; };
  }
  ''
    strata-controller validate-ir ${ir}
    jq -S . ${ir} > ir.json
    if ! diff -u ${../../internal/ir/testdata/standalone.json} ir.json; then
      echo "The example's IR differs from internal/ir/testdata/standalone.json; run make ir-golden." >&2
      exit 1
    fi
    cp ir.json $out
  ''
