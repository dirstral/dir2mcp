package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// dirstral-spec#117, SPEC 9.4.5 (spec 0.70.0): when generation fails the server
// publishes the retrieved context in `answer`, and nothing in the payload said
// so. Citations, hits, evidence and indexing_complete are all populated and all
// correct, because retrieval genuinely succeeded, so the only remaining signal
// was the shape of the prose. Two public deployments served context dumps for
// three days behind that gap, caught by an operator script that
// regular-expression-matched the fallback's own wording.

func provenanceService(t *testing.T, gen *fakeGenerator) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})
	var g model.Generator
	if gen != nil {
		g = gen
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, g)
	svc.SetChunkMetadata(1, model.SearchHit{
		ChunkID: 1, RelPath: "docs/a.md", Snippet: "the retrieved sentence",
		Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
	})
	return svc
}

func TestAsk117_AGeneratedAnswerClaimsNothing(t *testing.T) {
	svc := provenanceService(t, &fakeGenerator{out: "a real answer [docs/a.md]"})
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{Query: "q", K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// Absent means generated, so every existing client is unaffected.
	if got.AnswerSource != "" || got.AnswerSourceReason != "" {
		t.Fatalf("generated answer carries provenance %q/%q, want both empty",
			got.AnswerSource, got.AnswerSourceReason)
	}
}

func TestAsk117_AProviderFailureIsReportedNotHidden(t *testing.T) {
	// The outage shape: the provider refuses (out of credit, quota, auth) and
	// the server publishes the retrieved material instead.
	svc := provenanceService(t, &fakeGenerator{err: errors.New("429 quota exceeded")})
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{Query: "q", K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.AnswerSource != "retrieval_only" {
		t.Fatalf("answer_source = %q, want retrieval_only", got.AnswerSource)
	}
	if got.AnswerSourceReason != "generator_unavailable" {
		t.Fatalf("reason = %q, want generator_unavailable", got.AnswerSourceReason)
	}
	// The rest of the payload stays correct, which is exactly why the marking
	// is needed: none of it can carry the signal.
	if len(got.Hits) == 0 || len(got.Citations) == 0 {
		t.Fatalf("a retrieval-only answer must keep its hits and citations, got %d/%d",
			len(got.Hits), len(got.Citations))
	}
	if got.EvidenceVerdict == "insufficient" {
		t.Fatal("evidence was downgraded for a retrieval-only answer; it describes the RETRIEVAL, which succeeded")
	}
	if got.Faithfulness != "unchecked" {
		t.Fatalf("faithfulness = %q, want unchecked: nothing was generated to verify", got.Faithfulness)
	}
}

func TestAsk117_NoGeneratorIsNotAFault(t *testing.T) {
	svc := provenanceService(t, nil)
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{Query: "q", K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.AnswerSource != "retrieval_only" {
		t.Fatalf("answer_source = %q, want retrieval_only", got.AnswerSource)
	}
	// A distinct reason, because the operator action is different: configure
	// one, rather than go and fix a provider that is already configured.
	if got.AnswerSourceReason != "generator_not_configured" {
		t.Fatalf("reason = %q, want generator_not_configured", got.AnswerSourceReason)
	}
}

func TestAsk117_AnEmptyReplyIsAGeneratorError(t *testing.T) {
	// Reached and replied, and the reply could not be used as an answer. A
	// different operator action again: investigate the model, not the link.
	svc := provenanceService(t, &fakeGenerator{out: "   \n  "})
	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{Query: "q", K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.AnswerSource != "retrieval_only" || got.AnswerSourceReason != "generator_error" {
		t.Fatalf("empty reply gave %q/%q, want retrieval_only/generator_error",
			got.AnswerSource, got.AnswerSourceReason)
	}
}

func TestAsk117_TheReasonAndTheSourceTravelTogether(t *testing.T) {
	// SPEC 9.4.5 pairs them, and the published schema states the pairing as a
	// conditional, so half of it would fail validation.
	for _, gen := range []*fakeGenerator{
		{out: "a real answer [docs/a.md]"},
		{err: errors.New("down")},
		{out: ""},
		nil,
	} {
		svc := provenanceService(t, gen)
		got, err := svc.Ask(context.Background(), "q", model.SearchQuery{Query: "q", K: 5})
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if (got.AnswerSource == "") != (got.AnswerSourceReason == "") {
			t.Fatalf("half a pair: source=%q reason=%q", got.AnswerSource, got.AnswerSourceReason)
		}
		if got.AnswerSource != "" && !strings.HasPrefix(got.AnswerSourceReason, "generator_") {
			t.Fatalf("reason %q is outside the 9.4.5 vocabulary", got.AnswerSourceReason)
		}
	}
}
