package tests

import (
	"testing"
)

// evalK is the retrieval cutoff used for recall@k / nDCG@k across the ablation.
const evalK = 5

// configMetrics holds one ablation row's macro-averaged metrics plus the mean
// number of hits returned per query (a proxy for how aggressively a config
// trims the candidate set — relevant for the precision-oriented knobs) and the
// total number of cross-file duplicate hits (two returned rel_paths sharing a
// content_hash) summed over the query set — the direct signal cross-file dedup
// is supposed to drive to zero.
type configMetrics struct {
	cfg            knobConfig
	mean           RankedMetrics
	meanHits       float64
	crossFileDupes int
}

// countCrossFileDupes counts how many returned rel_paths are cross-file
// duplicates of an already-seen result — i.e. share a content_hash with a
// higher-ranked hit. rel_paths with an unknown/empty content_hash are never
// grouped (each is its own group), mirroring dedupCrossFileCandidates. The
// return value is (# returned rel_paths − # distinct content groups), so 0 means
// every source file appears at most once.
func countCrossFileDupes(relPaths []string, hashByRelPath map[string]string) int {
	seen := make(map[string]struct{}, len(relPaths))
	dupes := 0
	for _, rp := range relPaths {
		key := hashByRelPath[rp]
		if key == "" {
			key = "\x00relpath\x00" + rp // unknown/empty hash: never grouped
		}
		if _, ok := seen[key]; ok {
			dupes++
			continue
		}
		seen[key] = struct{}{}
	}
	return dupes
}

// ablationMatrix is the set of configurations the harness measures. Each row
// toggles one additional retrieval knob on top of a sensible base so its effect
// is isolated:
//   - vector-only:     dense retrieval only (baseline)
//   - hybrid:          + BM25 lexical RRF fusion
//   - hybrid+rerank:   + deterministic lexical reranker over the fused pool
//   - hybrid+dedup:    + retrieval-time cross-file de-duplication (#265)
//   - hybrid+langfilt: + per-language retrieval filter (#267 item 4)
//   - vector+minscore: vector-only + server-side relevance floor (#305), applied
//     on each score's ratio to the result set's best hit (#411, #858), so a
//     modest relative floor trims the low-relevance tail without dropping strong
//     hits
func ablationMatrix() []knobConfig {
	return []knobConfig{
		{name: "vector-only", hybrid: false},
		{name: "hybrid", hybrid: true},
		{name: "hybrid+rerank", hybrid: true, rerank: true},
		{name: "hybrid+dedup", hybrid: true, crossFileDD: true},
		{name: "hybrid+langfilt", hybrid: true, useLangs: true},
		{name: "vector+minscore", hybrid: false, minScore: 0.1},
	}
}

// runAblation evaluates every config in the matrix over the labeled query set
// and returns one configMetrics row per config, in matrix order.
func runAblation(t *testing.T, corpus evalCorpus) []configMetrics {
	t.Helper()
	hashByRelPath := make(map[string]string, len(corpus.docs))
	for _, d := range corpus.docs {
		hashByRelPath[d.RelPath] = d.ContentHash
	}
	rows := make([]configMetrics, 0, len(ablationMatrix()))
	for _, cfg := range ablationMatrix() {
		svc, err := corpus.buildService(cfg)
		if err != nil {
			t.Fatalf("buildService(%s): %v", cfg.name, err)
		}
		perQuery := make([]RankedMetrics, 0, len(corpus.queries))
		var totalHits, dupes int
		for _, q := range corpus.queries {
			retrieved, err := corpus.runQuery(svc, q, cfg, evalK)
			if err != nil {
				t.Fatalf("config %q query %q: %v", cfg.name, q.ID, err)
			}
			totalHits += len(retrieved)
			dupes += countCrossFileDupes(retrieved, hashByRelPath)
			perQuery = append(perQuery, Evaluate(retrieved, relevantSet(q.Relevant), evalK))
		}
		rows = append(rows, configMetrics{
			cfg:            cfg,
			mean:           MeanMetrics(perQuery),
			meanHits:       float64(totalHits) / float64(len(corpus.queries)),
			crossFileDupes: dupes,
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
	t.Logf("%-18s %8s %8s %8s %9s %9s", "config", "recall", "nDCG", "MRR", "meanHits", "xfileDup")
	for _, r := range rows {
		t.Logf("%-18s %8.4f %8.4f %8.4f %9.2f %9d",
			r.cfg.name, r.mean.RecallAtK, r.mean.NDCGAtK, r.mean.MRR, r.meanHits, r.crossFileDupes)
	}

	assertBaselineFindsEverything(t, rows)
	assertHybridImprovesRanking(t, rows)
	assertRerankImprovesRanking(t, rows)
	assertDedupRemovesCrossFileDuplicates(t, rows)
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
//
// Since the #519 over-fetch fix, hybrid fusion keeps a rerank-sized candidate
// pool instead of truncating to k, so the reranker now reorders the full
// over-fetch pool (and can rescue a chunk fused at rank k+1..pool) rather than
// only permuting the top-k. The invariant stays a no-regression FLOOR (rerank
// must never do worse than plain hybrid): asserting a strict improvement would
// over-fit this small fixture, and the floor already fails loudly if the wider
// pool ever let rerank drop a relevant doc.
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

// assertDedupRemovesCrossFileDuplicates: cross-file dedup collapses the byte-
// identical gardening alias so no source file appears twice in the top-k, while
// the canonical relevant doc survives so recall is unchanged.
//
// This assertion was re-baselined for the #519 over-fetch fix. Previously hybrid
// fusion truncated to the final k, so cross-file dedup returned strictly FEWER
// hits than plain hybrid (it dropped the alias without anything to backfill), and
// the gate asserted `dedup.meanHits < hybrid.meanHits`. The over-fetch fix widens
// the fused candidate pool to a rerank-sized set: dedup now removes the alias and
// backfills to k with the next UNIQUE cross-file result, so both configs return k
// hits and the old "fewer hits" invariant no longer holds (and was never the real
// contract — it was a proxy). The correct, stronger property is asserted directly:
//   - hybrid+dedup returns ZERO cross-file duplicates (every content_hash appears
//     at most once in the deduped top-k) — the property dedup exists to guarantee;
//   - plain hybrid DOES surface at least one cross-file duplicate, so the gate is
//     non-vacuous and would genuinely fail if dedup ever stopped collapsing them;
//   - recall is not reduced (the canonical relevant doc still survives).
func assertDedupRemovesCrossFileDuplicates(t *testing.T, rows []configMetrics) {
	t.Helper()
	hyb := metricsFor(rows, "hybrid")
	dd := metricsFor(rows, "hybrid+dedup")
	// Guard against a vacuous gate: without dedup the corpus/query set must
	// actually surface a cross-file duplicate into the top-k, otherwise
	// asserting dedup==0 proves nothing.
	if hyb.crossFileDupes == 0 {
		t.Fatalf("ablation fixtures must surface a cross-file duplicate without dedup (got 0); fixtures drifted")
	}
	if dd.crossFileDupes != 0 {
		t.Fatalf("cross-file dedup must leave no duplicate source file in the top-k: hybrid=%d dedup=%d",
			hyb.crossFileDupes, dd.crossFileDupes)
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

// assertMinScoreTrimsWithoutRecallLoss: a modest relative relevance floor (0.1,
// applied on each score's ratio to the result set's best hit, #411/#858) on the
// vector path drops the low-relevance tail (shrinking mean hits) while keeping
// every high-similarity relevant doc, so recall@k stays at the baseline. The
// trimmed hits score below a tenth of the best one, so they are weak in their
// own right and not merely last.
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
