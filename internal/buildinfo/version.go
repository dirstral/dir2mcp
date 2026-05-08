package buildinfo

import (
	"runtime/debug"
	"sync"
)

// defaultVersion is the placeholder baked into binaries that were built
// without a -ldflags injection. GoReleaser overrides Version with the
// release tag at link time (see .goreleaser.yaml); local `go build` /
// `go install` users get the placeholder, and the resolveVersion fallback
// then tries to surface something useful from the embedded debug info.
const defaultVersion = "0.0.0-dev"

// Version is the link-time-injected build version. Reading this directly is
// fine for tests that want the raw injected value; production code that
// renders the version to users should call String() instead so that
// `go install`-style builds get the embedded VCS revision rather than the
// bare "0.0.0-dev" placeholder.
var Version = defaultVersion

var (
	resolveOnce sync.Once
	resolved    string
)

// String returns the user-facing build version. See resolveVersion for the
// fallback chain.
func String() string {
	resolveOnce.Do(func() {
		info, _ := debug.ReadBuildInfo()
		resolved = resolveVersion(Version, info)
	})
	return resolved
}

// resolveVersion is split out from String for testability — it takes the
// inputs explicitly so tests can drive every branch without wrestling with
// the package-level sync.Once or the real ReadBuildInfo result.
//
// Resolution order:
//
//  1. The link-time-injected `injected` string, when it differs from the
//     default "0.0.0-dev" placeholder. (GoReleaser sets this on real
//     releases.)
//  2. info.Main.Version, when it is non-empty and not the Go toolchain's
//     "(devel)" sentinel. This makes `go install` builds report e.g.
//     "v0.5.2" or a pseudo-version like
//     "v0.0.0-20260508195411-8869f0aabbcc" instead of the placeholder.
//  3. info.Settings vcs.revision (short), wrapped as "dev-<sha>", so even
//     an in-tree `go build` from a clean checkout shows the commit it was
//     built from.
//  4. The "0.0.0-dev" placeholder as a final fallback.
func resolveVersion(injected string, info *debug.BuildInfo) string {
	if injected != "" && injected != defaultVersion {
		return injected
	}
	if info != nil {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "dev-" + s.Value[:7]
			}
		}
	}
	return defaultVersion
}
