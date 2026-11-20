# Passed to `strata-install apply --extra-module` by the nixos-installer
# test: lets the test driver control the installed system. Not used
# outside tests.
{ modulesPath, ... }:
{
  imports = [ (modulesPath + "/testing/test-instrumentation.nix") ];
}
