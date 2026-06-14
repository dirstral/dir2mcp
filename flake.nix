{
  description = "dir2mcp - deploy any local directory as an MCP knowledge server (lean binary)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { self, nixpkgs, flake-utils }:
    # Per-system outputs (packages / apps / devShells) are produced via
    # eachSystem and merged (//) with the system-agnostic outputs (overlay,
    # nix-darwin module, home-manager module). The package itself is defined
    # once in nix/package.nix and exposed through overlays.default so the
    # per-system build, the overlay, and the service modules all share a single
    # source of truth.
    (flake-utils.lib.eachSystem [
      "x86_64-linux"
      "aarch64-linux"
      "x86_64-darwin"
      "aarch64-darwin"
    ]
      (
        system:
        let
          pkgs = import nixpkgs {
            inherit system;
            overlays = [ self.overlays.default ];
          };
        in
        {
          packages = {
            default = pkgs.dir2mcp;
            dir2mcp = pkgs.dir2mcp;
          };

          apps.default = {
            type = "app";
            program = "${pkgs.dir2mcp}/bin/dir2mcp";
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.gotools
            ];
          };
        }
      ))
    // {
      # System-agnostic outputs.

      # Overlay: adds `dir2mcp` to any nixpkgs instance. `final.callPackage`
      # supplies { lib, buildGoModule }; we pass `src = self` (the flake's own
      # source tree) explicitly so the build works identically whether invoked
      # from this flake's per-system packages or from a downstream consumer's
      # nixpkgs that applies this overlay.
      #
      # Keep this in sync with the released tag. GoReleaser's nix publisher
      # generates a versioned flake on each release; this hand-written flake
      # tracks the source tree for `nix run` / `nix build` from a checkout.
      overlays.default = final: prev: {
        dir2mcp = final.callPackage ./nix/package.nix {
          src = self;
          version = "0.0.0-dev";
        };
      };

      # nix-darwin service module (launchd). `import` returns a function of
      # `self`, applied here so the module can default `package` to this flake's
      # build for the host system.
      darwinModules.default = import ./nix/darwin-module.nix self;

      # home-manager service module (launchd on darwin, systemd --user on linux).
      homeManagerModules.default = import ./nix/home-manager-module.nix self;
    };
}
