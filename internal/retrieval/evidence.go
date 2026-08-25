package retrieval

import (
	"fmt"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Insufficient-evidence guard (SPEC §9.4.3).
//
// The relevance floor (config `retrieval.min_score`, applyMinScoreFloor) is a
// RELATIVE pruning control: it compares each hit against the BEST score of the
// same result set, as a ratio, which is what makes it scale-free across the
// cosine / RRF / rerank modes (#411). That comparison also makes it
// structurally incapable of reporting insufficiency: the top-scoring hit maps to
// 1.0 by construction, so some hit always clears any floor whenever the set is
// non-empty, and a uniformly weak result set survives in full. A relative floor
// can prune weaker hits relative to the best one; it can never say the best one
// is itself too weak.
//
// §9.4.3 therefore requires the two controls to be separate. The pruning floor
// stays relative and selects the eligible set; the threshold that triggers
// abstention is ABSOLUTE, evaluated against a signal whose meaning does not
// depend on the other hits in the same response.
//
// SIGNAL. model.SearchHit.EvidenceScore, tagged with the scale it is on in
// model.SearchHit.EvidenceScale. It is recorded at the only two stages that
// score a (query, chunk) pair directly:
//
//	"cosine" - the vector index's query/chunk cosine similarity, recorded in
//	           searchHitFromIndexHit before fusion can overwrite Score.
//	"rerank" - the reranker's own relevance score, recorded in rerankPool; it
//	           supersedes the cosine reading for the hits the provider scored.
//
// A hit with no absolute signal carries an empty scale. That is the BM25-only
// fused candidate: an FTS5 bm25() score is corpus-relative and unbounded, and a
// rank-based RRF score encodes rank rather than relevance (the top hit of any
// query scores 1/(rrfK+1) no matter how irrelevant it is), so neither is a
// usable absolute reading.
//
// SCALE AND SHIPPED VALUES. See evidenceThresholds below. The thresholds are
// server constants, NOT operator-configurable: `retrieval.min_score` configures
// the pruning floor only (§9.4.3, "Configuration").
//
// AGGREGATION. The eligible set clears the threshold when its STRONGEST hit
// does, each hit measured against the threshold for its OWN scale. Measuring per
// hit is required rather than cosmetic: §9.1.1 lets one response carry scores
// from several scales at once (rerankPool appends the un-reranked tail of the
// fused pool after the reranked head), and one threshold applied across mixed
// scales would admit weak hits on one scale while rejecting strong hits on
// another.
const (
	evidenceScaleCosine = "cosine"
	evidenceScaleRerank = "rerank"
)

// evidenceThresholds maps an evidence scale to the absolute minimum signal a hit
// must reach to count as evidence. The values are deliberately conservative:
//
//   - cosine 0.05. Embedding cosine baselines are provider-dependent (the
//     similarity an unrelated pair scores differs by an order of magnitude
//     between embedding families), so a threshold that generalizes across
//     providers can only reject near-orthogonal evidence. Raising it would make
//     the guard silently corpus- and provider-specific, which is the failure the
//     scale-free relative floor was introduced to avoid.
//   - rerank 0.02. A cross-encoder's relevance score is calibrated for the
//     (query, chunk) pair rather than the corpus, so a low reading is meaningful
//     on its own; providers put clearly-irrelevant pairs well below this.
//
// A scale absent from this map has no shipped threshold and is treated as
// "no absolute signal" (see classifyEvidence).
var evidenceThresholds = map[string]float64{
	evidenceScaleCosine: 0.05,
	evidenceScaleRerank: 0.02,
}

// evidenceVerdict is the outcome of the absolute insufficient-evidence test.
type evidenceVerdict int

const (
	// evidenceSufficient: at least one eligible hit reached the absolute
	// threshold for its own scale. Generate normally.
	evidenceSufficient evidenceVerdict = iota
	// evidenceInsufficient: every eligible hit carried an absolute signal and
	// none reached its threshold. Abstain (§9.4.3).
	evidenceInsufficient
	// evidenceUnknown: no eligible hit carried an absolute signal at all (a
	// purely lexical result set). Nothing can be asserted about strength, so the
	// guard fails OPEN and generation proceeds: abstaining here would suppress
	// answers on a corpus whose vector index is simply unavailable, which is a
	// worse failure than answering with a documented blind spot.
	evidenceUnknown
)

// Wire names for the evidence verdict (SPEC §9.4.3, spec 0.55.0). One closed
// vocabulary shared by the per-hit field and the ask-level aggregate. "strong"
// is part of the spec vocabulary but is NOT emitted here: the spec lets a
// server emit it only if it documents a stronger per-scale threshold than the
// abstention one, and this server documents only the abstention thresholds
// above. Adding a strong tier is a thresholds decision, not a wiring change.
const (
	verdictSufficient   = "sufficient"
	verdictInsufficient = "insufficient"
	verdictUnknown      = "unknown"
)

// hitEvidenceVerdict names the absolute verdict for ONE hit: its EvidenceScore
// measured against the shipped threshold for its own scale, or "unknown" when
// the hit carries no absolute signal (empty or unrecognized scale). This is the
// per-hit half of the §9.4.3 exposure (#785); the aggregate half reuses
// classifyEvidence below so the two can never disagree.
func hitEvidenceVerdict(h model.SearchHit) string {
	threshold, ok := evidenceThresholds[h.EvidenceScale]
	if !ok {
		return verdictUnknown
	}
	if h.EvidenceScore >= threshold {
		return verdictSufficient
	}
	return verdictInsufficient
}

// stampEvidenceVerdicts writes each hit's named verdict onto the slice, in
// place, just before hits leave retrieval. Stamped here rather than in the MCP
// layer so the verdict and the thresholds it is measured against live in one
// package and cannot drift.
func stampEvidenceVerdicts(hits []model.SearchHit) {
	for i := range hits {
		hits[i].EvidenceVerdict = hitEvidenceVerdict(hits[i])
	}
}

// aggregateEvidenceVerdict names the verdict of the ELIGIBLE set, by the
// normative aggregation of spec 0.55.0: the strongest eligible hit's verdict,
// with "unknown" only when NO eligible hit carries an absolute signal. It is
// classifyEvidence with a wire name, and delegates to it so the exposed
// aggregate and the abstention decision cannot diverge.
func aggregateEvidenceVerdict(hits []model.SearchHit) string {
	switch classifyEvidence(hits) {
	case evidenceSufficient:
		return verdictSufficient
	case evidenceInsufficient:
		return verdictInsufficient
	default:
		return verdictUnknown
	}
}

// classifyEvidence applies the absolute insufficient-evidence test (§9.4.3) to
// the eligible hit set. See the package comment above for the signal, its scale,
// the shipped thresholds and the aggregation rule.
func classifyEvidence(hits []model.SearchHit) evidenceVerdict {
	scored := false
	for _, h := range hits {
		threshold, ok := evidenceThresholds[h.EvidenceScale]
		if !ok {
			continue
		}
		scored = true
		if h.EvidenceScore >= threshold {
			return evidenceSufficient
		}
	}
	if !scored {
		return evidenceUnknown
	}
	return evidenceInsufficient
}

// insufficientEvidenceAnswer is the answer text returned when retrieval found
// candidates but none of them cleared the absolute evidence threshold.
//
// §9.4.3 requires a caller to be able to tell abstention apart from an empty
// corpus result: both return an empty citations array, but "I found nothing" and
// "I found material and judged it too weak" are different answers to the
// operator. The distinction is carried in the answer TEXT rather than in a new
// structured field so the ask result shape (`ask.json` marks question, answer,
// citations, hits and indexing_complete required) is unchanged for strict
// clients; the retrieved candidates remain visible in `hits`, so a caller can
// still inspect exactly what was rejected.
func insufficientEvidenceAnswer(candidates int) string {
	noun := "candidate passages"
	if candidates == 1 {
		noun = "candidate passage"
	}
	return fmt.Sprintf(
		"Insufficient evidence to answer: retrieval returned %d %s from the indexed corpus, "+
			"but none met the minimum relevance required to ground an answer. "+
			"Not answering rather than guessing; see `hits` for the rejected candidates.",
		candidates, noun,
	)
}
