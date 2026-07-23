package tests

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Document-level summary generation for hierarchical retrieval (SPEC §5.2 /
// §9.7 / §16.2, dir2mcp #329).

// summaryStore is a fakeIngestStore that also implements the summary-source
// capability: it reports a document's summarizable representations and their
// fine text.
type summaryStore struct {
	fakeIngestStore
	sources   []store.SummarySourceRep
	texts     map[int64][]string
	sourceErr error
}

func (s *summaryStore) SummarySourceReps(context.Context, string) ([]store.SummarySourceRep, error) {
	if s.sourceErr != nil {
		return nil, s.sourceErr
	}
	return s.sources, nil
}

func (s *summaryStore) SummarySourceText(_ context.Context, repID int64) ([]string, error) {
	return s.texts[repID], nil
}

// fakeSummarizer is a deterministic model.Generator stand-in that counts calls
// so cache reuse can be asserted. genErr, when set, makes every call fail.
type fakeSummarizer struct {
	mu     sync.Mutex
	calls  int
	prompt string
	out    string
	genErr error
}

func (f *fakeSummarizer) Generate(_ context.Context, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.prompt = prompt
	if f.genErr != nil {
		return "", f.genErr
	}
	if f.out != "" {
		return f.out, nil
	}
	return "a concise summary of the document", nil
}

func (f *fakeSummarizer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSummarizer) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompt
}

// hierarchicalConfig returns a config with hierarchical retrieval enabled at the
// document level and the spec defaults filled in (as config validation would).
func hierarchicalConfig(stateDir string) config.Config {
	return config.Config{
		StateDir:                           stateDir,
		RetrievalHierarchicalEnabled:       true,
		RetrievalHierarchicalLevels:        []string{config.HierarchicalLevelDocument},
		RetrievalHierarchicalMaxTokens:     config.DefaultHierarchicalMaxTokens,
		RetrievalHierarchicalPromptVersion: config.DefaultHierarchicalPromptVersion,
	}
}

func summaryReps(reps []model.Representation) []model.Representation {
	out := make([]model.Representation, 0, len(reps))
	for _, rep := range reps {
		if model.IsSummaryRepType(rep.RepType) {
			out = append(out, rep)
		}
	}
	return out
}

// TestDocumentSummary_Disabled_ProducesNoSummary pins the default: with
// hierarchical retrieval off, no summary representation is derived and the chat
// generator is never called — ingest is byte-identical to before the feature.
func TestDocumentSummary_Disabled_ProducesNoSummary(t *testing.T) {
	t.Parallel()
	st := &summaryStore{
		sources: []store.SummarySourceRep{{RepID: 1, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 2}},
		texts:   map[int64][]string{1: {"alpha", "beta"}},
	}
	svc := mustNewIngestService(t, config.Config{StateDir: t.TempDir()}, st)
	gen := &fakeSummarizer{}
	svc.SetSummarizer(gen, "mistral", "mistral-small-latest")

	svc.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "notes.md", DocType: "md"})

	if got := summaryReps(st.reps); len(got) != 0 {
		t.Fatalf("expected no summary with the feature off, got %+v", got)
	}
	if gen.callCount() != 0 {
		t.Fatalf("generator called %d times with the feature off", gen.callCount())
	}
}

// TestDocumentSummary_PersistsCoverageMeta pins the §5.2 meta_json shape: the
// level, the generator identity, the prompt version, and a `coverage` naming the
// single source representation of this document with a whole-document range.
func TestDocumentSummary_PersistsCoverageMeta(t *testing.T) {
	t.Parallel()
	st := &summaryStore{
		sources: []store.SummarySourceRep{{RepID: 11, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 2}},
		texts:   map[int64][]string{11: {"alpha", "beta"}},
	}
	svc := mustNewIngestService(t, hierarchicalConfig(t.TempDir()), st)
	gen := &fakeSummarizer{}
	svc.SetSummarizer(gen, "mistral", "mistral-small-latest")

	svc.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "notes.md", DocType: "md"})

	got := summaryReps(st.reps)
	if len(got) != 1 {
		t.Fatalf("expected exactly one summary representation, got %d (%+v)", len(got), st.reps)
	}
	rep := got[0]
	if rep.RepType != model.SummaryRepType {
		t.Fatalf("rep_type = %q, want %q", rep.RepType, model.SummaryRepType)
	}
	if rep.DocID != 5 {
		t.Fatalf("summary doc_id = %d, want 5", rep.DocID)
	}
	if rep.RepHash == "" {
		t.Fatal("summary rep_hash (the derivation identity) must be non-empty")
	}
	assertSummaryMeta(t, rep.MetaJSON, 11)
	assertPendingTextChunk(t, st)
}

// assertSummaryMeta checks the §5.2 meta_json fields of a document-level summary
// written by the default (no-override) configuration.
func assertSummaryMeta(t *testing.T, metaJSON string, wantSourceRepID int64) {
	t.Helper()
	var meta model.SummaryMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("summary meta_json is not parseable (%q): %v", metaJSON, err)
	}
	if meta.SummaryLevel != model.SummaryLevelDocument {
		t.Fatalf("summary_level = %q, want %q", meta.SummaryLevel, model.SummaryLevelDocument)
	}
	if meta.Provider != "mistral" || meta.Model != "mistral-small-latest" {
		t.Fatalf("generator identity = %q/%q, want mistral/mistral-small-latest", meta.Provider, meta.Model)
	}
	if meta.PromptVersion != config.DefaultHierarchicalPromptVersion {
		t.Fatalf("prompt_version = %q, want %q", meta.PromptVersion, config.DefaultHierarchicalPromptVersion)
	}
	if meta.PromptHash != "" {
		t.Fatalf("prompt_hash must be absent without an operator override, got %q", meta.PromptHash)
	}
	assertDocumentCoverage(t, meta.Coverage, wantSourceRepID)
}

// assertDocumentCoverage checks the parent→child linkage: a structurally valid
// whole-representation range naming this document's own source rep.
func assertDocumentCoverage(t *testing.T, coverage model.SummaryCoverage, wantSourceRepID int64) {
	t.Helper()
	if !coverage.Valid() {
		t.Fatalf("coverage is not structurally valid: %+v", coverage)
	}
	if coverage.SourceRepID != wantSourceRepID {
		t.Fatalf("coverage.source_rep_id = %d, want %d (the document's own source rep)",
			coverage.SourceRepID, wantSourceRepID)
	}
	if coverage.Range.Kind != model.SummaryRangeDocument {
		t.Fatalf("coverage.range.kind = %q, want %q", coverage.Range.Kind, model.SummaryRangeDocument)
	}
}

// assertPendingTextChunk checks that the summary was chunked onto the TEXT axis
// so it is embedded additively in the same space as the document's fine chunks
// (SPEC §5.2 embedding axis).
func assertPendingTextChunk(t *testing.T, st *summaryStore) {
	t.Helper()
	for _, chunk := range st.chunks {
		if chunk.IndexKind == "text" && chunk.EmbeddingStatus == "pending" {
			return
		}
	}
	t.Fatalf("expected at least one pending text-axis summary chunk, got chunks %+v", st.chunks)
}

// TestDocumentSummary_FailOpenWithNoChatProvider pins the capability-driven
// fail-open contract (§9.7): with hierarchical retrieval enabled but NO chat
// provider resolved, no summary is derived and nothing fails — the document
// keeps its fine chunks and falls back to flat retrieval.
func TestDocumentSummary_FailOpenWithNoChatProvider(t *testing.T) {
	t.Parallel()
	st := &summaryStore{
		sources: []store.SummarySourceRep{{RepID: 11, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 1}},
		texts:   map[int64][]string{11: {"alpha"}},
	}
	// mustNewIngestService constructs with no provider credentials, so
	// resolveSummaryBinding finds no chat provider and leaves the binding nil.
	svc := mustNewIngestService(t, hierarchicalConfig(t.TempDir()), st)

	svc.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "notes.md", DocType: "md"})

	if got := summaryReps(st.reps); len(got) != 0 {
		t.Fatalf("expected no summary without a chat provider, got %+v", got)
	}
}

// TestDocumentSummary_FailOpenOnGeneratorError pins the per-document fail-open
// path: a generator failure logs and leaves the document with no summary rather
// than erroring out of ingest.
func TestDocumentSummary_FailOpenOnGeneratorError(t *testing.T) {
	t.Parallel()
	st := &summaryStore{
		sources: []store.SummarySourceRep{{RepID: 11, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 1}},
		texts:   map[int64][]string{11: {"alpha"}},
	}
	svc := mustNewIngestService(t, hierarchicalConfig(t.TempDir()), st)
	svc.SetSummarizer(&fakeSummarizer{genErr: errors.New("provider down")}, "mistral", "m")

	// Must not panic and must not fail: GenerateDocumentSummaries returns nothing.
	svc.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "notes.md", DocType: "md"})

	if got := summaryReps(st.reps); len(got) != 0 {
		t.Fatalf("expected no summary after a generator failure, got %+v", got)
	}

	// A source-enumeration failure is equally non-fatal.
	failing := &summaryStore{sourceErr: errors.New("db unavailable")}
	svc2 := mustNewIngestService(t, hierarchicalConfig(t.TempDir()), failing)
	svc2.SetSummarizer(&fakeSummarizer{}, "mistral", "m")
	svc2.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "notes.md", DocType: "md"})
	if len(failing.reps) != 0 {
		t.Fatalf("expected no representation when source enumeration fails, got %+v", failing.reps)
	}
}

// TestDocumentSummary_AutoPicksPrimaryTextRepresentation pins the `auto`
// source_reps default (§16.2): exactly ONE representation is summarized — the
// transcript for time media, ahead of any other representation the document
// carries — so `coverage.source_rep_id` is unambiguous.
func TestDocumentSummary_AutoPicksPrimaryTextRepresentation(t *testing.T) {
	t.Parallel()
	st := &summaryStore{
		sources: []store.SummarySourceRep{
			{RepID: 11, DocID: 5, RepType: ingest.RepTypeAnnotationText, Chunks: 1},
			{RepID: 12, DocID: 5, RepType: ingest.RepTypeTranscript, Chunks: 3},
			{RepID: 13, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 2},
		},
		texts: map[int64][]string{
			11: {"annotation"},
			12: {"[00:00] hello", "[00:05] world"},
			13: {"raw"},
		},
	}
	svc := mustNewIngestService(t, hierarchicalConfig(t.TempDir()), st)
	svc.SetSummarizer(&fakeSummarizer{}, "mistral", "m")

	svc.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "talk.mp3", DocType: "audio"})

	got := summaryReps(st.reps)
	if len(got) != 1 {
		t.Fatalf("auto must summarize exactly one representation, got %d (%+v)", len(got), got)
	}
	var meta model.SummaryMeta
	if err := json.Unmarshal([]byte(got[0].MetaJSON), &meta); err != nil {
		t.Fatalf("parse summary meta: %v", err)
	}
	if meta.Coverage.SourceRepID != 12 {
		t.Fatalf("auto picked source_rep_id %d, want the transcript (12)", meta.Coverage.SourceRepID)
	}
}

// TestDocumentSummary_ExplicitSourceRepsProduceDistinctSummaries pins the
// explicit `source_reps` list: each named representation gets its OWN summary
// under a distinct rep_type, so both rows survive the store's
// UNIQUE(doc_id, rep_type) constraint and each names its own source.
func TestDocumentSummary_ExplicitSourceRepsProduceDistinctSummaries(t *testing.T) {
	t.Parallel()
	st := &summaryStore{
		sources: []store.SummarySourceRep{
			{RepID: 12, DocID: 5, RepType: ingest.RepTypeTranscript, Chunks: 2},
			{RepID: 13, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 1},
		},
		texts: map[int64][]string{12: {"spoken words"}, 13: {"written words"}},
	}
	cfg := hierarchicalConfig(t.TempDir())
	cfg.RetrievalHierarchicalSourceReps = []string{ingest.RepTypeTranscript, ingest.RepTypeRawText}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetSummarizer(&fakeSummarizer{}, "mistral", "m")

	svc.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "talk.mp3", DocType: "audio"})

	got := summaryReps(st.reps)
	if len(got) != 2 {
		t.Fatalf("expected one summary per configured source rep, got %d (%+v)", len(got), got)
	}
	seenTypes := map[string]int64{}
	for _, rep := range got {
		var meta model.SummaryMeta
		if err := json.Unmarshal([]byte(rep.MetaJSON), &meta); err != nil {
			t.Fatalf("parse summary meta: %v", err)
		}
		if _, dup := seenTypes[rep.RepType]; dup {
			t.Fatalf("duplicate summary rep_type %q would collide under UNIQUE(doc_id, rep_type)", rep.RepType)
		}
		seenTypes[rep.RepType] = meta.Coverage.SourceRepID
	}
	if seenTypes[model.SummaryRepType+"-"+ingest.RepTypeTranscript] != 12 {
		t.Fatalf("transcript summary must cover rep 12, got %+v", seenTypes)
	}
	if seenTypes[model.SummaryRepType+"-"+ingest.RepTypeRawText] != 13 {
		t.Fatalf("raw_text summary must cover rep 13, got %+v", seenTypes)
	}
}

// TestDocumentSummary_CachedByDerivationIdentity pins the §8.6.7 caching
// contract: the same source summarized by the same generator with the same
// effective prompt reuses the cached summary (no second model call), while a
// change to any identity component re-derives.
func TestDocumentSummary_CachedByDerivationIdentity(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	newStore := func() *summaryStore {
		return &summaryStore{
			sources: []store.SummarySourceRep{{RepID: 11, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 1}},
			texts:   map[int64][]string{11: {"alpha beta gamma"}},
		}
	}
	doc := model.Document{DocID: 5, RelPath: "notes.md", DocType: "md"}

	gen := &fakeSummarizer{}
	svc := mustNewIngestService(t, hierarchicalConfig(stateDir), newStore())
	svc.SetSummarizer(gen, "mistral", "m")
	svc.GenerateDocumentSummaries(context.Background(), doc)
	if gen.callCount() != 1 {
		t.Fatalf("first run: generator calls = %d, want 1", gen.callCount())
	}

	// Same state dir, same identity, same source: served from cache.
	svc2 := mustNewIngestService(t, hierarchicalConfig(stateDir), newStore())
	svc2.SetSummarizer(gen, "mistral", "m")
	svc2.GenerateDocumentSummaries(context.Background(), doc)
	if gen.callCount() != 1 {
		t.Fatalf("second run should hit the cache; generator calls = %d, want 1", gen.callCount())
	}

	// A model swap changes the derivation identity, so it re-derives.
	svc3 := mustNewIngestService(t, hierarchicalConfig(stateDir), newStore())
	svc3.SetSummarizer(gen, "mistral", "other-model")
	svc3.GenerateDocumentSummaries(context.Background(), doc)
	if gen.callCount() != 2 {
		t.Fatalf("a model swap must re-derive; generator calls = %d, want 2", gen.callCount())
	}

	// The cache key itself distinguishes an edited operator prompt override.
	promptCfg := hierarchicalConfig(stateDir)
	promptCfg.RetrievalHierarchicalPrompt = "Summarize in one sentence."
	svcPrompt := mustNewIngestService(t, promptCfg, newStore())
	svcPrompt.SetSummarizer(gen, "mistral", "m")
	if svcPrompt.SummaryCacheKey("alpha beta gamma") == svc.SummaryCacheKey("alpha beta gamma") {
		t.Fatal("an operator prompt override must change the summary derivation identity")
	}
}

// TestDocumentSummary_PromptOverrideIsHashedAndUsed pins that an operator prompt
// override replaces the built-in instructions AND is recorded as `prompt_hash`
// in meta_json (§5.2/§8.6.7), while the source text is always still supplied.
func TestDocumentSummary_PromptOverrideIsHashedAndUsed(t *testing.T) {
	t.Parallel()
	st := &summaryStore{
		sources: []store.SummarySourceRep{{RepID: 11, DocID: 5, RepType: ingest.RepTypeRawText, Chunks: 1}},
		texts:   map[int64][]string{11: {"the quick brown fox"}},
	}
	cfg := hierarchicalConfig(t.TempDir())
	cfg.RetrievalHierarchicalPrompt = "OVERRIDE-INSTRUCTIONS"
	svc := mustNewIngestService(t, cfg, st)
	gen := &fakeSummarizer{}
	svc.SetSummarizer(gen, "mistral", "m")

	svc.GenerateDocumentSummaries(context.Background(), model.Document{DocID: 5, RelPath: "notes.md", DocType: "md"})

	prompt := gen.lastPrompt()
	if prompt == "" {
		t.Fatal("generator was never called")
	}
	if want := "OVERRIDE-INSTRUCTIONS"; !strings.Contains(prompt, want) {
		t.Fatalf("prompt %q does not carry the operator override", prompt)
	}
	if !strings.Contains(prompt, "the quick brown fox") {
		t.Fatalf("prompt %q dropped the source text", prompt)
	}

	got := summaryReps(st.reps)
	if len(got) != 1 {
		t.Fatalf("expected one summary, got %d", len(got))
	}
	var meta model.SummaryMeta
	if err := json.Unmarshal([]byte(got[0].MetaJSON), &meta); err != nil {
		t.Fatalf("parse summary meta: %v", err)
	}
	if meta.PromptHash == "" {
		t.Fatal("prompt_hash must be recorded when an operator override is configured")
	}
}
