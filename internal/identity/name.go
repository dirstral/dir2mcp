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
	return nameFromKey(clean, filepath.Base(clean), dev)
}

// AutoServerNameForS3 derives the name for a corpus that lives in an object
// store, where the local root is not the corpus identity.
//
// `S3FS.Walk` ignores its root argument: bucket plus prefix IS the corpus root
// (SPEC §7.8). The local `root_dir` an S3 deployment carries is incidental and
// commonly left at its default, so deriving identity from it gave two different
// buckets launched from one directory the same MCP server name, the same
// service label and the same `claude mcp add` alias, letting the second install
// collide with the first (#737).
//
// The key is `s3://[endpoint/]bucket/prefix`, and it deliberately contains:
//
//   - the bucket and the normalized prefix, which are what identify the corpus;
//   - the endpoint host ONLY when one is configured, because two S3-compatible
//     stores can host the same bucket name while AWS bucket names are globally
//     unique. Including it unconditionally would change the identity of every
//     existing AWS deployment.
//
// and deliberately omits:
//
//   - every credential. The key reaches diagnostics and `claude mcp list`.
//   - the region. AWS bucket names are global, so it adds no uniqueness, and
//     setting it later on an existing deployment would silently rename the
//     instance.
func AutoServerNameForS3(bucket, prefix, endpoint string, dev bool) string {
	bucket = strings.ToLower(strings.TrimSpace(bucket))
	prefix = normalizeS3Prefix(prefix)
	key := "s3://"
	if host := endpointHost(endpoint); host != "" {
		key += host + "/"
	}
	key += bucket
	if prefix != "" {
		key += "/" + prefix
	}
	// The slug is the human-readable half. The bucket alone would make two
	// prefixes of one bucket visually identical, so the last prefix segment is
	// appended when there is one.
	slugSource := bucket
	if prefix != "" {
		if idx := strings.LastIndex(prefix, "/"); idx >= 0 {
			slugSource = bucket + "-" + prefix[idx+1:]
		} else {
			slugSource = bucket + "-" + prefix
		}
	}
	return nameFromKey(key, slugSource, dev)
}

// nameFromKey builds `<prefix>-<slug>-<6-hex>` from a canonical identity key
// and the string the human-readable slug is taken from. The key is what is
// hashed, so two corpora differ in the suffix whenever their keys differ even
// if their slugs collide after truncation.
func nameFromKey(key, slugSource string, dev bool) string {
	slug := slugify(slugSource)
	sum := sha256.Sum256([]byte(key))
	p := releasePrefix
	if dev {
		p = devPrefix
	}
	return p + "-" + slug + "-" + hex.EncodeToString(sum[:])[:hashLen]
}

// normalizeS3Prefix collapses the spellings of one prefix to a single form, so
// `corpus`, `corpus/`, `/corpus/` and `corpus//` are one corpus and not four
// instances. It is deliberately NOT filepath.Clean: an object key is
// slash-separated on every platform, and Clean would rewrite `\` on Windows.
func normalizeS3Prefix(prefix string) string {
	trimmed := strings.Trim(strings.TrimSpace(prefix), "/")
	if trimmed == "" {
		return ""
	}
	segments := make([]string, 0, 4)
	for _, segment := range strings.Split(trimmed, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return strings.Join(segments, "/")
}

// endpointHost reduces a configured endpoint to its host, so the same store
// addressed as `https://minio.example:9000` and `minio.example:9000` is one
// identity rather than two.
func endpointHost(endpoint string) string {
	host := strings.ToLower(strings.TrimSpace(endpoint))
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	return strings.Trim(host, "/")
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
