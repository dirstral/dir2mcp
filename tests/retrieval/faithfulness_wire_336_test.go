package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// SPEC §9.4.4 (spec 0.57.0): the answer-level verdict on the wire.
//
// §9.4.3's `evidence` reports whether the RETRIEVAL was relevant. It cannot
// report whether the answer says what the retrieved passages say, so a client
// keying on `evidence` alone reads "sufficient" on a refusal and takes it for
// an answer. These pin the field that closes that gap, and the property that
// makes it worth having: the two verdicts move independently.

func TestAsk336_WithheldAnswerReportsUnsupported(t *testing.T) {
	gen := &faithScriptedGenerator{
		answer:  "Buddy Kennedy was ejected from the game.",
		verdict: faithVerifierToken,
	}
	svc := faith336Service(t, gen)
	svc.SetVerifyFaithfulness(true)

	got, err := svc.Ask(context.Background(), "Who was ejected?", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Faithfulness != "unsupported" {
		t.Fatalf("Faithfulness = %q, want unsupported", got.Faithfulness)
	}
	// The point of the field: evidence still says the retrieval was fine, so
	// the two verdicts disagree and only this one exposes the refusal.
	if got.EvidenceVerdict != "sufficient" {
		t.Fatalf("EvidenceVerdict = %q, want sufficient", got.EvidenceVerdict)
	}
	if !strings.Contains(got.Answer, faithRefusalMarker) {
		t.Fatalf("answer = %q, want the refusal", got.Answer)
	}
}

func TestAsk336_PublishedAnswerReportsVerified(t *testing.T) {
	gen := &faithScriptedGenerator{
		answer:  "Buddy Kennedy challenged the pitch and the call was confirmed.",
		verdict: "SUPPORTED",
	}
	svc := faith336Service(t, gen)
	svc.SetVerifyFaithfulness(true)

	got, err := svc.Ask(context.Background(), "What did Kennedy do?", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Faithfulness != "verified" {
		t.Fatalf("Faithfulness = %q, want verified", got.Faithfulness)
	}
}

// TestAsk336_OffReportsUnchecked: "unchecked" is not a weak "verified". §9.4.4
// defines it as carrying no answer-level judgement, so a server that never
// checked MUST NOT look like one that checked and passed.
func TestAsk336_OffReportsUnchecked(t *testing.T) {
	gen := &faithScriptedGenerator{
		answer:  "Buddy Kennedy was ejected from the game.",
		verdict: faithVerifierToken,
	}
	svc := faith336Service(t, gen)

	got, err := svc.Ask(context.Background(), "Who was ejected?", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Faithfulness != "unchecked" {
		t.Fatalf("Faithfulness = %q, want unchecked", got.Faithfulness)
	}
}

// TestAsk336_VerifierOutageReportsUnchecked is the other cause §9.4.4 names,
// and the reason "unchecked" is not defined as "the check did not run": here
// it DID run and could not complete. The answer is published, so the verdict
// must not imply it was validated.
func TestAsk336_VerifierOutageReportsUnchecked(t *testing.T) {
	gen := &faithScriptedGenerator{
		answer: "Buddy Kennedy was ejected from the game.",
		verr:   errors.New("provider unavailable"),
	}
	svc := faith336Service(t, gen)
	svc.SetVerifyFaithfulness(true)

	got, err := svc.Ask(context.Background(), "Who was ejected?", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Faithfulness != "unchecked" {
		t.Fatalf("Faithfulness = %q, want unchecked", got.Faithfulness)
	}
	if !strings.Contains(got.Answer, "ejected") {
		t.Fatalf("fail-open broken: answer = %q", got.Answer)
	}
}

// TestAsk336_UnparseableVerdictReportsUnchecked: a verifier that answers
// neither token has not answered the question asked. Reporting "verified"
// would manufacture an approval from a non-response.
func TestAsk336_UnparseableVerdictReportsUnchecked(t *testing.T) {
	gen := &faithScriptedGenerator{
		answer:  "Buddy Kennedy was ejected from the game.",
		verdict: "I am not sure.",
	}
	svc := faith336Service(t, gen)
	svc.SetVerifyFaithfulness(true)

	got, err := svc.Ask(context.Background(), "Who was ejected?", model.SearchQuery{K: 1})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Faithfulness != "unchecked" {
		t.Fatalf("Faithfulness = %q, want unchecked", got.Faithfulness)
	}
	if !strings.Contains(got.Answer, "ejected") {
		t.Fatalf("a non-response must not withhold the answer: %q", got.Answer)
	}
}

// TestAsk336_TheVocabularyIsClosed guards the wire contract: any value outside
// the spec's three would fail a client validating against the served schema.
func TestAsk336_TheVocabularyIsClosed(t *testing.T) {
	allowed := map[string]bool{"verified": true, "unsupported": true, "unchecked": true}
	cases := []struct {
		name string
		gen  *faithScriptedGenerator
		on   bool
	}{
		{"withheld", &faithScriptedGenerator{answer: "a", verdict: faithVerifierToken}, true},
		{"published", &faithScriptedGenerator{answer: "a", verdict: "SUPPORTED"}, true},
		{"off", &faithScriptedGenerator{answer: "a", verdict: "SUPPORTED"}, false},
		{"outage", &faithScriptedGenerator{answer: "a", verr: errors.New("down")}, true},
		{"garbage", &faithScriptedGenerator{answer: "a", verdict: "maybe?"}, true},
	}
	for _, tc := range cases {
		svc := faith336Service(t, tc.gen)
		svc.SetVerifyFaithfulness(tc.on)
		got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 1})
		if err != nil {
			t.Fatalf("%s: Ask: %v", tc.name, err)
		}
		if !allowed[got.Faithfulness] {
			t.Errorf("%s: Faithfulness = %q, outside the closed vocabulary", tc.name, got.Faithfulness)
		}
	}
}
