{
  description = "Strata: declarative, NixOS-native bare-metal virtualization platform";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # Declarative partitioning (ADR-0012). Shares our nixpkgs.
    disko = {
      url = "github:nix-community/disko";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      disko,
    }:
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

      # Only Go sources, module files and test data affect the Go build.
      goSrc = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          (lib.fileset.fileFilter (file: file.hasExt "go") ./cmd)
          (lib.fileset.fileFilter (file: file.hasExt "go") ./internal)
          # Golden files used by Go tests.
          ./internal/install/testdata
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

      # The reference host: the host template plus test values for what the
      # template leaves to the user (nix/hosts/reference.nix).
      mkReferenceHost =
        system:
        nixpkgs.lib.nixosSystem {
          modules = [
            self.nixosModules.default
            ./templates/host/hardware-configuration.nix
            ./templates/host/configuration.nix
            ./nix/hosts/reference.nix
            { nixpkgs.hostPlatform = system; }
          ];
        };

      # The installer ISO's NixOS system (installer/iso).
      mkInstaller =
        system: extraModules:
        nixpkgs.lib.nixosSystem {
          modules = [
            (import ./installer/iso {
              inherit self nixpkgs disko;
              referenceHost = mkReferenceHost system;
            })
            { nixpkgs.hostPlatform = system; }
          ]
          ++ extraModules;
        };
    in
    {
      packages = forAllSystems (pkgs: {
        strata = pkgs.callPackage ./nix/packages/strata.nix {
          src = goSrc;
          inherit version commit;
        };

        default = self.packages.${pkgs.stdenv.hostPlatform.system}.strata;

        # Installer ISO image: nix build .#iso
        iso = (mkInstaller pkgs.stdenv.hostPlatform.system [ ]).config.system.build.isoImage;
      });

      nixosModules = {
        strata =
          { lib, pkgs, ... }:
          {
            imports = [
              disko.nixosModules.disko
              ./nix/modules
            ];
            strata.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.strata;
          };
        default = self.nixosModules.strata;
      };

      templates = {
        host = {
          path = ./templates/host;
          description = "Configuration flake for one Strata host";
        };
        default = self.templates.host;
      };

      nixosConfigurations.reference = mkReferenceHost "x86_64-linux";

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

          # NixOS VM tests (require KVM).
          nixos-agent = import ./nix/tests/nixos/agent.nix { inherit self pkgs; };
          nixos-host = import ./nix/tests/nixos/host.nix { inherit self pkgs; };
          nixos-incus = import ./nix/tests/nixos/incus.nix { inherit self pkgs; };
          nixos-storage = import ./nix/tests/nixos/storage.nix { inherit self pkgs; };

          # The reference host builds for every supported system.
          reference-host = (mkReferenceHost system).config.system.build.toplevel;

          # The installer ISO's system (the image itself is built by the
          # nixos-installer test, in a test variant).
          installer-system = (mkInstaller system [ ]).config.system.build.toplevel;
        }
        # Partition an empty disk, install the reference host and boot it.
        # x86_64 only: disko's UEFI install tests do not boot on aarch64.
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isx86_64 {
          nixos-install-ext4 = import ./nix/tests/nixos/install.nix {
            inherit
              pkgs
              disko
              mkReferenceHost
              ;
          };

          # Boot the installer ISO, install a host offline and boot it.
          nixos-installer = import ./nix/tests/nixos/installer.nix {
            inherit pkgs mkInstaller mkReferenceHost;
          };
        }
      );
    };
}
