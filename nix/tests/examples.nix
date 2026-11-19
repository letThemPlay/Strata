# Evaluation test for the sample configurations in ./examples.
#
# This is a structural smoke test only: it asserts that each example
# evaluates and that references between resources are consistent. It is not
# a schema validator; schema validation belongs to the M3 IR work.
{ pkgs, lib }:
let
  sample = import ../../examples/standalone/strata.nix;

  workloads = lib.attrValues sample.workloads;

  checks = [
    {
      msg = "top-level keys must be exactly node, storage, networks, workloads";
      ok =
        lib.attrNames sample == [
          "networks"
          "node"
          "storage"
          "workloads"
        ];
    }
    {
      msg = "node.id must be a non-empty string";
      ok = lib.isString sample.node.id && sample.node.id != "";
    }
    {
      msg = "every workload type must be one of vm, lxc, oci";
      ok = lib.all (
        w:
        lib.elem w.type [
          "vm"
          "lxc"
          "oci"
        ]
      ) workloads;
    }
    {
      msg = "every disk must reference a declared storage pool";
      ok = lib.all (w: lib.all (d: sample.storage.pools ? ${d.storage}) (w.disks or [ ])) workloads;
    }
    {
      msg = "every network attachment must reference a declared network";
      ok = lib.all (w: lib.all (n: sample.networks ? ${n.bridge}) (w.networking or [ ])) workloads;
    }
    {
      msg = "VLAN IDs must be in range 1-4094";
      ok = lib.all (
        w: lib.all (n: !(n ? vlan) || (n.vlan >= 1 && n.vlan <= 4094)) (w.networking or [ ])
      ) workloads;
    }
  ];

  failures = lib.filter (c: !c.ok) checks;
in
assert lib.assertMsg (failures == [ ]) (
  "examples/standalone/strata.nix: " + lib.concatMapStringsSep "; " (c: c.msg) failures
);
pkgs.writeText "strata-example-standalone.json" (builtins.toJSON sample)
