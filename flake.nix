{
  description = "llama-warden: decides when the GPU is contended and tells its consumers to let go of it";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in
    {
      packages.${system}.default = pkgs.python3Packages.callPackage ./nix/package.nix { };

      nixosModules.default = import ./nix/module.nix self;

      checks.${system}.default = self.packages.${system}.default;

      # `nix develop -c pytest -q`. PYTHONPATH rather than an editable install,
      # so the checkout is what runs.
      devShells.${system}.default = pkgs.mkShell {
        packages = [
          (pkgs.python3.withPackages (ps: self.packages.${system}.default.dependencies ++ [ ps.pytest ]))
          pkgs.ruff
        ];
        shellHook = ''
          export PYTHONPATH="$PWD/src''${PYTHONPATH:+:$PYTHONPATH}"
          export LD_LIBRARY_PATH="${pkgs.addDriverRunpath.driverLink}/lib''${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
        '';
      };

      formatter.${system} = pkgs.nixfmt-tree;
    };
}
