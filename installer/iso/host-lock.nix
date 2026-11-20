# The flake.lock of a host flake created by strata-install.
#
# It pins the host's `strata` input to the Strata source this ISO was
# built from, and takes every other node (nixpkgs, disko, ...) from
# Strata's own flake.lock, so the installed host evaluates exactly what
# the ISO tested and carries. All sources are already in the ISO's store,
# and Nix finds them there by narHash, so no network access is needed.
#
# The node layout matches templates/host/flake.nix:
#   inputs.strata.url = "github:letThemPlay/Strata";
#   inputs.nixpkgs.follows = "strata/nixpkgs";
{
  lib,
  self,
  owner ? "letThemPlay",
  repo ? "Strata",
}:
let
  strataLock = lib.importJSON (self + "/flake.lock");
  strataRoot = strataLock.nodes.${strataLock.root};
  inherited = removeAttrs strataLock.nodes [ strataLock.root ];

  # `follows` paths are relative to the lock's root; Strata's root is the
  # host's `strata` node.
  reroot = input: if lib.isList input then [ "strata" ] ++ input else input;
  rerootNode =
    node: node // lib.optionalAttrs (node ? inputs) { inputs = lib.mapAttrs (_: reroot) node.inputs; };

  # Builds from a dirty tree have no revision. The placeholder keeps the
  # lock well-formed; the narHash still pins the exact source.
  rev = self.rev or "0000000000000000000000000000000000000000";
in
assert lib.assertMsg (
  !(inherited ? strata) && !(inherited ? root)
) "host-lock: Strata's flake.lock has a node named strata or root";
{
  version = 7;
  root = "root";
  nodes = lib.mapAttrs (_: rerootNode) inherited // {
    root.inputs = {
      strata = "strata";
      nixpkgs = [
        "strata"
        "nixpkgs"
      ];
    };
    strata = {
      inputs = lib.mapAttrs (_: reroot) strataRoot.inputs;
      locked = {
        type = "github";
        inherit owner repo rev;
        inherit (self) narHash lastModified;
      };
      original = {
        type = "github";
        inherit owner repo;
      };
    };
  };
}
