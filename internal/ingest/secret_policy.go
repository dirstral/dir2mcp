package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// Secret-policy re-evaluation (spec 0.93.0 follow-up).
//
// A `secret_excluded` verdict is reached under one set of security.secret_patterns.
// When that set changes, the verdict can be wrong in either direction, but the
// incremental scan never asks again for an unchanged document:
//
//   - the cheap-signal fast path (ETag, or size and mtime for media) skips the
//     document before any read;
//   - carrySecretExclusion keeps a derived verdict (OCR, STT, extraction) on the
//     row for as long as the content hash is unchanged.
//
// So a binary upgrade that ships narrower default patterns (spec 0.93.0 fixed
// redirect links and URL slugs that the old patterns withheld) would leave every
// false positive withheld until a full reindex.
//
// The fix is one fingerprint. The scan records the identity of the effective
// pattern set in the settings table. When the recorded value differs from the
// active one (which includes "never recorded", the state of every corpus indexed
// before this existed), the scan clears the content hash of every withheld
// document, and of every archive that holds a withheld member, before it visits
// any file. An empty content hash is the existing "not finalized" state, so the
// normal pipeline re-reads those documents and decides again under the current
// patterns. Documents that are not withheld are not touched: the raw-byte scan
// already runs on every read, and a stricter pattern set reaching derived text of
// an `ok` document is outside this mechanism.
//
// The clear and the new identity commit in one transaction, and the clear is
// durable: an interrupted scan leaves the remaining rows with an empty hash, so
// the next scan still re-evaluates them.

// secretPolicyIdentity is the fingerprint of the effective pattern set. The
// order of the patterns does not change a verdict (any match withholds), so the
// pattern strings are sorted before hashing; an operator who reorders the list
// does not trigger a pass.
func secretPolicyIdentity(patterns []*regexp.Regexp) string {
	sources := make([]string, 0, len(patterns))
	for _, rx := range patterns {
		if rx == nil {
			continue
		}
		sources = append(sources, rx.String())
	}
	sort.Strings(sources)
	sum := sha256.Sum256([]byte(strings.Join(sources, "\n")))
	return "v1:" + hex.EncodeToString(sum[:])
}

// secretPolicyStore is the optional store capability that persists the secret
// policy identity and resets withheld documents for a new verdict. A store
// without it keeps the previous behaviour.
type secretPolicyStore interface {
	SecretPolicyIdentity(ctx context.Context) (string, error)
	ResetSecretExcludedForPolicy(ctx context.Context, identity string) (int64, error)
}

// reevaluateSecretExclusions compares the recorded secret policy identity with
// the active one and, when they differ, makes every withheld document eligible
// for a new verdict on this scan. It is called once per scan, before any
// document is visited. It never fails the scan: a store error is logged and the
// identity stays unrecorded, so the next scan tries again.
func (s *Service) reevaluateSecretExclusions(ctx context.Context, patterns []*regexp.Regexp) {
	policyStore, ok := s.store.(secretPolicyStore)
	if !ok {
		return
	}
	active := secretPolicyIdentity(patterns)
	recorded, err := policyStore.SecretPolicyIdentity(ctx)
	if err != nil {
		s.getLogger().Printf("secret policy: read secret policy identity failed, withheld documents keep their verdict this scan: %v", err)
		return
	}
	if recorded == active {
		return
	}
	cleared, err := policyStore.ResetSecretExcludedForPolicy(ctx, active)
	if err != nil {
		s.getLogger().Printf("secret policy: reset withheld documents failed, withheld documents keep their verdict this scan: %v", err)
		return
	}
	if cleared > 0 {
		s.getLogger().Printf(
			"secret policy: security.secret_patterns changed since the last scan; %d withheld document(s) or archive(s) will be evaluated again",
			cleared)
	}
}
