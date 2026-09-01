{
  description = "rclone with the remarkable backend";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in {
      packages = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.callPackage ./package.nix { };
          rclone-remarkable = self.packages.${system}.default;
        });

      overlays.default = final: _prev: {
        rclone-remarkable = final.callPackage ./package.nix { };
      };

      nixosModules.default = { pkgs, ... }: {
        system.fsPackages = [ self.packages.${pkgs.stdenv.hostPlatform.system}.default ];
      };

      checks = forAllSystems (system:
        let
          pkgs = import nixpkgs { inherit system; };
          fsPackage = self.packages.${system}.default;
          evaluated = nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              self.nixosModules.default
              {
                fileSystems."/mnt/remarkable" = {
                  device = "remarkable:";
                  fsType = "rclone";
                  options = [
                    "config=/etc/rclone-remarkable.conf"
                    "cache_dir=/var/cache/rclone/remarkable"
                    "vfs_cache_mode=full"
                  ];
                };
              }
            ];
          };
          mount = evaluated.config.fileSystems."/mnt/remarkable";
        in {
          nixos-filesystems =
            assert mount.device == "remarkable:";
            assert mount.fsType == "rclone";
            assert builtins.elem "cache_dir=/var/cache/rclone/remarkable" mount.options;
            assert builtins.elem fsPackage evaluated.config.system.fsPackages;
            pkgs.runCommand "rclone-remarkable-nixos-filesystems" { } ''
              touch "$out"
            '';
        });

      devShells = forAllSystems (system:
        let pkgs = import nixpkgs { inherit system; };
        in {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              git
              fuse3
              pkg-config
            ];
          };
        });
    };
}