// Package tests holds the offline retrieval-quality eval + ablation harness
// (issue #322). It is an EXTERNAL test package (package name "tests", import
// path tests/eval) so it exercises only the public retrieval.Service surface.
//
// This file defines the pure ranking-quality metrics — recall@k, nDCG@k, and
// MRR — used by the ablation runner. They take a ranked list of retrieved
// document keys and the set of relevant keys for a query, and are deliberately
// free of any dir2mcp types so they can be unit-tested in isolation and reused
// for any ranked-retrieval evaluation.
package tests

import (
	"math"
	"sort"
)

// RankedMetrics bundles the per-query ranking-quality scores computed over one
// retrieved list at a given cutoff k.
type RankedMetrics struct {
	RecallAtK float64 // fraction of relevant items present in the top-k
	NDCGAtK   float64 // normalized discounted cumulative gain over the top-k
	MRR       float64 // reciprocal rank of the first relevant item (any depth)
}

// RecallAtK returns the fraction of the relevant set that appears within the
// first k retrieved keys. It is 0 when there are no relevant items (an
// undefined ratio is reported as 0 so it can be averaged) and clamps k to the
// length of retrieved. Duplicate retrieved keys are counted once.
func RecallAtK(retrieved []string, relevant map[string]struct{}, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	if k < 0 {
		k = 0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}
	found := make(map[string]struct{}, k)
	for i := 0; i < k; i++ {
		key := retrieved[i]
		if _, ok := relevant[key]; ok {
			found[key] = struct{}{}
		}
	}
	return float64(len(found)) / float64(len(relevant))
}

// MRR returns the reciprocal rank of the first relevant retrieved key (1-based
// rank), or 0 when no relevant key is retrieved at any depth. The full
// retrieved list is considered (no cutoff), matching the standard MRR
// definition.
func MRR(retrieved []string, relevant map[string]struct{}) float64 {
	if len(relevant) == 0 {
		return 0
	}
	for i, key := range retrieved {
		if _, ok := relevant[key]; ok {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// NDCGAtK returns the normalized discounted cumulative gain over the first k
// retrieved keys, using binary relevance (gain 1 for a relevant key, 0
// otherwise) and the standard log2(rank+1) discount. The ideal DCG normalizer
// is computed from min(len(relevant), k) perfectly-ranked relevant items, so a
// perfect ranking scores 1.0 even when there are more relevant items than k.
// Returns 0 when there are no relevant items. Duplicate relevant keys already
// credited earlier in the list are not double-counted.
func NDCGAtK(retrieved []string, relevant map[string]struct{}, k int) float64 {
	if len(relevant) == 0 {
		return 0
	}
	if k < 0 {
		k = 0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}
	credited := make(map[string]struct{}, k)
	dcg := 0.0
	for i := 0; i < k; i++ {
		key := retrieved[i]
		if _, ok := relevant[key]; !ok {
			continue
		}
		if _, dup := credited[key]; dup {
			continue
		}
		credited[key] = struct{}{}
		dcg += 1.0 / math.Log2(float64(i+2)) // rank is i+1, discount log2(rank+1)
	}
	idealHits := len(relevant)
	if idealHits > k {
		idealHits = k
	}
	idcg := 0.0
	for i := 0; i < idealHits; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// Evaluate computes all three ranking metrics for one retrieved list at cutoff
// k against the relevant set.
func Evaluate(retrieved []string, relevant map[string]struct{}, k int) RankedMetrics {
	return RankedMetrics{
		RecallAtK: RecallAtK(retrieved, relevant, k),
		NDCGAtK:   NDCGAtK(retrieved, relevant, k),
		MRR:       MRR(retrieved, relevant),
	}
}

// MeanMetrics averages a slice of per-query RankedMetrics into a single macro
// average (each query weighted equally). An empty input yields a zero value.
func MeanMetrics(perQuery []RankedMetrics) RankedMetrics {
	n := len(perQuery)
	if n == 0 {
		return RankedMetrics{}
	}
	var sum RankedMetrics
	for _, m := range perQuery {
		sum.RecallAtK += m.RecallAtK
		sum.NDCGAtK += m.NDCGAtK
		sum.MRR += m.MRR
	}
	div := float64(n)
	return RankedMetrics{
		RecallAtK: sum.RecallAtK / div,
		NDCGAtK:   sum.NDCGAtK / div,
		MRR:       sum.MRR / div,
	}
}

// relevantSet builds a set from a slice of relevant keys, dropping empties.
// Exposed via the harness for fixture loading; kept here next to the metrics it
// feeds. Sorting the input first keeps construction deterministic for callers
// that range over the result.
func relevantSet(keys []string) map[string]struct{} {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	set := make(map[string]struct{}, len(sorted))
	for _, k := range sorted {
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	return set
}
