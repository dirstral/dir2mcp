package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// SPEC 9.4.5 (spec 0.70.0) ON THE WIRE, dirstral-spec#117.
//
// tests/retrieval pins that the service COMPUTES the provenance. It cannot see
// whether serialization carries it, and a field that never reaches
// structuredContent is invisible to every client however correct the retriever
// is. That is the whole point here: an operator's health check is supposed to
// read a field instead of regular-expression-matching the fallback's prose.

func TestAsk117Wire_TheMarkingReachesTheClient(t *testing.T) {
	result := faithBaseResult()
	result.AnswerSource = "retrieval_only"
	result.AnswerSourceReason = "generator_unavailable"
	result.Faithfulness = "unchecked"
	got := callAskStructured(t, result, "dir2mcp_ask", `{"question":"q"}`)
	if got["answer_source"] != "retrieval_only" {
		t.Fatalf("answer_source = %v, want retrieval_only", got["answer_source"])
	}
	if got["answer_source_reason"] != "generator_unavailable" {
		t.Fatalf("answer_source_reason = %v, want generator_unavailable", got["answer_source_reason"])
	}
}

func TestAsk117Wire_AGeneratedAnswerCarriesNeitherField(t *testing.T) {
	// Absent means generated, so an ordinary answer must put nothing on the
	// wire and every existing client is unaffected.
	got := callAskStructured(t, faithBaseResult(), "dir2mcp_ask", `{"question":"q"}`)
	for _, field := range []string{"answer_source", "answer_source_reason"} {
		if _, present := got[field]; present {
			t.Fatalf("%s present on a generated answer: %v", field, got[field])
		}
	}
}

func TestAsk117Wire_HalfAPairIsNeverEmitted(t *testing.T) {
	// The served schema states the pairing as a conditional, so emitting one
	// half would fail a strict client's validation of the WHOLE call, which is
	// the #387 failure class. A reason without a source also says nothing a
	// client can act on.
	for name, result := range map[string]model.AskResult{
		"source without reason": func() model.AskResult {
			r := faithBaseResult()
			r.AnswerSource = "retrieval_only"
			return r
		}(),
		"reason without source": func() model.AskResult {
			r := faithBaseResult()
			r.AnswerSourceReason = "generator_error"
			return r
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			got := callAskStructured(t, result, "dir2mcp_ask", `{"question":"q"}`)
			for _, field := range []string{"answer_source", "answer_source_reason"} {
				if _, present := got[field]; present {
					t.Fatalf("%s emitted from half a pair: %v", field, got[field])
				}
			}
		})
	}
}

// The served schema DECLARING both fields is not asserted here. The
// conformance suite already holds every answer surface's served outputSchema
// to exact property equality with the canonical contract
// (TestTranscribeAndAsk_ServedOutputMatchesTheCanonicalContract_643 and its
// siblings), so a declaration that went missing fails there first, against the
// spec rather than against a list repeated in this file.
