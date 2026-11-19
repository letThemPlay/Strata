{
  description = "Strata: declarative, NixOS-native bare-metal virtualization platform";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;

      # Explicit list of supported systems (see ARCHITECTURE.md, ADR-0003).
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];

      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      version = "0.1.0-dev";
      commit = self.shortRev or self.dirtyShortRev or "unknown";

      # Only Go sources and module files affect the Go build.
      goSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          (lib.fileset.fileFilter (file: file.hasExt "go") ./cmd)
          (lib.fileset.fileFilter (file: file.hasExt "go") ./internal)
        ];
      };

      # Sources needed by lint checks (Go sources plus linter configuration).
      lintSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          (lib.fileset.fromSource goSrc)
          ./.golangci.yml
        ];
      };

      nixFiles = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.fileFilter (file: file.hasExt "nix") ./.;
      };
    in
    {
      packages = forAllSystems (pkgs: {
        strata = pkgs.callPackage ./nix/packages/strata.nix {
          src = goSrc;
          inherit version commit;
        };

        default = self.packages.${pkgs.stdenv.hostPlatform.system}.strata;
      });

      nixosModules = {
        strata =
          { lib, pkgs, ... }:
          {
            imports = [ ./nix/modules ];
            strata.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.strata;
          };
        default = self.nixosModules.strata;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools
            gofumpt
            golangci-lint
            gnumake
            git
            nixfmt
            statix
            deadnix
          ];

          shellHook = ''
            export GOTOOLCHAIN=local
          '';
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);

      checks = forAllSystems (
        pkgs:
        let
          inherit (pkgs.stdenv.hostPlatform) system;

          goEnv = ''
            export HOME=$TMPDIR
            export GOCACHE=$TMPDIR/go-cache
            export GOPATH=$TMPDIR/go
            export GOLANGCI_LINT_CACHE=$TMPDIR/golangci-cache
            export GOFLAGS=-mod=mod
            export GOPROXY=off
            export GOTOOLCHAIN=local
            export CGO_ENABLED=0
          '';
        in
        {
          # Builds all binaries and runs `go test ./...`.
          build = self.packages.${system}.strata;

          go-fmt =
            pkgs.runCommand "strata-go-fmt"
              {
                nativeBuildInputs = [ pkgs.gofumpt ];
              }
              ''
                unformatted=$(gofumpt -l ${goSrc})
                if [ -n "$unformatted" ]; then
                  echo "The following Go files are not gofumpt-formatted:" >&2
                  echo "$unformatted" >&2
                  exit 1
                fi
                touch $out
              '';

          go-lint =
            pkgs.runCommand "strata-golangci-lint"
              {
                nativeBuildInputs = [
                  pkgs.go
                  pkgs.golangci-lint
                ];
              }
              ''
                ${goEnv}
                cp -r ${lintSrc} src
                chmod -R u+w src
                cd src
                golangci-lint run ./...
                touch $out
              '';

          nix-fmt =
            pkgs.runCommand "strata-nix-fmt"
              {
                nativeBuildInputs = [ pkgs.nixfmt ];
              }
              ''
                find ${nixFiles} -name '*.nix' -print0 | xargs -0 nixfmt --check
                touch $out
              '';

          nix-lint =
            pkgs.runCommand "strata-nix-lint"
              {
                nativeBuildInputs = [
                  pkgs.statix
                  pkgs.deadnix
                ];
              }
              ''
                statix check ${nixFiles}
                deadnix --fail ${nixFiles}
                touch $out
              '';

          examples = import ./nix/tests/examples.nix { inherit pkgs lib; };

          module-eval = import ./nix/tests/modules.nix {
            inherit
              self
              nixpkgs
              pkgs
              lib
              ;
          };

          # NixOS VM test (requires KVM).
          nixos-agent = import ./nix/tests/nixos/agent.nix { inherit self pkgs; };
        }
      );
    };
}
