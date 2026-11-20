# Strata Go binaries. Building this package also runs the full Go unit test
# suite (ADR-0006).
{
  lib,
  buildGoModule,
  src,
  version,
  commit,
}:
buildGoModule {
  pname = "strata";
  inherit version src;

  # No third-party Go dependencies yet (ADR-0004).
  vendorHash = null;

  subPackages = [
    "cmd/strata"
    "cmd/strata-api"
    "cmd/strata-agent"
    "cmd/strata-controller"
    "cmd/strata-install"
  ];

  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
    "-X github.com/letthemplay/Strata/internal/version.version=${version}"
    "-X github.com/letthemplay/Strata/internal/version.commit=${commit}"
  ];

  doCheck = true;
  checkFlags = [ "-v" ];
  preCheck = ''
    # buildGoModule only tests subPackages by default; test everything.
    unset subPackages
  '';

  meta = {
    description = "Declarative, NixOS-native bare-metal virtualization platform";
    mainProgram = "strata";
    platforms = lib.platforms.linux;
  };
}
