# Passed to `strata-install apply --extra-module` by the nixos-installer
# test. Not used outside tests.
{ modulesPath, ... }:
{
  imports = [
    # Lets the test driver control the installed system.
    (modulesPath + "/testing/test-instrumentation.nix")
    # In a VM, nixos-generate-config imports the QEMU guest profile; on
    # physical machines it imports not-detected.nix instead, which enables
    # redistributable firmware, and the detected vendor's microcode is
    # loaded. Do the same here, for both vendors, so that the offline build
    # covers what a physical install builds.
    (modulesPath + "/installer/scan/not-detected.nix")
  ];
  hardware.cpu = {
    intel.updateMicrocode = true;
    amd.updateMicrocode = true;
  };
}
