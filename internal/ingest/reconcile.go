package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Output-set reconciliation (dir2mcp #692).
//
// Incremental ingest treats a document's representations as independent upserts.
// It writes the outputs the ACTIVE pipeline produces, but it never retires the
// outputs a PREVIOUS pipeline produced. A representation can stop being wanted
// for three different reasons, and they need different handling:
//
//  1. its derivation identity changed (a new STT/OCR model). §8.6.7 already
//     covers this: the representation is re-derived in place, so the same
//     rep_type is overwritten and nothing is left over. No reconciliation needed.
//  2. the configuration no longer asks for that output at all (a translation
//     target was removed, hierarchical summaries were switched off). NOTHING
//     covers this today: the identity gate in §8.6.7 compares a recorded identity
//     against an ACTIVE one, and when the capability is switched off there is no
//     active identity to compare against, so the gate self-skips and the old row
//     stays live and searchable forever. This file covers this case.
//  3. the document type no longer supports that output. Out of scope here: a
//     doc_type change is a content change, so the normal content gate already
//     reprocesses the document.
//
// A full reindex does NOT fix case 2 either: `Reindex` clears
// documents.content_hash to force "changed" semantics, it does not tombstone
// representations, and re-running the pipeline only upserts the rep_types the
// current configuration produces. So the leftover row survives `--force` too.
//
// Cost. Retiring an output is cheap; rebuilding one may not be. So this
// reconciliation NEVER retires an output the active configuration still asks
// for, even when that output failed to derive this run: a best-effort failure
// leaves the last good row in place. It only retires outputs the operator has
// explicitly stopped asking for, and both covered outputs are backed by an
// on-disk derivation cache (translateCacheKey, summaryCacheKey), so re-enabling
// the target rebuilds them from cache rather than from a paid provider call.
//
// Trigger. Reconciliation is gated on the PIPELINE OUTPUT IDENTITY: a compact,
// greppable fingerprint of the configuration that decides the desired output
// SET. It is recorded in the store after each completed scan. When the recorded
// value differs from the active one the next scan reconciles every document it
// visits — including documents whose content did not change, which is the whole
// point — and then records the new value. On a steady-state scan the fingerprint
// matches, and reconciliation costs one settings read for the entire run.

// pipelineOutputIdentity is the canonical fingerprint of the configuration that
// decides WHICH representation types a document should carry. It deliberately
// covers only the inputs this reconciliation acts on, so an unrelated config
// edit does not trigger a corpus-wide pass. It is a readable string rather than
// a hash so it is greppable in logs and diffable in tests, matching
// derivationIdentity (§8.6.7).
func (s *Service) pipelineOutputIdentity() string {
	translate := "off"
	if s.cfg.MediaTranslateEnabled {
		translate = "on"
	}
	summary := "off"
	if s.cfg.HierarchicalDocumentLevelEnabled() {
		summary = "on"
	}
	return fmt.Sprintf("translate=%s:%s|summary=%s",
		translate, strings.Join(normalizedLanguageList(s.cfg.MediaTranslateTargetLangs), ","), summary)
}

// normalizedLanguageList lower-cases, trims, de-duplicates and sorts a language
// tag list so the fingerprint does not change when the operator merely reorders
// or re-cases the same targets.
func normalizedLanguageList(langs []string) []string {
	seen := make(map[string]struct{}, len(langs))
	out := make([]string, 0, len(langs))
	for _, lang := range langs {
		lang = strings.ToLower(strings.TrimSpace(lang))
		if lang == "" {
			continue
		}
		if _, dup := seen[lang]; dup {
			continue
		}
		seen[lang] = struct{}{}
		out = append(out, lang)
	}
	sort.Strings(out)
	return out
}

// activeRepresentationLister is the optional store capability that enumerates a
// document's active representations. A store without it disables reconciliation
// entirely, preserving prior behaviour.
type activeRepresentationLister interface {
	ActiveRepresentations(ctx context.Context, relPath string) ([]store.RepresentationRow, error)
}

// representationRetirer is the optional store capability that tombstones a
// chosen set of representations and their chunks in one transaction.
type representationRetirer interface {
	SoftDeleteRepresentations(ctx context.Context, relPath string, repIDs []int64) (int, error)
}

// pipelineOutputIdentityStore is the optional store capability that persists the
// pipeline output identity across runs.
type pipelineOutputIdentityStore interface {
	PipelineOutputIdentity(ctx context.Context) (string, error)
	SetPipelineOutputIdentity(ctx context.Context, identity string) error
}

// beginOutputReconciliation decides whether THIS scan must reconcile the output
// set of every document it visits, and arms the per-document hook accordingly.
// It is called once at the start of a scan.
//
// It arms the pass when the recorded pipeline output identity differs from the
// active one, or when the operator forced a reindex (`--force` is the operator's
// "make it right" lever, so it reconciles as well as re-derives). A corpus that
// has never recorded an identity is also reconciled once: absence says nothing
// about which outputs the previous configuration produced, and the pass is a
// read plus a rare tombstone, so it cannot re-derive anything expensive. That is
// why this differs from the §8.6.7 "an empty recorded identity always passes"
// rule, whose whole purpose is to avoid a paid corpus-wide re-derivation.
func (s *Service) beginOutputReconciliation(ctx context.Context, forceReindex bool) {
	s.reconcileOutputs = false
	s.pendingOutputIdentity = ""
	if _, ok := s.store.(activeRepresentationLister); !ok {
		return
	}
	if _, ok := s.store.(representationRetirer); !ok {
		return
	}
	identityStore, ok := s.store.(pipelineOutputIdentityStore)
	if !ok {
		return
	}
	active := s.pipelineOutputIdentity()
	recorded, err := identityStore.PipelineOutputIdentity(ctx)
	if err != nil {
		// Fail open: an unreadable setting must not start an unnecessary pass, and
		// must not fail the scan. The identity stays UNRECORDED as well, so the next
		// scan gets another chance to compare and reconcile instead of inheriting a
		// value this run never applied.
		s.getLogger().Printf("output reconciliation: read pipeline output identity failed, skipping this scan: %v", err)
		return
	}
	s.pendingOutputIdentity = active
	if recorded == active && !forceReindex {
		return
	}
	s.reconcileOutputs = true
	s.getLogger().Printf(
		"output reconciliation: pipeline outputs changed (recorded %q, active %q); retiring representations the current configuration no longer produces (#692)",
		recorded, active)
}

// commitOutputReconciliation records the pipeline output identity this scan
// reconciled the corpus against. It runs only after the scan completed, so an
// interrupted scan reconciles again on the next run instead of recording an
// identity it did not finish applying.
func (s *Service) commitOutputReconciliation(ctx context.Context) {
	if s.pendingOutputIdentity == "" {
		// The scan never established a comparable identity (no store capability, or
		// the read failed), so it has nothing it is entitled to record.
		return
	}
	identityStore, ok := s.store.(pipelineOutputIdentityStore)
	if !ok {
		return
	}
	if err := identityStore.SetPipelineOutputIdentity(ctx, s.pendingOutputIdentity); err != nil {
		s.getLogger().Printf("output reconciliation: record pipeline output identity failed: %v", err)
	}
	s.pendingOutputIdentity = ""
}

// reconcileDocumentOutputs retires the document's active representations that
// the current configuration no longer asks for. It is best-effort by contract:
// it never returns an error and never changes the document status, because a
// cleanup failure must not fail an otherwise good ingest. It is a no-op unless
// this scan armed the pass.
//
// Callers MUST invoke it AFTER the document's replacement outputs commit, so a
// representation is never destroyed before the output that supersedes it exists.
func (s *Service) reconcileDocumentOutputs(ctx context.Context, relPath string) {
	if !s.reconcileOutputs {
		return
	}
	lister, ok := s.store.(activeRepresentationLister)
	if !ok {
		return
	}
	retirer, ok := s.store.(representationRetirer)
	if !ok {
		return
	}
	reps, err := lister.ActiveRepresentations(ctx, relPath)
	if err != nil {
		// A missing document is normal here (a skipped or path-excluded asset), so
		// this is logged at the same low stakes as any other cleanup miss.
		return
	}
	obsolete := s.obsoleteRepresentations(reps)
	if len(obsolete) == 0 {
		return
	}
	retired, err := retirer.SoftDeleteRepresentations(ctx, relPath, obsolete)
	if err != nil {
		s.getLogger().Printf("output reconciliation: retire representations for %s failed: %v", relPath, err)
		return
	}
	if retired > 0 {
		s.getLogger().Printf("output reconciliation: retired %d representation(s) for %s that the current pipeline no longer produces (#692)",
			retired, relPath)
	}
}

// obsoleteRepresentations selects, from a document's active representations, the
// ones the current configuration no longer asks for. It is pure so the policy is
// testable without a store.
func (s *Service) obsoleteRepresentations(reps []store.RepresentationRow) []int64 {
	wantedLangs, langsKnown := s.desiredTranslationTargets()
	wantSummaries, summariesKnown := s.desiredSummaryOutputs()

	var obsolete []int64
	for _, rep := range reps {
		if lang, ok := translatedTranscriptLanguage(rep); ok {
			if langsKnown && !containsLanguage(wantedLangs, lang) {
				obsolete = append(obsolete, rep.RepID)
			}
			continue
		}
		if model.IsSummaryRepType(rep.RepType) && summariesKnown && !wantSummaries {
			obsolete = append(obsolete, rep.RepID)
		}
	}
	return obsolete
}

// desiredTranslationTargets returns the set of target languages the active
// configuration asks translated transcripts for, and whether that set is KNOWN.
//
// "Known" is the safety valve. An unresolved translator is NOT the same thing as
// translation being switched off: a missing credential or a provider that failed
// to build must leave every existing translation in place, because retiring them
// would silently discard paid output over a transient wiring problem. So the set
// is known in exactly two states:
//
//   - translation is wired and has targets: the desired set is those targets, so
//     a REMOVED target is retired while the remaining ones are untouched;
//   - the operator switched translation off in the configuration AND no
//     translator is wired: the desired set is empty, so every translation is
//     retired.
//
// Any other state (enabled in configuration but unresolved) reports unknown, and
// nothing is retired.
func (s *Service) desiredTranslationTargets() (map[string]struct{}, bool) {
	if s.translationConfigured() {
		wanted := make(map[string]struct{}, len(s.translateTargetLangs))
		for _, lang := range normalizedLanguageList(s.translateTargetLangs) {
			wanted[lang] = struct{}{}
		}
		return wanted, true
	}
	if !s.cfg.MediaTranslateEnabled && s.translator == nil && s.translateSTT == nil {
		return map[string]struct{}{}, true
	}
	return nil, false
}

// desiredSummaryOutputs reports whether the active configuration asks for
// document-level `summary` representations, and whether that answer is KNOWN. It
// mirrors desiredTranslationTargets: summaries are known-unwanted only when the
// operator switched hierarchical document-level generation off AND no summarizer
// is wired, so a summarizer that failed to resolve never costs an operator their
// existing summaries.
//
// Retiring them is what makes a disabled hierarchical mode actually flat: the
// summary vectors are additive entries in the document's own vector space
// (§8.6.7), so leaving them live lets a disabled feature keep consuming result
// slots.
func (s *Service) desiredSummaryOutputs() (wanted, known bool) {
	if s.summaryConfigured() {
		return true, true
	}
	if !s.cfg.HierarchicalDocumentLevelEnabled() && s.summarizer == nil {
		return false, true
	}
	return false, false
}

// translatedTranscriptLanguage reports whether rep is a TRANSLATED transcript
// and, if so, its target language.
//
// The verdict is read from the recorded provenance (meta_json `source`), never
// from the rep_type string, for two reasons. A `transcript-<lang>` rep_type is
// shared by an AUTHORED sidecar transcript (§8.6.4), which no translation
// setting may ever retire; and a translation of an additional audio track
// carries a track-qualified rep_type ("transcript@t1-es", §8.6.12), which a
// rep_type prefix test would miss. A translation whose meta records no language
// reports false, so an unreadable row is kept rather than guessed at.
func translatedTranscriptLanguage(rep store.RepresentationRow) (string, bool) {
	if !strings.HasPrefix(rep.RepType, RepTypeTranscript) {
		return "", false
	}
	metaJSON := strings.TrimSpace(rep.MetaJSON)
	if metaJSON == "" {
		return "", false
	}
	var meta transcriptMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return "", false
	}
	if strings.TrimSpace(meta.Source) != translationSource {
		return "", false
	}
	lang := strings.ToLower(strings.TrimSpace(meta.Language))
	if lang == "" {
		return "", false
	}
	return lang, true
}

// containsLanguage reports whether a desired-language set holds lang, matching
// the loose comparison the translate loop uses to skip a no-op self-translation:
// a tag matches when it equals a wanted tag or shares its primary subtag, so a
// target written as "pt-BR" keeps a transcript recorded as "pt".
func containsLanguage(wanted map[string]struct{}, lang string) bool {
	if _, ok := wanted[lang]; ok {
		return true
	}
	for want := range wanted {
		if sameLanguage(want, lang) {
			return true
		}
	}
	return false
}
