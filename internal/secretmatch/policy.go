package secretmatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Policy is the effective secret policy as data: the sorted secret and allow
// pattern lists (SPEC §7.2). Ingest records it, fingerprints it, and compares
// it with the policy that the corpus was last evaluated against.
type Policy struct {
	Secret []string `json:"secret"`
	Allow  []string `json:"allow"`
}

// Policy returns the sorted pattern lists of the matcher.
func (m *Matcher) Policy() Policy {
	secret := append([]string{}, m.SecretSources()...)
	allow := append([]string{}, m.AllowSources()...)
	sort.Strings(secret)
	sort.Strings(allow)
	return Policy{Secret: secret, Allow: allow}
}

// Identity is the fingerprint of the policy. The order of the patterns does
// not change a verdict, so the lists are sorted (Matcher.Policy sorts them) and
// an operator who reorders a list does not trigger a re-evaluation. The two
// lists are JSON-encoded as one object before hashing, so the encoding is
// unambiguous: a pattern that holds a newline never hashes the same as two
// patterns, and a secret pattern never hashes the same as an allow pattern.
func (p Policy) Identity() string {
	sorted := Policy{Secret: sortedCopy(p.Secret), Allow: sortedCopy(p.Allow)}
	// Marshalling two []string fields cannot fail.
	encoded, _ := json.Marshal(sorted)
	sum := sha256.Sum256(encoded)
	return "v2:" + hex.EncodeToString(sum[:])
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// preSpec0930SecretPatterns is the default list every release before spec
// 0.93.0 shipped (since #443).
var preSpec0930SecretPatterns = []string{
	`AKIA[0-9A-Z]{16}`,
	`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`,
	`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	GenericTokenPattern,
	`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
	`sk-[A-Za-z0-9-]{20,}`,
	`[A-Z][A-Z0-9_]*API_KEY\s*[:=]\s*\S{16,}`,
}

// PreSpec0930DefaultPolicy returns the default policy that every release
// before spec 0.93.0 shipped: its secret defaults and no allow list.
func PreSpec0930DefaultPolicy() Policy {
	return Policy{Secret: append([]string{}, preSpec0930SecretPatterns...)}
}

// narrowedSecretPatterns maps a pattern to the older pattern it narrows: every
// string the new pattern matches, the old one matches too. Each new pattern
// keeps the old pattern's core as a literal suffix and only adds a left
// boundary, so this holds by construction (spec 0.93.0). The generic token
// rule is not here: spec 0.93.0 keeps it broad and releases benign keys with
// the allow list instead.
var narrowedSecretPatterns = map[string]string{
	`(?:^|[^A-Za-z0-9_])(?:sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32})`: `sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
	`(?:^|[^A-Za-z0-9_-])sk-[A-Za-z0-9-]{20,}`:                   `sk-[A-Za-z0-9-]{20,}`,
}

// NarrowedSecretPatterns returns a copy of the declared narrowings: each key
// is a pattern, each value is the older pattern that it narrows.
func NarrowedSecretPatterns() map[string]string {
	out := make(map[string]string, len(narrowedSecretPatterns))
	for k, v := range narrowedSecretPatterns {
		out[k] = v
	}
	return out
}

// IsNarrower reports whether the active policy can only match less than the
// previous one. Then a document that previous did not match cannot match
// active, and indexed documents need no new read. It holds when:
//
//   - every active secret pattern is in previous, or is a declared narrowing
//     of a pattern in previous (NarrowedSecretPatterns); and
//   - every previous allow pattern is still in active. An added allow pattern
//     only cancels more matches; a removed or edited one can uncover a match.
//
// Any other change, also one that is stricter in fact (for example `X-[0-9]+`
// edited to `X-[0-9]{4}`), counts as broader: only the declared narrowings are
// known to be narrower.
func IsNarrower(previous, active Policy) bool {
	known := make(map[string]struct{}, len(previous.Secret))
	for _, p := range previous.Secret {
		known[p] = struct{}{}
	}
	for _, p := range active.Secret {
		if _, ok := known[p]; ok {
			continue
		}
		if old, ok := narrowedSecretPatterns[p]; ok {
			if _, had := known[old]; had {
				continue
			}
		}
		return false
	}
	activeAllow := make(map[string]struct{}, len(active.Allow))
	for _, p := range active.Allow {
		activeAllow[p] = struct{}{}
	}
	for _, p := range previous.Allow {
		if _, ok := activeAllow[p]; !ok {
			return false
		}
	}
	return true
}

// Recorded is the previous secret policy: the one a store recorded at its last
// transition, or, for a corpus that an older release indexed and that has no
// record, the lists that the run before held in its effective-config snapshot
// (config.PreviousSecretPolicy). Identity is empty when the store has no
// record. Known is false when the previous lists cannot be established.
type Recorded struct {
	Identity string
	Policy   Policy
	Known    bool
}

// NeedsIndexedReevaluation reports whether a move from the previous policy to
// the active one can match more, so that every indexed document must be read
// again (and is withheld until then, SPEC §7.2). Ingest decides its reset with
// it, and retrieval decides with it whether a transition that ingest has not
// applied yet must hide the indexed documents.
//
// The active configuration says nothing about the previous one, so an
// unknown previous policy counts as broader: a corpus indexed under a custom
// list that left out a default rule must be read again. A move that the
// previous lists prove narrower hides nothing.
func NeedsIndexedReevaluation(previous Recorded, active Policy) bool {
	if previous.Identity != "" && previous.Identity == active.Identity() {
		return false
	}
	if !previous.Known {
		return true
	}
	return !IsNarrower(previous.Policy, active)
}
