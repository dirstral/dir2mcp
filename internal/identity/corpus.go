package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CorpusIDSettingKey is the `settings` row that holds the corpus's stable
// identity (SPEC §5.5). Once written it is authoritative: the derived value is
// only a seed, so moving or renaming the indexed directory does not silently
// mint a second corpus out of one store.
const CorpusIDSettingKey = "corpus_id"

// corpusIDHexLen is how much of the key digest the corpus id carries. 32 hex
// chars = 128 bits, which makes an accidental collision between two corpora
// sharing one broker not a thing an operator has to think about.
const corpusIDHexLen = 32

// CorpusKey returns the canonical identity key for a corpus whose bytes live
// under a local (or NFS-mounted) root. It is the same key AutoServerName hashes
// into the instance name, so the corpus identity and the MCP server identity
// can never disagree about which corpus is which.
//
// rootAbs is expected to already be absolute; identity is only as stable as the
// caller's normalization.
func CorpusKey(rootAbs string) string {
	return filepath.Clean(rootAbs)
}

// CorpusKeyForS3 returns the canonical identity key for a corpus that lives in
// an object store: `s3://[endpoint-host/]bucket[/prefix]`.
//
// It is the key AutoServerNameForS3 hashes (#737), reused rather than
// re-derived, and it inherits that function's two deliberate properties: the
// endpoint host participates ONLY when one is configured (two S3-compatible
// stores can host the same bucket name, while AWS bucket names are global), and
// no credential and no region ever participates. The key reaches diagnostics.
func CorpusKeyForS3(bucket, prefix, endpoint string) string {
	key := "s3://"
	if host := endpointHost(endpoint); host != "" {
		key += host + "/"
	}
	key += strings.ToLower(strings.TrimSpace(bucket))
	if p := normalizeS3Prefix(prefix); p != "" {
		key += "/" + p
	}
	return key
}

// CorpusID derives the stable corpus identity (SPEC §5.5) from a canonical
// identity key: `corpus-<32 hex>` over sha256(key).
//
// It is deliberately OPAQUE, unlike the instance name the same keys produce.
// A corpus id is written onto every distributed embedding job (SPEC §8.7.2) and
// therefore lands in a broker that, by design, several corpora may share — on a
// multi-tenant host that queue is the one place where corpus A can read rows
// corpus B wrote. A human-readable slug there would publish B's local path or
// bucket name to A; a digest identifies the corpus without describing it. An
// operator who needs the mapping reads it back from that corpus's own store
// (`settings.corpus_id`), which is exactly the boundary the id is meant to keep.
//
// It also deliberately omits the dev/release prefix AutoServerName carries: a
// persisted corpus identity that changed when the same corpus was opened by a
// dev build would split one corpus into two.
func CorpusID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "corpus-" + hex.EncodeToString(sum[:])[:corpusIDHexLen]
}

// SettingsStore is the `settings` (SPEC §5.5) read/write capability
// ResolveCorpusID needs. The metadata store satisfies it; it is declared here
// as a two-method interface so this package keeps its zero dependency on the
// store package. A missing key MUST be reported as os.ErrNotExist (what
// store.SQLiteStore.GetSetting returns).
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// ResolveCorpusID returns the corpus's stable identity (SPEC §5.5), reading the
// persisted value and seeding it from key on first use.
//
// The PERSISTED value wins whenever there is one. That is the whole point of
// the setting: a corpus that is moved to a new path, re-mounted at a different
// mountpoint, or re-pointed at a renamed bucket keeps the identity its already-
// enqueued jobs, its already-written vectors and its running workers were bound
// to. Deriving on every call instead would rename the corpus underneath them.
//
// A read failure other than "not set" is returned rather than papered over: a
// worker that guesses a corpus identity is exactly the cross-wiring the id
// exists to prevent.
func ResolveCorpusID(ctx context.Context, st SettingsStore, key string) (string, error) {
	if st == nil {
		return "", errors.New("identity: resolve corpus id: nil settings store")
	}
	existing, err := st.GetSetting(ctx, CorpusIDSettingKey)
	switch {
	case err == nil:
		if trimmed := strings.TrimSpace(existing); trimmed != "" {
			return trimmed, nil
		}
	case errors.Is(err, os.ErrNotExist):
		// First use of this store: fall through and seed it below.
	default:
		return "", fmt.Errorf("identity: read %s: %w", CorpusIDSettingKey, err)
	}

	derived := CorpusID(key)
	if err := st.SetSetting(ctx, CorpusIDSettingKey, derived); err != nil {
		return "", fmt.Errorf("identity: persist %s: %w", CorpusIDSettingKey, err)
	}
	return derived, nil
}
