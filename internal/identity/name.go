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
	// prefix is the project-wide name prefix. Kept here (not in the
	// caller) so the suffix logic can guarantee a known, parseable shape.
	prefix = "dir2mcp"
)

// Resolve returns override (after whitespace trim) when non-empty,
// otherwise the auto-derived name for rootAbs. Callers that thread a
// config value should prefer this over AutoServerName so the override
// path is the same everywhere.
func Resolve(rootAbs, override string) string {
	if t := strings.TrimSpace(override); t != "" {
		return t
	}
	return AutoServerName(rootAbs)
}

// AutoServerName returns a stable, terminal-friendly identifier for a
// dir2mcp instance keyed off the absolute path of the indexed directory.
//
// Shape: `dir2mcp-<slug>-<6-hex>` where <slug> is the lowercased,
// dash-normalized basename of rootAbs (capped at slugMaxLen) and <6-hex>
// is the first hashLen hex chars of sha256(rootAbs).
//
// rootAbs is expected to already be absolute (callers should run it
// through filepath.Abs); if it is not absolute, the hash is computed
// over whatever was passed and identity is therefore only as stable as
// the caller's normalization.
func AutoServerName(rootAbs string) string {
	clean := filepath.Clean(rootAbs)
	slug := slugify(filepath.Base(clean))
	sum := sha256.Sum256([]byte(clean))
	return prefix + "-" + slug + "-" + hex.EncodeToString(sum[:])[:hashLen]
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
