package model

import (
	"fmt"
	"math"
)

// ValidateEmbedVectors is the shared provider-output boundary check for
// embedding vectors (issue #703). Every embed adapter runs its response through
// it before returning, so a malformed vector is rejected at the edge instead of
// being persisted by an index backend.
//
// A vector is malformed when it is:
//
//   - empty — nothing to score against;
//   - the wrong length, when wantDim > 0 names a dimension the caller requested
//     (SPEC 8.1.6) or has already fixed for this response;
//   - non-finite — a NaN or ±Inf component poisons every distance computation
//     downstream (NaN comparisons are unordered, so such a vector can both never
//     rank and corrupt a partial sort);
//   - zero-norm — an all-zero vector has NO cosine direction. It is not a weak
//     embedding, it is the absence of one: cosine/inner-product scoring returns
//     0 against every query, so the chunk is permanently unretrievable, yet once
//     written it is indistinguishable from healthy indexed data (external
//     backends keep it, and it can even fix a collection's dimension). Providers
//     and proxies emit these on quota exhaustion, on truncated inputs, and from
//     broken self-hosted servers, and the failure is otherwise SILENT — a
//     200 OK that lands as embedding_status=embedded.
//
// The returned error is a NON-RETRYABLE *ProviderError: the same response would
// come back on retry, and the embedding worker's classifier routes it to
// store.ErrorCategoryEmbeddingFailure so the affected chunk is recorded FAILED
// with a reason (visible in `dir2mcp status`/`doctor`) rather than silently
// indexed. In a multi-input batch the worker's bisection (#399) isolates the
// offending input so healthy siblings still embed. On the query side the same
// error surfaces as a provider error instead of a result set scored entirely at
// zero.
//
// code is the caller's provider error code (e.g. "OPENAI_FAILED") so the message
// keeps naming the provider that produced the bad vector.
//
// The message deliberately does NOT carry the vector's position in the batch:
// these strings are keyword-classified downstream (store.ClassifyError /
// IsTransientError), where a bare index that happened to be 503 or 429 would
// read as a transient upstream status and leave the chunk PENDING forever. A
// batch index walks every integer, so that collision is a matter of batch size;
// the worker's bisection identifies the individual chunk anyway.
func ValidateEmbedVectors(code string, wantDim int, vectors [][]float32) error {
	for _, v := range vectors {
		if err := validateEmbedVector(code, wantDim, v); err != nil {
			return err
		}
	}
	return nil
}

// validateEmbedVector checks one vector; split out to keep ValidateEmbedVectors
// a single loop and stay inside the cyclomatic-complexity budget.
func validateEmbedVector(code string, wantDim int, v []float32) error {
	bad := func(problem string) error {
		return &ProviderError{
			Code:      code,
			Message:   "malformed embedding vector in provider response: " + problem,
			Retryable: false,
		}
	}
	if len(v) == 0 {
		return bad("empty (no components)")
	}
	if wantDim > 0 && len(v) != wantDim {
		return bad(fmt.Sprintf("has %d dimensions, want %d", len(v), wantDim))
	}
	var sum float64
	for _, x := range v {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return bad("contains a non-finite component (NaN or Inf)")
		}
		sum += f * f
	}
	if sum == 0 {
		return bad("all components are zero, so the vector has no cosine direction")
	}
	return nil
}
