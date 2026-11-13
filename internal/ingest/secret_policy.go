package ingest

import (
	"context"
	"fmt"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
)

// Secret-policy re-evaluation (SPEC §7.2, spec 0.93.0).
//
// A `secret_excluded` verdict, and equally an `ok` verdict, is reached under one
// secret policy: security.secret_patterns and security.secret_allow_patterns.
// The policy changes when a release changes a default (0.93.0 narrowed two
// rules and added the allow list) or when an operator edits a list. The
// incremental scan does not ask again on its own for an unchanged document:
//
//   - the cheap-signal fast path (ETag, or size and mtime for media) skips the
//     document before any read;
//   - carrySecretExclusion keeps a derived verdict (OCR, STT, extraction) on the
//     row for as long as the content hash is unchanged;
//   - the content gate does not regenerate the representations of an unchanged
//     `ok` document, so its derived text is never screened again, and an archive
//     member is never re-read while its container is unchanged.
//
// SPEC §7.2 requires both directions on the first scan after a change. Both use
// the same rule: clear the content hash of each document the change can affect,
// before the scan visits any file. An empty content hash is the existing "not
// finalized" state, so the normal pipeline re-reads the source and re-derives
// the text, and the same code that decided the first verdict decides again.
// Nothing is reconstructed from stored chunks: a chunk can drop bytes the raw
// scan sees (a trimmed trailing newline, the markup of a member), and two chunks
// can overlap, so a check over chunk text is not the check ingest makes.
// OCR, transcription and translation come from their on-disk caches, so the
// re-read does not repeat a provider call that already succeeded.
//
//  1. Withheld documents are always evaluated again: every `secret_excluded`
//     document, and every archive that holds one.
//  2. Indexed documents are evaluated again only when the new policy can match
//     something the old one did not (secretmatch.IsNarrower decides: an added
//     allow pattern or a declared narrowing of a secret pattern is narrower;
//     anything else is not). Then every `ok` document and every archive is
//     cleared, and marked as waiting for re-evaluation. Retrieval returns no
//     part of a marked document until the scan has decided it again, because a
//     credential can cross a chunk boundary where no excerpt matches.
//
// The reset and the new identity commit in one store transaction. The retry
// state is the empty hash on each row: a document that fails, or that a stopped
// scan did not reach, keeps it and is processed again on a later scan. A failure
// of the reset itself fails the scan, so it is retried in full.

// secretPolicyStore is the optional store capability behind the re-evaluation.
// A store without it keeps the previous behaviour.
type secretPolicyStore interface {
	SecretPolicyIdentity(ctx context.Context) (string, error)
	SecretPolicyPatterns(ctx context.Context) ([]string, bool, error)
	SecretPolicyAllowPatterns(ctx context.Context) ([]string, bool, error)
	ResetForSecretPolicy(ctx context.Context, identity string, patterns, allowPatterns []string, reevaluateIndexed bool) (int64, error)
}

// beginSecretPolicyReevaluation compares the recorded secret policy with the
// active one and, when they differ, resets the documents the change can affect
// (see the file comment). It runs once per scan, before any document is
// visited. A store failure is returned and fails the scan: a scan that could
// not apply a changed policy must not report success, and the next scan tries
// again because nothing was recorded.
func (s *Service) beginSecretPolicyReevaluation(ctx context.Context, matcher *secretmatch.Matcher) error {
	policyStore, ok := s.store.(secretPolicyStore)
	if !ok {
		return nil
	}
	activePolicy := matcher.Policy()
	active := activePolicy.Identity()
	recorded, err := policyStore.SecretPolicyIdentity(ctx)
	if err != nil {
		return fmt.Errorf("secret policy: read the recorded secret policy identity: %w", err)
	}
	if recorded == active {
		return nil
	}
	previous, known, err := recordedSecretPolicy(ctx, policyStore, recorded, activePolicy)
	if err != nil {
		return err
	}
	reevaluateIndexed := !known || !secretmatch.IsNarrower(previous, activePolicy)
	cleared, err := policyStore.ResetForSecretPolicy(ctx, active, activePolicy.Secret, activePolicy.Allow, reevaluateIndexed)
	if err != nil {
		return fmt.Errorf("secret policy: reset documents for the changed secret policy: %w", err)
	}
	scope := "withheld documents"
	if reevaluateIndexed {
		scope = "withheld and indexed documents"
	}
	s.getLogger().Printf(
		"secret policy: security.secret_patterns or security.secret_allow_patterns changed since the last scan; %d %s (and the archives that hold them) will be read and evaluated again",
		cleared, scope)
	return nil
}

// recordedSecretPolicy returns the policy the corpus was last evaluated
// against. known is false when that policy cannot be proven, and then every
// indexed document is read again.
//
// A corpus with no recorded identity was indexed by a release before spec
// 0.93.0. Such a release recorded nothing, so the list it used is lost. When
// the active policy is the shipped default, the corpus is taken to have used
// the pre-0.93.0 defaults: the move from those defaults is a declared
// narrowing, so only the withheld documents are read again. When the active
// policy is a custom one, the old list may have lacked a rule that the active
// list holds, so the previous policy is unknown.
func recordedSecretPolicy(ctx context.Context, policyStore secretPolicyStore, recorded string, active secretmatch.Policy) (secretmatch.Policy, bool, error) {
	if recorded == "" {
		if isShippedDefaultPolicy(active) {
			return secretmatch.PreSpec0930DefaultPolicy(), true, nil
		}
		return secretmatch.Policy{}, false, nil
	}
	secret, secretKnown, err := policyStore.SecretPolicyPatterns(ctx)
	if err != nil {
		return secretmatch.Policy{}, false, fmt.Errorf("secret policy: read the recorded security.secret_patterns list: %w", err)
	}
	allow, allowKnown, err := policyStore.SecretPolicyAllowPatterns(ctx)
	if err != nil {
		return secretmatch.Policy{}, false, fmt.Errorf("secret policy: read the recorded security.secret_allow_patterns list: %w", err)
	}
	return secretmatch.Policy{Secret: secret, Allow: allow}, secretKnown && allowKnown, nil
}

// isShippedDefaultPolicy reports whether the active policy is the shipped
// default of both lists.
func isShippedDefaultPolicy(active secretmatch.Policy) bool {
	defaults := config.Default()
	matcher, err := secretmatch.Compile(defaults.SecretPatterns, defaults.SecretAllowPatterns)
	if err != nil {
		return false
	}
	return matcher.Policy().Identity() == active.Identity()
}

// retireNewlyWithheld retires the representations of a document that its raw
// bytes now withhold while its stored row is still `ok`. That happens when the
// pattern set became broader (SPEC §7.2): the raw-byte scan reaches the new
// verdict on the read, but the document generates no new representations, so
// without this its old chunks stay searchable under a `secret_excluded` row.
//
// It runs BEFORE the caller upserts the new status, and a failure is returned so
// the caller does not write that status: the row stays `ok` with its current
// content hash, and the next read decides again. Writing `secret_excluded` over
// live chunks would hide the document from this check for good, because the
// check fires only on a row that is still `ok`.
func (s *Service) retireNewlyWithheld(ctx context.Context, existing, doc model.Document) error {
	if doc.Status != string(model.DocumentStatusSecretExcluded) {
		return nil
	}
	if existing.RelPath == "" || existing.Deleted || existing.Status != string(model.DocumentStatusOK) {
		return nil
	}
	if err := s.retireRepresentationsForPath(ctx, doc.RelPath, "secret policy"); err != nil {
		return fmt.Errorf("secret policy: retire the representations of newly withheld %s: %w", doc.RelPath, err)
	}
	return nil
}

// settleSecretVerdict applies the two secret-policy steps that run between the
// build of a document row and its upsert: the carry of a derived verdict
// (carrySecretExclusion, #681) and the retirement of an `ok` document that its
// raw bytes now withhold (retireNewlyWithheld). One call keeps processDocument
// and processDocumentFromContent within the complexity budget.
func (s *Service) settleSecretVerdict(ctx context.Context, doc *model.Document, existing model.Document) error {
	carrySecretExclusion(doc, existing)
	return s.retireNewlyWithheld(ctx, existing, *doc)
}

// existingDocWithSecretVerdict reads the stored row of doc and applies
// settleSecretVerdict to doc. It returns the stored row (zero when there is
// none) for the caller's gate and transition log.
func (s *Service) existingDocWithSecretVerdict(ctx context.Context, doc *model.Document) (model.Document, error) {
	existing, err := s.store.GetDocumentByPath(ctx, doc.RelPath)
	if isUnexpectedStoreErr(err) {
		return model.Document{}, fmt.Errorf("get existing document: %w", err)
	}
	if err := s.settleSecretVerdict(ctx, doc, existing); err != nil {
		return model.Document{}, err
	}
	return existing, nil
}
