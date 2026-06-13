{
  description = "dir2mcp - deploy any local directory as an MCP knowledge server (lean binary)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachSystem [
      "x86_64-linux"
      "aarch64-linux"
      "x86_64-darwin"
      "aarch64-darwin"
    ]
      (
        system:
        let
          pkgs = import nixpkgs { inherit system; };

          # Keep this in sync with the released tag. GoReleaser's nix
          # publisher generates a versioned flake on each release; this
          # hand-written flake tracks the source tree for `nix run` /
          # `nix build` from a checkout.
          version = "0.0.0-dev";

          dir2mcp = pkgs.buildGoModule {
            pname = "dir2mcp";
            inherit version;

            src = self;

            # NOTE (maintainer / CI): replace `lib.fakeHash` with the real
            # vendor hash on the first build. Run `nix build`, then copy the
            # `got:` hash from the mismatch error into this field. GoReleaser's
            # nix publisher computes this automatically for the derivation it
            # generates; this value only matters when building this
            # hand-written flake directly.
            vendorHash = pkgs.lib.fakeHash;

            subPackages = [ "cmd/dir2mcp" ];

            # Inject the version into the symbol the runtime reads
            # (internal/buildinfo.Version), mirroring .goreleaser.yaml and
            # the Makefile. Lean binary only -- no docling runtime bundled.
            ldflags = [
              "-s"
              "-w"
              "-X github.com/dirstral/dir2mcp/internal/buildinfo.Version=${version}"
            ];

            # Lean build: CGO is disabled in the release matrix.
            env.CGO_ENABLED = "0";

            # Skip tests during the package build; integration suites need
            # external credentials and the full check matrix runs in CI.
            doCheck = false;

            meta = with pkgs.lib; {
              description = "Deploy any local directory as an MCP knowledge server with indexing, retrieval, and citations (lean binary; docling-full ships via container/Homebrew).";
              homepage = "https://github.com/dirstral/dir2mcp";
              license = licenses.mit;
              mainProgram = "dir2mcp";
              platforms = platforms.unix;
            };
          };
        in
        {
          packages = {
            default = dir2mcp;
            dir2mcp = dir2mcp;
          };

          apps.default = {
            type = "app";
            program = "${dir2mcp}/bin/dir2mcp";
          };

          devShells.default = pkgs.mkShell {
            packages = [
              pkgs.go
              pkgs.gopls
              pkgs.gotools
            ];
          };
        }
      );
}
