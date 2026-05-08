package buildinfo

import (
	"runtime/debug"
	"strings"
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
//
// The result is memoized for the process lifetime via sync.Once so the
// resolution work runs at most once. Tests that want to exercise the
// resolution branches under different inputs should call resolveVersion
// directly with synthetic BuildInfo rather than mutating the Version
// package var and re-calling String — the second call always returns
// the cached result and the new Version value is never observed.
func String() string {
	resolveOnce.Do(func() {
		info, _ := debug.ReadBuildInfo()
		resolved = resolveVersion(Version, info)
	})
	return resolved
}

// Display returns the version with a "v" prefix, suitable for direct
// rendering to users. Centralized to avoid the
// "v" + strings.TrimPrefix(buildinfo.String(), "v") dance at every call
// site (see internal/cli/app.go for prior copies).
func Display() string {
	return "v" + strings.TrimPrefix(String(), "v")
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
//     built from. We require at least 7 hex digits to guard against
//     toolchain quirks that could plant a non-SHA value in vcs.revision
//     and produce a meaningless "dev-<7 garbage chars>" string.
//     If the build was made on a dirty tree (vcs.modified=true), append
//     "+dirty" so the rendered version flags local changes.
//  4. The "0.0.0-dev" placeholder as a final fallback.
func resolveVersion(injected string, info *debug.BuildInfo) string {
	if injected != "" && injected != defaultVersion {
		return injected
	}
	if info != nil {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		var revision string
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) >= 7 && isHex(s.Value) {
					revision = s.Value[:7]
				}
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if revision != "" {
			out := "dev-" + revision
			if dirty {
				out += "+dirty"
			}
			return out
		}
	}
	return defaultVersion
}

// isHex reports whether s is composed entirely of hexadecimal digits.
// Used to validate vcs.revision before we treat its prefix as a Git SHA.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
