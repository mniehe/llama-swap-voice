{
  description = "OpenAI audio API (speech and transcriptions) in front of llama-swap";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  inputs.flake-utils.url = "github:numtide/flake-utils";

  inputs.gomod2nix = {
    url = "github:nix-community/gomod2nix";
    inputs.nixpkgs.follows = "nixpkgs";
    inputs.flake-utils.follows = "flake-utils";
  };

  inputs.pyproject-nix = {
    url = "github:pyproject-nix/pyproject.nix";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  inputs.uv2nix = {
    url = "github:pyproject-nix/uv2nix";
    inputs.pyproject-nix.follows = "pyproject-nix";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  inputs.pyproject-build-systems = {
    url = "github:pyproject-nix/build-system-pkgs";
    inputs.pyproject-nix.follows = "pyproject-nix";
    inputs.uv2nix.follows = "uv2nix";
    inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
    gomod2nix,
    ...
  } @ inputs:
    flake-utils.lib.eachSystem ["x86_64-linux" "aarch64-linux" "aarch64-darwin"] (
      system: let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [gomod2nix.overlays.default];
        };

        version = pkgs.lib.trim (builtins.readFile ./.version);

        packages = import ./nix/packages.nix {inherit pkgs version inputs;};
        go = packages.default.go;
      in {
        formatter = pkgs.alejandra;
        inherit packages;
        devShells = import ./nix/devshells.nix {inherit pkgs go;};
        # Only the Linux checks are run anywhere; Darwin gets the package and
        # the dev shell, not a gate nobody has seen pass.
        checks = pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux (
          import ./nix/checks.nix {
            inherit pkgs go version;
            package = packages.default;
            module = self.nixosModules.default;
          }
          // packages.qwen-tts-server.passthru.checks or {}
        );
      }
    )
    // {
      nixosModules.default = import ./nix/module.nix self;
    };
}
