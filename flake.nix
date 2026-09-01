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