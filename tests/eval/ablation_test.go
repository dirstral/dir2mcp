package tests

import (
	"testing"
)

// evalK is the retrieval cutoff used for recall@k / nDCG@k across the ablation.
const evalK = 5

// configMetrics holds one ablation row's macro-averaged metrics plus the mean
// number of hits returned per query (a proxy for how aggressively a config
// trims the candidate set — relevant for the precision-oriented knobs).
type configMetrics struct {
	cfg      knobConfig
	mean     RankedMetrics
	meanHits float64
}

// ablationMatrix is the set of configurations the harness measures. Each row
// toggles one additional retrieval knob on top of a sensible base so its effect
// is isolated:
//   - vector-only:     dense retrieval only (baseline)
//   - hybrid:          + BM25 lexical RRF fusion
//   - hybrid+rerank:   + deterministic lexical reranker over the fused pool
//   - hybrid+dedup:    + retrieval-time cross-file de-duplication (#265)
//   - hybrid+langfilt: + per-language retrieval filter (#267 item 4)
//   - vector+minscore: vector-only + server-side relevance floor (#305), where
//     scores are interpretable cosine similarities
func ablationMatrix() []knobConfig {
	return []knobConfig{
		{name: "vector-only", hybrid: false},
		{name: "hybrid", hybrid: true},
		{name: "hybrid+rerank", hybrid: true, rerank: true},
		{name: "hybrid+dedup", hybrid: true, crossFileDD: true},
		{name: "hybrid+langfilt", hybrid: true, useLangs: true},
		{name: "vector+minscore", hybrid: false, minScore: 0.7},
	}
}

// runAblation evaluates every config in the matrix over the labeled query set
// and returns one configMetrics row per config, in matrix order.
func runAblation(t *testing.T, corpus evalCorpus) []configMetrics {
	t.Helper()
	rows := make([]configMetrics, 0, len(ablationMatrix()))
	for _, cfg := range ablationMatrix() {
		svc := corpus.buildService(cfg)
		perQuery := make([]RankedMetrics, 0, len(corpus.queries))
		var totalHits int
		for _, q := range corpus.queries {
			retrieved, err := corpus.runQuery(svc, q, cfg, evalK)
			if err != nil {
				t.Fatalf("config %q query %q: %v", cfg.name, q.ID, err)
			}
			totalHits += len(retrieved)
			perQuery = append(perQuery, Evaluate(retrieved, relevantSet(q.Relevant), evalK))
		}
		rows = append(rows, configMetrics{
			cfg:      cfg,
			mean:     MeanMetrics(perQuery),
			meanHits: float64(totalHits) / float64(len(corpus.queries)),
		})
	}
	return rows
}

func metricsFor(rows []configMetrics, name string) configMetrics {
	for _, r := range rows {
		if r.cfg.name == name {
			return r
		}
	}
	panic("config not found: " + name)
}

// TestAblationMatrix is the headline deliverable: it runs the full ablation over
// the deterministic, creds-free fixture corpus, logs a metrics table per
// configuration (recall@k / nDCG@k / MRR, plus mean hits), and asserts the
// quality invariants each knob should uphold so a regression fails CI.
func TestAblationMatrix(t *testing.T) {
	corpus, err := loadCorpus("testdata")
	if err != nil {
		t.Fatalf("load fixtures: %v", err)
	}
	rows := runAblation(t, corpus)

	t.Logf("retrieval ablation @k=%d over %d queries:", evalK, len(corpus.queries))
	t.Logf("%-18s %8s %8s %8s %9s", "config", "recall", "nDCG", "MRR", "meanHits")
	for _, r := range rows {
		t.Logf("%-18s %8.4f %8.4f %8.4f %9.2f",
			r.cfg.name, r.mean.RecallAtK, r.mean.NDCGAtK, r.mean.MRR, r.meanHits)
	}

	assertBaselineFindsEverything(t, rows)
	assertHybridImprovesRanking(t, rows)
	assertRerankImprovesRanking(t, rows)
	assertDedupTrimsWithoutRecallLoss(t, rows)
	assertLangFilterDoesNotHurtRecall(t, rows)
	assertMinScoreTrimsWithoutRecallLoss(t, rows)
}

// assertBaselineFindsEverything: the corpus is built so every relevant doc is
// retrievable in the top-k by the vector baseline (recall@k == 1).
func assertBaselineFindsEverything(t *testing.T, rows []configMetrics) {
	t.Helper()
	base := metricsFor(rows, "vector-only")
	if !almostEqual(base.mean.RecallAtK, 1.0) {
		t.Fatalf("vector-only baseline must find every relevant doc in top-%d (recall=%.4f); fixtures drifted",
			evalK, base.mean.RecallAtK)
	}
}

// assertHybridImprovesRanking: BM25+vector RRF fusion lifts a doc the dense axis
// ranks below an off-topic neighbor (q_photosynthesis), so hybrid's nDCG and MRR
// strictly exceed the vector-only baseline.
func assertHybridImprovesRanking(t *testing.T, rows []configMetrics) {
	t.Helper()
	base := metricsFor(rows, "vector-only")
	hyb := metricsFor(rows, "hybrid")
	if !(hyb.mean.MRR > base.mean.MRR) {
		t.Fatalf("hybrid must improve MRR over vector-only: base=%.4f hybrid=%.4f", base.mean.MRR, hyb.mean.MRR)
	}
	if !(hyb.mean.NDCGAtK > base.mean.NDCGAtK) {
		t.Fatalf("hybrid must improve nDCG over vector-only: base=%.4f hybrid=%.4f", base.mean.NDCGAtK, hyb.mean.NDCGAtK)
	}
}

// assertRerankImprovesRanking: the deterministic lexical reranker is at least as
// good as plain hybrid on ranking quality (it reorders the fused pool toward the
// lexically-best candidate) and never regresses recall.
func assertRerankImprovesRanking(t *testing.T, rows []configMetrics) {
	t.Helper()
	hyb := metricsFor(rows, "hybrid")
	rr := metricsFor(rows, "hybrid+rerank")
	if rr.mean.MRR < hyb.mean.MRR-eps {
		t.Fatalf("rerank must not regress MRR vs hybrid: hybrid=%.4f rerank=%.4f", hyb.mean.MRR, rr.mean.MRR)
	}
	if rr.mean.RecallAtK < hyb.mean.RecallAtK-eps {
		t.Fatalf("rerank must not regress recall vs hybrid: hybrid=%.4f rerank=%.4f", hyb.mean.RecallAtK, rr.mean.RecallAtK)
	}
}

// assertDedupTrimsWithoutRecallLoss: cross-file dedup collapses the byte-
// identical gardening alias, shrinking the mean hit count, while the canonical
// relevant doc survives so recall/MRR are unchanged.
func assertDedupTrimsWithoutRecallLoss(t *testing.T, rows []configMetrics) {
	t.Helper()
	hyb := metricsFor(rows, "hybrid")
	dd := metricsFor(rows, "hybrid+dedup")
	if !(dd.meanHits < hyb.meanHits) {
		t.Fatalf("cross-file dedup must shrink the candidate set: hybrid=%.2f dedup=%.2f", hyb.meanHits, dd.meanHits)
	}
	if dd.mean.RecallAtK < hyb.mean.RecallAtK-eps {
		t.Fatalf("cross-file dedup must not drop a canonical relevant doc: hybrid=%.4f dedup=%.4f",
			hyb.mean.RecallAtK, dd.mean.RecallAtK)
	}
}

// assertLangFilterDoesNotHurtRecall: the per-language filter excludes the
// other-language gardening translations (trimming the set) but the labeled
// relevant docs are all English, so recall is preserved.
func assertLangFilterDoesNotHurtRecall(t *testing.T, rows []configMetrics) {
	t.Helper()
	hyb := metricsFor(rows, "hybrid")
	lf := metricsFor(rows, "hybrid+langfilt")
	if lf.mean.RecallAtK < hyb.mean.RecallAtK-eps {
		t.Fatalf("language filter must not drop relevant in-language docs: hybrid=%.4f langfilt=%.4f",
			hyb.mean.RecallAtK, lf.mean.RecallAtK)
	}
	if !(lf.meanHits <= hyb.meanHits) {
		t.Fatalf("language filter must not grow the candidate set: hybrid=%.2f langfilt=%.2f", hyb.meanHits, lf.meanHits)
	}
}

// assertMinScoreTrimsWithoutRecallLoss: a 0.7 relevance floor on the vector path
// drops low-similarity noise (shrinking mean hits) while keeping every
// high-similarity relevant doc, so recall@k stays at the baseline.
func assertMinScoreTrimsWithoutRecallLoss(t *testing.T, rows []configMetrics) {
	t.Helper()
	base := metricsFor(rows, "vector-only")
	ms := metricsFor(rows, "vector+minscore")
	if !(ms.meanHits < base.meanHits) {
		t.Fatalf("min_score floor must trim low-score noise: base=%.2f minscore=%.2f", base.meanHits, ms.meanHits)
	}
	if ms.mean.RecallAtK < base.mean.RecallAtK-eps {
		t.Fatalf("min_score floor must not drop high-similarity relevant docs: base=%.4f minscore=%.4f",
			base.mean.RecallAtK, ms.mean.RecallAtK)
	}
}
