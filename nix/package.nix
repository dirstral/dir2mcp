{ lib, buildGoModule, src, version ? "0.0.0-dev" }:

# callPackage-able derivation for the lean dir2mcp binary. The flake passes
# `src = self` (the repo root) so both the per-system `packages.default` build
# and the `overlays.default` consume the exact same source tree and build
# logic — a single source of truth. Downstream callers (overlay consumers) get
# `pkgs.dir2mcp` for free.
buildGoModule {
  pname = "dir2mcp";
  inherit version src;

  # Hash of the vendored Go module set (go.mod/go.sum). Recompute on any
  # dependency change: set this to `lib.fakeHash`, run `nix build .#dir2mcp`,
  # and copy the `got:` sha256 from the mismatch error back here. It is
  # independent of the version/ldflags. GoReleaser's nix publisher computes this
  # automatically for the derivation it generates; this value only matters when
  # building this hand-written flake directly.
  vendorHash = "sha256-Dl04jHfBLoG5yPykhUAX+jGS18PQoI+x2AwgIat11Oo=";

  subPackages = [ "cmd/dir2mcp" ];

  # Inject the version into the symbol the runtime reads
  # (internal/buildinfo.Version), mirroring .goreleaser.yaml and the Makefile.
  # Lean binary only -- no docling runtime bundled.
  ldflags = [
    "-s"
    "-w"
    "-X github.com/dirstral/dir2mcp/internal/buildinfo.Version=${version}"
  ];

  # Lean build: CGO is disabled in the release matrix.
  env.CGO_ENABLED = "0";

  # Skip tests during the package build; integration suites need external
  # credentials and the full check matrix runs in CI.
  doCheck = false;

  meta = with lib; {
    description = "Deploy any local directory as an MCP knowledge server with indexing, retrieval, and citations (lean binary; docling-full ships via container/Homebrew).";
    homepage = "https://github.com/dirstral/dir2mcp";
    license = licenses.mit;
    mainProgram = "dir2mcp";
    platforms = platforms.unix;
  };
}
