package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Document-level summary representations for hierarchical (coarse-to-fine)
// retrieval — SPEC §5.2 (`summary` rep_type + `coverage`), §8.6.7 (derivation
// identity), §9.7 (retrieval flow), §16.2 (`retrieval.hierarchical`).
//
// A summary is derived from ONE source representation of its OWN document via
// the configured chat generator, persisted as a `summary` representation whose
// meta_json carries the parent→child `coverage` linkage, and embedded as an
// ADDITIVE text-axis vector alongside that document's fine chunks. It is
// explicitly NOT an embed-identity input (§8.1.4): toggling the feature is an
// index add/remove, never a corpus-wide re-embed.
//
// Generation is capability-driven and FAIL-OPEN per document, mirroring
// OCR/STT/translate: with no chat-capable provider the binding stays nil and the
// step self-skips; a per-document generation failure logs, leaves the document
// with no summary (flat retrieval for that document), and is retried on the next
// scan. A summary never blocks ingest of the underlying chunks.

// summaryMaxSourceChars bounds how much source text is fed into ONE summary
// prompt. Document summaries run over whole documents, which can be far larger
// than any model's context window, so the source is truncated head-first at this
// budget. It is deliberately generous (a document summary is one call per
// document, §9.7) and domain-general.
const summaryMaxSourceChars = 24000

// summarySourceReader is the optional store capability hierarchical ingest needs:
// enumerate a document's summarizable representations and read their fine text.
// A store that does not implement it simply produces no summaries (the ingest
// pipeline is unchanged), mirroring the LexicalSearcher/DocumentHashLister
// optional-capability pattern.
type summarySourceReader interface {
	SummarySourceReps(ctx context.Context, relPath string) ([]store.SummarySourceRep, error)
	SummarySourceText(ctx context.Context, repID int64) ([]string, error)
}

// resolveSummaryBinding resolves the optional summary-generation binding (SPEC
// §9.7). When hierarchical retrieval is enabled we resolve the chat capability —
// honoring an explicit `retrieval.hierarchical.provider` pin — and build a
// generator. When the feature is off (the default), or no chat provider
// resolves, the field stays nil and every summary step self-skips, so ingest
// behaviour is byte-identical to before the feature existed.
func (svc *Service) resolveSummaryBinding(cfg config.Config) {
	if !cfg.HierarchicalDocumentLevelEnabled() {
		if cfg.HierarchicalSectionLevelRequested() {
			svc.warnSectionSummariesUnsupported()
		}
		return
	}
	if cfg.HierarchicalSectionLevelRequested() {
		svc.warnSectionSummariesUnsupported()
	}
	prof, err := cfg.Providers().ResolveExplicit(provider.CapChat, cfg.RetrievalHierarchicalProvider, false)
	if err != nil {
		svc.getLogger().Printf("hierarchical retrieval: summary generation disabled (no chat provider): %v", err)
		return
	}
	gen, err := providerfactory.Generator(prof)
	if err != nil {
		svc.getLogger().Printf("hierarchical retrieval: summary generation disabled (build generator %s): %v", prof.Name, err)
		return
	}
	svc.summarizer = gen
	svc.summaryProvider = strings.TrimSpace(prof.Name)
	svc.summaryModel = strings.TrimSpace(prof.ChatModel)
}

// warnSectionSummariesUnsupported records, once per service, that section-level
// summaries were requested but are not implemented yet: document-level summaries
// are still derived and section windows simply do not exist. Honest coverage
// beats a silent no-op (§9.7).
func (svc *Service) warnSectionSummariesUnsupported() {
	svc.getLogger().Printf("hierarchical retrieval: retrieval.hierarchical.levels requests %q, which is not implemented yet; only document-level summaries are derived",
		config.HierarchicalLevelSection)
}

// SetSummarizer overrides the summary-generation binding, primarily for tests.
// Passing a nil generator disables summary generation (the step self-skips). The
// recorded provider/model are written into each summary's meta_json and folded
// into its derivation identity (§5.2/§8.6.7). It mirrors SetTranslator.
func (s *Service) SetSummarizer(summarizer model.Generator, providerName, modelName string) {
	s.summarizer = summarizer
	s.summaryProvider = strings.TrimSpace(providerName)
	s.summaryModel = strings.TrimSpace(modelName)
}

// summaryConfigured reports whether document summaries should be derived this
// run: the feature is on for the document level AND a generator resolved.
func (s *Service) summaryConfigured() bool {
	return s.cfg.HierarchicalDocumentLevelEnabled() && s.summarizer != nil
}

// GenerateDocumentSummaries derives the document-level `summary`
// representation(s) for doc (SPEC §5.2/§9.7). It is FAIL-OPEN by construction:
// it never returns an error and never mutates the document's status, so a
// failed/absent summary leaves the document fully indexed and flat-retrievable.
func (s *Service) GenerateDocumentSummaries(ctx context.Context, doc model.Document) {
	if !s.summaryConfigured() || s.repGen == nil {
		return
	}
	reader, ok := s.store.(summarySourceReader)
	if !ok {
		return
	}
	candidates, err := reader.SummarySourceReps(ctx, doc.RelPath)
	if err != nil {
		s.getLogger().Printf("hierarchical retrieval: list summary sources for %s failed, skipping summary: %v", doc.RelPath, err)
		return
	}
	selected := selectSummarySourceReps(candidates, s.cfg.RetrievalHierarchicalSourceReps)
	for _, src := range selected {
		if err := s.generateSummaryForRep(ctx, doc, reader, src, len(selected) > 1); err != nil {
			// Fail-open per document/representation (§9.7): log and move on. The
			// document keeps its fine chunks and falls back to flat retrieval; the
			// next scan retries.
			s.getLogger().Printf("hierarchical retrieval: summary generation failed for %s (%s), falling back to flat retrieval: %v",
				doc.RelPath, src.RepType, err)
		}
	}
}

// selectSummarySourceReps picks which of a document's representations are
// summarized (SPEC §16.2 `source_reps`).
//
// A nil/empty want list is the `auto` default: the document's PRIMARY retrievable
// text representation — `transcript` for time media, else the extractor output
// (`extracted_markdown`), else `raw_text` — exactly ONE representation, so a
// summary's `coverage.source_rep_id` is unambiguous. An explicit list selects
// each named rep_type that exists on the document, in the CONFIGURED order.
//
// Only text-carrying, chunked representations are eligible: direct-embedding
// `media` chunks carry bytes rather than prose, and `annotation_json` is not
// chunked, so neither is ever summarized.
func selectSummarySourceReps(candidates []store.SummarySourceRep, want []string) []store.SummarySourceRep {
	eligible := make([]store.SummarySourceRep, 0, len(candidates))
	for _, cand := range candidates {
		if summaryRepEligible(cand.RepType) {
			eligible = append(eligible, cand)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	if len(want) == 0 {
		if primary, ok := primarySummarySourceRep(eligible); ok {
			return []store.SummarySourceRep{primary}
		}
		return nil
	}
	out := make([]store.SummarySourceRep, 0, len(want))
	for _, repType := range want {
		repType = strings.ToLower(strings.TrimSpace(repType))
		for _, cand := range eligible {
			if strings.EqualFold(cand.RepType, repType) {
				out = append(out, cand)
			}
		}
	}
	return out
}

// summaryRepEligible reports whether a representation type may be summarized:
// text-carrying, chunked representations only. `summary` itself is excluded at
// the store layer; `media` (direct-embedding bytes) and `annotation_json`
// (unchunked structured output) are excluded here.
func summaryRepEligible(repType string) bool {
	repType = strings.ToLower(strings.TrimSpace(repType))
	switch {
	case repType == "", repType == RepTypeMedia, repType == RepTypeAnnotationJSON:
		return false
	case model.IsSummaryRepType(repType):
		return false
	default:
		return true
	}
}

// primarySummarySourceRep resolves the `auto` selection: the document's primary
// retrievable text representation (SPEC §16.2). Transcripts win for time media
// (including per-language `transcript-<lang>` sidecars), then the extractor
// output, then raw text; any remaining representation is a last resort so a
// document is never left unsummarized purely for lack of a known rep_type. Ties
// within a tier resolve to the lowest rep_id (candidates arrive rep_id-ordered),
// so the pick is deterministic across re-index.
func primarySummarySourceRep(eligible []store.SummarySourceRep) (store.SummarySourceRep, bool) {
	tier := func(repType string) int {
		repType = strings.ToLower(strings.TrimSpace(repType))
		switch {
		case repType == RepTypeTranscript || strings.HasPrefix(repType, RepTypeTranscript+"-"):
			return 0
		case repType == RepTypeExtractedMarkdown:
			return 1
		case repType == RepTypeRawText:
			return 2
		case repType == RepTypeAnnotationText:
			return 3
		default:
			return 4
		}
	}
	best, found := store.SummarySourceRep{}, false
	for _, cand := range eligible {
		if !found || tier(cand.RepType) < tier(best.RepType) {
			best, found = cand, true
		}
	}
	return best, found
}

// generateSummaryForRep derives, caches, and persists one `summary`
// representation over src (SPEC §5.2). multi selects the rep_type: a single
// summarized representation uses the canonical `summary`, while an explicit
// multi-representation configuration produces one `summary-<source_rep_type>`
// per source so each has a distinct row under UNIQUE(doc_id, rep_type) — the
// same suffixing idiom per-language transcripts use.
func (s *Service) generateSummaryForRep(ctx context.Context, doc model.Document, reader summarySourceReader, src store.SummarySourceRep, multi bool) error {
	texts, err := reader.SummarySourceText(ctx, src.RepID)
	if err != nil {
		return fmt.Errorf("read source text: %w", err)
	}
	sourceText := truncateSummarySource(strings.Join(texts, "\n\n"))
	if strings.TrimSpace(sourceText) == "" {
		// Nothing to summarize (e.g. a media-only representation): not an error.
		return nil
	}

	summaryText, err := s.readOrComputeSummary(ctx, sourceText)
	if err != nil {
		return err
	}
	summaryText = strings.TrimSpace(summaryText)
	if summaryText == "" {
		return fmt.Errorf("generator returned an empty summary")
	}

	metaJSON, err := s.summaryMetaJSON(src)
	if err != nil {
		return fmt.Errorf("build summary meta: %w", err)
	}
	repType := model.SummaryRepType
	if multi {
		repType = model.SummaryRepType + "-" + strings.ToLower(strings.TrimSpace(src.RepType))
	}
	rep := model.Representation{
		DocID: doc.DocID,
		// The rep_hash is the summary derivation identity (§8.6.7): source content
		// + generator + effective prompt. It changes whenever any input changes, so
		// the persisted summary is never mistaken for a fresh one.
		RepType:     repType,
		RepHash:     s.summaryCacheKey(sourceText),
		MetaJSON:    metaJSON,
		CreatedUnix: time.Now().Unix(),
	}
	return s.repGen.PersistSummary(ctx, rep, summaryText)
}

// truncateSummarySource bounds the prompt input at summaryMaxSourceChars,
// cutting on a rune boundary so the prompt never carries a partial character.
func truncateSummarySource(text string) string {
	if len(text) <= summaryMaxSourceChars {
		return text
	}
	cut := summaryMaxSourceChars
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// utf8RuneStart reports whether b starts a UTF-8 rune (i.e. is not a
// continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// summaryMetaJSON builds the §5.2 meta_json persisted on a `summary`
// representation: the level, the generator identity (part of the derivation
// identity, §8.6.7), the prompt version, the effective-prompt hash when an
// operator override is configured, and the parent→child `coverage` linkage.
//
// The coverage names exactly ONE source representation — of this document — and
// a whole-representation range, which is what `summary_level=document` means.
func (s *Service) summaryMetaJSON(src store.SummarySourceRep) (string, error) {
	meta := model.SummaryMeta{
		SummaryLevel:  model.SummaryLevelDocument,
		Provider:      s.summaryProvider,
		Model:         s.summaryModel,
		PromptVersion: strings.TrimSpace(s.cfg.RetrievalHierarchicalPromptVersion),
		Coverage: model.SummaryCoverage{
			SourceRepID: src.RepID,
			Range:       model.SummaryCoverageRange{Kind: model.SummaryRangeDocument},
		},
	}
	if override := strings.TrimSpace(s.cfg.RetrievalHierarchicalPrompt); override != "" {
		// A version tag alone cannot detect an EDITED override, so the effective
		// prompt is hashed into the derivation identity (§5.2/§8.6.7).
		meta.PromptHash = computeContentHash([]byte(override))
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// activeSummaryIdentity is the generator side of the summary derivation identity
// (SPEC §8.6.7) in the canonical derivationIdentity form: the chat capability,
// the resolved provider/model, and the prompt version. The effective-prompt hash
// is folded in separately by summaryCacheKey so an edited override re-derives.
func (s *Service) activeSummaryIdentity() string {
	return derivationIdentity(string(provider.CapChat),
		s.summaryProvider, s.summaryModel, strings.TrimSpace(s.cfg.RetrievalHierarchicalPromptVersion), "")
}

// summaryCacheKey folds {source text, generator identity, effective prompt} into
// one stable key, mirroring translateCacheKey/transcriptCacheKey (SPEC §8.6.7).
// The same source summarized by the same generator with the same effective
// prompt reuses the cached summary (including across corpora); any change to a
// component misses the cache and re-derives.
func (s *Service) summaryCacheKey(sourceText string) string {
	parts := []string{
		computeContentHash([]byte(sourceText)),
		s.activeSummaryIdentity(),
		computeContentHash([]byte(strings.TrimSpace(s.cfg.RetrievalHierarchicalPrompt))),
	}
	return computeContentHash([]byte(strings.Join(parts, "\x00")))
}

// SummaryCacheKey exposes summaryCacheKey for tests in the tests/ tree so the
// {source, provider/model, effective prompt} → key binding (SPEC §8.6.7) can be
// asserted directly.
func (s *Service) SummaryCacheKey(sourceText string) string {
	return s.summaryCacheKey(sourceText)
}

// readOrComputeSummary returns the summary of sourceText, caching it under
// <state>/cache/summary keyed by the summary derivation identity (§8.6.7), so an
// unchanged document re-ingested with an unchanged generator never re-calls the
// model. Cache-write failures are non-fatal: the freshly generated summary is
// still returned.
func (s *Service) readOrComputeSummary(ctx context.Context, sourceText string) (string, error) {
	key := s.summaryCacheKey(sourceText)
	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "summary")
	cachePath := filepath.Join(cacheDir, key+".txt")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), nil
	}

	generated, err := s.generateSummaryText(ctx, sourceText)
	if err != nil {
		return "", err
	}
	out := strings.ReplaceAll(strings.ReplaceAll(generated, "\r\n", "\n"), "\r", "\n")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		s.getLogger().Printf("hierarchical retrieval: create summary cache dir failed (continuing uncached): %v", err)
		return out, nil
	}
	if err := os.WriteFile(cachePath, []byte(out), 0o644); err != nil {
		s.getLogger().Printf("hierarchical retrieval: write summary cache failed (continuing uncached): %v", err)
	}
	return out, nil
}

// generateSummaryText runs the configured generator over the built-in (general,
// domain-free) summary prompt, bounded by
// `retrieval.hierarchical.max_tokens` when the generator implements
// model.BoundedGenerator (a non-positive bound falls back to the generator's own
// default, per the BoundedGenerator contract).
func (s *Service) generateSummaryText(ctx context.Context, sourceText string) (string, error) {
	prompt := buildSummaryPrompt(sourceText, s.cfg.RetrievalHierarchicalPrompt)
	maxTokens := s.cfg.RetrievalHierarchicalMaxTokens
	if maxTokens > 0 {
		if bg, ok := s.summarizer.(model.BoundedGenerator); ok {
			return bg.GenerateWithMaxTokens(ctx, prompt, maxTokens)
		}
	}
	return s.summarizer.Generate(ctx, prompt)
}

// buildSummaryPrompt renders the summary prompt. The built-in template is
// deliberately general-purpose and domain-free — no language, subject, or
// corpus assumptions — and asks for a retrieval-oriented summary in the
// document's OWN language so a multilingual corpus keeps per-language recall.
// An operator override replaces the instruction block verbatim; the source text
// is always appended under a fixed delimiter so the override cannot silently
// drop the material being summarized.
func buildSummaryPrompt(sourceText, override string) string {
	instructions := strings.TrimSpace(override)
	if instructions == "" {
		instructions = strings.Join([]string{
			"Write a concise summary of the document below.",
			"Cover its main topics, entities, and conclusions so the summary can be matched against a search query.",
			"Write in the same language as the document.",
			"Output only the summary text, with no preamble, heading, or commentary.",
		}, " ")
	}
	var b strings.Builder
	b.WriteString(instructions)
	b.WriteString("\n\nDocument:\n")
	b.WriteString(sourceText)
	return b.String()
}
