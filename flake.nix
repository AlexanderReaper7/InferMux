{
  description = "InferMux: llama-swap's model router with a GPU warden that yields the card to other work and never kills the user's own prompt";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in
    {
      packages.${system}.default = pkgs.callPackage ./nix/package.nix { };

      nixosModules.default = import ./nix/module.nix self;

      checks.${system}.default = self.packages.${system}.default;

      # `nix develop -c go test ./internal/warden/`. gcc for go-nvml's cgo.
      devShells.${system}.default = pkgs.mkShell {
        packages = [
          pkgs.go_1_27
          pkgs.gopls
          pkgs.nodejs
          # infermux-ui's keys tests encrypt for real.
          pkgs.sops
          pkgs.age
          pkgs.gnupg
        ];
        # As in nix/package.nix: go-nvml's symbols resolve at dlopen, not at load.
        hardeningDisable = [ "bindnow" ];
        shellHook = ''
          export CGO_CFLAGS="-Wno-deprecated-declarations"
          export LD_LIBRARY_PATH="${pkgs.addDriverRunpath.driverLink}/lib''${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
        '';
      };

      formatter.${system} = pkgs.nixfmt-tree;
    };
}
