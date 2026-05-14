// Package identity derives a stable, unique server name for a dir2mcp
// instance from the absolute path of the directory it indexes. The
// derived name is used as the MCP serverInfo.name and as the suggested
// alias for `claude mcp add`, so power users running many instances can
// distinguish them in their MCP client list without manual bookkeeping.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// slugMaxLen caps the human-readable folder slug so the final
	// `dir2mcp-<slug>-<hash>` name stays terminal-friendly. The hash
	// suffix is always preserved, so truncation never costs uniqueness.
	slugMaxLen = 32
	// hashLen is the prefix length (in hex chars) of sha256(rootAbs)
	// used as the disambiguation suffix. 6 hex chars = 24 bits =
	// 1-in-16M collision pressure per pair, which is more than enough
	// for hand-managed MCP server lists.
	hashLen = 6
	// releasePrefix is the project-wide name prefix used by released
	// (GoReleaser-built) binaries.
	releasePrefix = "dir2mcp"
	// devPrefix is used by source/dev builds so a developer running
	// their in-tree binary alongside the brew-installed release sees
	// two visibly distinct entries in `claude mcp list` instead of
	// colliding on the same auto-derived name.
	devPrefix = "dir2mcp-dev"
)

// Resolve returns override (after whitespace trim) when non-empty,
// otherwise the auto-derived name for rootAbs. The dev flag selects
// the project prefix (release vs dev) for the auto-derivation path; an
// explicit override is always used verbatim regardless of build type.
// Callers that thread a config value should prefer this over
// AutoServerName so the override semantics stay in one place.
func Resolve(rootAbs, override string, dev bool) string {
	if t := strings.TrimSpace(override); t != "" {
		return t
	}
	return AutoServerName(rootAbs, dev)
}

// AutoServerName returns a stable, terminal-friendly identifier for a
// dir2mcp instance keyed off the absolute path of the indexed directory.
//
// Shape: `<prefix>-<slug>-<6-hex>` where <prefix> is `dir2mcp` for
// released binaries and `dir2mcp-dev` for developer builds (see
// buildinfo.IsDev), <slug> is the lowercased dash-normalized basename
// of rootAbs (capped at slugMaxLen), and <6-hex> is the first hashLen
// hex chars of sha256(rootAbs).
//
// rootAbs is expected to already be absolute (callers should run it
// through filepath.Abs); if it is not absolute, the hash is computed
// over whatever was passed and identity is therefore only as stable as
// the caller's normalization.
func AutoServerName(rootAbs string, dev bool) string {
	clean := filepath.Clean(rootAbs)
	slug := slugify(filepath.Base(clean))
	sum := sha256.Sum256([]byte(clean))
	p := releasePrefix
	if dev {
		p = devPrefix
	}
	return p + "-" + slug + "-" + hex.EncodeToString(sum[:])[:hashLen]
}

// slugify lowercases s, replaces every non-[a-z0-9] rune with a single
// dash, collapses runs of dashes, trims leading/trailing dashes, and
// caps the result at slugMaxLen runes. Empty input (or input that
// contains no alphanumerics) produces "dir" so the auto-name stays
// well-formed.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) && r < 0x80, unicode.IsDigit(r) && r < 0x80:
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "dir"
	}
	if len(out) > slugMaxLen {
		out = strings.TrimRight(out[:slugMaxLen], "-")
		if out == "" {
			return "dir"
		}
	}
	return out
}
