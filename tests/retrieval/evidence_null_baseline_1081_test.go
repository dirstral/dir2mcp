package tests

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #1081, SPEC §9.4.3 (spec 0.76.0): the cosine evidence threshold is
// max(fixed floor, null baseline p90), where the baseline is the top cosine of
// the shipped probe questions against the corpus. These tests pin the pure
// rule on synthetic distributions, the guard firing under a high baseline and
// staying silent under a low one, the pin, the state-dir cache, and the
// stats report.

// textEmbedder answers one vector for the query text and another for every
// other input (the probe set), so a test chooses the query cosine and the
// baseline cosine against a corpus chunk at {1, 0} independently.
type textEmbedder struct {
	mu          sync.Mutex
	query       string
	queryVec    []float32
	probeVec    []float32
	probeBatchs int
}

func (e *textEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(texts) > 1 {
		e.probeBatchs++
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		src := e.probeVec
		if text == e.query {
			src = e.queryVec
		}
		out[i] = append([]float32(nil), src...)
	}
	return out, nil
}

func (e *textEmbedder) probeBatches() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.probeBatchs
}

// unitVec returns the unit vector whose cosine against {1, 0} is cos.
func unitVec(cos float64) []float32 {
	return []float32{float32(cos), float32(math.Sqrt(1 - cos*cos))}
}

func baselineService(t *testing.T, gen *fakeGenerator, emb *textEmbedder, chunks ...uint64) (*retrieval.Service, *index.HNSWIndex) {
	t.Helper()
	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(nil, idx, emb, gen)
	for _, id := range chunks {
		addVec(t, idx, id, []float32{1, 0})
		svc.SetChunkMetadata(id, model.SearchHit{
			ChunkID: id, RelPath: "docs/a.md", Snippet: "text",
			Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
		})
	}
	return svc, idx
}

// syntheticBaseline is 32 readings from 0.40 to 0.71 in steps of 0.01: p50 at
// the middle, p90 at position 27.9, max at the end.
func syntheticBaseline() ([]float64, model.EvidenceBaseline) {
	scores := make([]float64, 32)
	for i := range scores {
		scores[i] = 0.40 + float64(i)*0.01
	}
	return scores, retrieval.SummarizeNullBaseline(scores)
}

// assertThreshold checks one application of the pure rule.
func assertThreshold(t *testing.T, label string, baseline *model.EvidenceBaseline, auto bool, pinned, wantValue float64, wantSource string) {
	t.Helper()
	got, src := retrieval.EvidenceCosineThreshold(0.05, baseline, auto, pinned)
	if math.Abs(got-wantValue) > 1e-9 || src != wantSource {
		t.Fatalf("%s: threshold %.4f source %q, want %.4f %q", label, got, src, wantValue, wantSource)
	}
}

func TestEvidence1081_SummaryQuantiles(t *testing.T) {
	scores, b := syntheticBaseline()
	if b.Probes != 32 {
		t.Fatalf("probes = %d, want 32", b.Probes)
	}
	if math.Abs(b.P50-0.555) > 1e-9 || math.Abs(b.P90-0.679) > 1e-9 || math.Abs(b.Max-0.71) > 1e-9 {
		t.Fatalf("summary = p50 %.4f p90 %.4f max %.4f, want 0.555 / 0.679 / 0.71", b.P50, b.P90, b.Max)
	}
	// Three on-topic probes do not move the p90 (the maximum would follow them).
	shifted := append(append([]float64(nil), scores[:29]...), 0.95, 0.96, 0.97)
	if s := retrieval.SummarizeNullBaseline(shifted); math.Abs(s.P90-0.679) > 0.03 {
		t.Fatalf("p90 after three on-topic probes = %.4f, moved from 0.679", s.P90)
	}
	if e := retrieval.SummarizeNullBaseline(nil); e.Probes != 0 {
		t.Fatalf("empty summary probes = %d, want 0", e.Probes)
	}
}

func TestEvidence1081_RuleOnSyntheticDistributions(t *testing.T) {
	_, b := syntheticBaseline()
	low := retrieval.SummarizeNullBaseline([]float64{0.01, 0.02, 0.03})
	// The rule applies the p90 under auto, a pin whatever the baseline, and
	// the floor when there is no baseline, a baseline below it, or auto off.
	assertThreshold(t, "auto", &b, true, 0, b.P90, model.EvidenceThresholdSourceAuto)
	assertThreshold(t, "pin", &b, true, 0.42, 0.42, model.EvidenceThresholdSourceConfig)
	assertThreshold(t, "no baseline", nil, true, 0, 0.05, model.EvidenceThresholdSourceFloor)
	assertThreshold(t, "low baseline", &low, true, 0, 0.05, model.EvidenceThresholdSourceFloor)
	assertThreshold(t, "auto off", &b, false, 0, 0.05, model.EvidenceThresholdSourceFloor)
}

// A query at cosine 0.5 is far above the 0.05 floor, so today it answers. With
// every probe at 0.6 the baseline p90 is 0.6, the threshold follows it, and
// the guard fires.
func TestEvidence1081_GuardFiresUnderHighBaseline(t *testing.T) {
	gen := &fakeGenerator{out: "must not run"}
	emb := &textEmbedder{query: "q", queryVec: unitVec(0.5), probeVec: unitVec(0.6)}
	svc, _ := baselineService(t, gen, emb, 1)
	if svc.CalibratesEvidence() {
		t.Fatal("a service nobody configured must not report that it calibrates")
	}
	svc.SetEvidenceThreshold(true, 0)
	if !svc.CalibratesEvidence() {
		t.Fatal("auto must report that the service calibrates")
	}

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "insufficient" || len(got.Citations) != 0 || gen.lastPrompt != "" {
		t.Fatalf("expected abstention under a 0.6 baseline, got verdict %q citations %d prompt %q", got.EvidenceVerdict, len(got.Citations), gen.lastPrompt)
	}
	if !strings.Contains(got.Answer, "Insufficient evidence") {
		t.Fatalf("answer = %q, want the insufficient-evidence text", got.Answer)
	}
	if emb.probeBatches() != 1 {
		t.Fatalf("probe batches = %d, want exactly one embed call for the probe set", emb.probeBatches())
	}
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", K: 1})
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search: %v (%d hits)", err, len(hits))
	}
	if hits[0].EvidenceVerdict != "insufficient" {
		t.Fatalf("search hit verdict = %q, want insufficient against the calibrated threshold", hits[0].EvidenceVerdict)
	}
}

func TestEvidence1081_GuardSilentUnderLowBaseline(t *testing.T) {
	gen := &fakeGenerator{out: "grounded answer [docs/a.md]"}
	emb := &textEmbedder{query: "q", queryVec: unitVec(0.5), probeVec: unitVec(0.02)}
	svc, _ := baselineService(t, gen, emb, 1)
	svc.SetEvidenceThreshold(true, 0)

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "sufficient" || gen.lastPrompt == "" {
		t.Fatalf("a 0.02 baseline keeps the 0.05 floor: verdict %q, generated=%v", got.EvidenceVerdict, gen.lastPrompt != "")
	}
	report := svc.EvidenceReport(context.Background())
	if report.CosineThreshold != 0.05 || report.CosineThresholdSource != model.EvidenceThresholdSourceFloor {
		t.Fatalf("report = %+v, want the floor 0.05", report)
	}
	if report.Baseline == nil || report.Baseline.Probes == 0 || math.Abs(report.Baseline.P90-0.02) > 1e-3 {
		t.Fatalf("report baseline = %+v, want p90 about 0.02", report.Baseline)
	}
}

func TestEvidence1081_UnconfiguredServiceKeepsTheFloor(t *testing.T) {
	gen := &fakeGenerator{out: "grounded answer [docs/a.md]"}
	emb := &textEmbedder{query: "q", queryVec: unitVec(0.5), probeVec: unitVec(0.9)}
	svc, _ := baselineService(t, gen, emb, 1)
	// No SetEvidenceThreshold call: the shipped floor applies and no probe
	// is embedded, which is the behaviour every older test relies on.
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "sufficient" || emb.probeBatches() != 0 {
		t.Fatalf("unconfigured: verdict %q probe batches %d, want sufficient and 0", got.EvidenceVerdict, emb.probeBatches())
	}
}

func TestEvidence1081_PinOverridesTheBaseline(t *testing.T) {
	gen := &fakeGenerator{out: "grounded answer [docs/a.md]"}
	emb := &textEmbedder{query: "q", queryVec: unitVec(0.5), probeVec: unitVec(0.6)}
	svc, _ := baselineService(t, gen, emb, 1)
	svc.SetEvidenceThreshold(false, 0.1)
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "sufficient" {
		t.Fatalf("pin 0.1 under a 0.6 baseline: verdict %q, want sufficient", got.EvidenceVerdict)
	}
	report := svc.EvidenceReport(context.Background())
	if report.CosineThreshold != 0.1 || report.CosineThresholdSource != model.EvidenceThresholdSourceConfig || report.Baseline == nil {
		t.Fatalf("report = %+v, want pinned 0.1 with the baseline still reported", report)
	}

	svc.SetEvidenceThreshold(false, 0.9)
	gen.lastPrompt = ""
	got, err = svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "insufficient" || gen.lastPrompt != "" {
		t.Fatalf("pin 0.9: verdict %q generated=%v, want abstention", got.EvidenceVerdict, gen.lastPrompt != "")
	}
}

// The baseline is cached in the state dir keyed by model, probe set and chunk
// count: a restart loads it instead of embedding the probes again, and a
// changed chunk count recomputes it.
func TestEvidence1081_StateDirCacheSurvivesRestartAndTracksTheCorpus(t *testing.T) {
	state := t.TempDir()
	gen := &fakeGenerator{out: "grounded answer [docs/a.md]"}
	first := &textEmbedder{query: "q", queryVec: unitVec(0.5), probeVec: unitVec(0.6)}
	svc, _ := baselineService(t, gen, first, 1)
	svc.SetStateDir(state)
	svc.SetEvidenceThreshold(true, 0)
	if _, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	assertCachedBaseline(t, state, 0.6, 1)

	// Restart: a second service over the same state dir and corpus. Its
	// embedder would give a LOW baseline, but the cached high one is loaded,
	// so the guard still fires and no probe batch is embedded.
	second := &textEmbedder{query: "q", queryVec: unitVec(0.5), probeVec: unitVec(0.02)}
	svc2, idx2 := baselineService(t, gen, second, 1)
	svc2.SetStateDir(state)
	svc2.SetEvidenceThreshold(true, 0)
	got, err := svc2.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask after restart: %v", err)
	}
	if got.EvidenceVerdict != "insufficient" || second.probeBatches() != 0 {
		t.Fatalf("restart: verdict %q probe batches %d, want insufficient from the cache and 0", got.EvidenceVerdict, second.probeBatches())
	}

	// The corpus grows: the key changes, the baseline is recomputed with the
	// current embedder, and the low result lets the query through.
	addVec(t, idx2, 2, []float32{1, 0})
	svc2.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "docs/b.md", Snippet: "more", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}})
	got, err = svc2.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask after growth: %v", err)
	}
	if got.EvidenceVerdict != "sufficient" || second.probeBatches() != 1 {
		t.Fatalf("growth: verdict %q probe batches %d, want sufficient after one recompute", got.EvidenceVerdict, second.probeBatches())
	}
	assertCachedBaseline(t, state, 0.02, 2)

	// A new embed identity (a reindex with another model or contextual
	// prompt) changes the key, so the cached baseline is not reused.
	third := &textEmbedder{query: "q", queryVec: unitVec(0.5), probeVec: unitVec(0.6)}
	svc3, idx3 := baselineService(t, gen, third, 1, 2)
	if err := idx3.Reset(context.Background(), "other-provider|other-model|8"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, id := range []uint64{1, 2} {
		addVec(t, idx3, id, []float32{1, 0})
	}
	svc3.SetStateDir(state)
	svc3.SetEvidenceThreshold(true, 0)
	got, err = svc3.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask after identity change: %v", err)
	}
	if got.EvidenceVerdict != "insufficient" || third.probeBatches() != 1 {
		t.Fatalf("identity change: verdict %q probe batches %d, want a recompute (insufficient, 1)", got.EvidenceVerdict, third.probeBatches())
	}
}

// assertCachedBaseline reads the state-dir cache and checks its p90 and chunk
// count, plus that the file carries a probe set and a model name.
func assertCachedBaseline(t *testing.T, state string, wantP90 float64, wantChunks int) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(state, "evidence_baseline.json")); err != nil {
		t.Fatalf("cache file: %v", err)
	}
	cached, err := retrieval.LoadEvidenceBaseline(state)
	if err != nil || cached == nil {
		t.Fatalf("LoadEvidenceBaseline: %v %+v", err, cached)
	}
	if math.Abs(cached.P90-wantP90) > 1e-3 || cached.Chunks != wantChunks || cached.ProbeSet == "" || cached.EmbedModel == "" {
		t.Fatalf("cached baseline = %+v, want p90 %.2f over %d chunk(s) with a probe set and model", cached, wantP90, wantChunks)
	}
}

// An embedder that fails leaves the floor in place and never refuses a
// request for that reason (fail-open, like the rest of the guard).
type failingEmbedder struct{ inner *textEmbedder }

func (e *failingEmbedder) Embed(ctx context.Context, m string, role model.EmbedRole, texts []string) ([][]float32, error) {
	if len(texts) > 1 {
		return nil, context.DeadlineExceeded
	}
	return e.inner.Embed(ctx, m, role, texts)
}

func TestEvidence1081_UnreachableBaselineFailsOpen(t *testing.T) {
	gen := &fakeGenerator{out: "grounded answer [docs/a.md]"}
	emb := &failingEmbedder{inner: &textEmbedder{query: "q", queryVec: unitVec(0.5)}}
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	svc := retrieval.NewService(nil, idx, emb, gen)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/a.md", Snippet: "text", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2}})
	svc.SetEvidenceThreshold(true, 0)
	if err := svc.WarmEvidenceBaseline(context.Background()); err == nil {
		t.Fatal("WarmEvidenceBaseline must report the embed failure")
	}
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.EvidenceVerdict != "sufficient" {
		t.Fatalf("verdict %q, want sufficient against the floor while the baseline is unavailable", got.EvidenceVerdict)
	}
	report := svc.EvidenceReport(context.Background())
	if report.CosineThresholdSource != model.EvidenceThresholdSourceFloor || report.Baseline != nil {
		t.Fatalf("report = %+v, want the floor and no baseline", report)
	}
}
