package retrieval

import (
	"context"
	"strings"

	"github.com/dirstral/dir2mcp/internal/promptfence"
	"github.com/dirstral/dir2mcp/internal/usage"
)

// Runtime citation-faithfulness verification (issue #336).
//
// The absolute evidence threshold (§9.4.3) answers "is there relevant material
// here". It cannot answer "does the answer actually say what the material
// says", and those come apart in the way that matters most.
//
// MEASURED on the pilot corpus. "Who was ejected from the game?" returned
// "Buddy Kennedy was ejected from the game", with evidence: sufficient and ten
// citations. The word "eject" appears in ZERO of the 892 chunks. The top hit
// reads "Buddy Kennedy challenged (pitch result), call on the field was
// confirmed: Buddy Kennedy called out on strikes". Retrieval was not at fault:
// a challenge really is the nearest thing in the corpus to an ejection, so the
// evidence genuinely is relevant and the threshold correctly says so. The model
// made the leap from "challenged" to "ejected" and kept the name attached.
//
// Only reading the answer back against the context can catch that, which is
// what this does.
//
// COST AND DEFAULT. One extra generation call per answered ask, so it is
// off by default and opt-in through rag.verify_faithfulness.
//
// FAIL-OPEN, deliberately, and consistent with the guard it sits beside.
// classifyEvidence already fails open when no hit carries an absolute signal,
// on the reasoning that suppressing answers over a blind spot is worse than
// answering with a documented one. A verifier that cannot be reached is the
// same shape: dropping every answer during a provider blip would turn a trust
// feature into an availability outage. The failure is logged.

// faithfulnessVerdict is the answer to "is every claim in this answer supported
// by the context the model was shown".
type faithfulnessVerdict int

const (
	// faithfulnessUnchecked: verification is off, or could not run. The answer
	// is published unchanged.
	faithfulnessUnchecked faithfulnessVerdict = iota
	// faithfulnessSupported: the verifier found every claim supported.
	faithfulnessSupported
	// faithfulnessUnsupported: at least one claim is not supported by the
	// context. The answer is withheld.
	faithfulnessUnsupported
)

// The verifier must answer with one of these and nothing else. A single token
// is deliberate: it is cheap to produce, unambiguous to parse, and it cannot be
// half-matched the way a sentence can.
const (
	faithfulSupportedToken   = "SUPPORTED"
	faithfulUnsupportedToken = "UNSUPPORTED"
)

// buildFaithfulnessPrompt asks whether answer is supported by contextSection,
// which is the VERBATIM context region of the prompt the answering model saw.
// Reusing those exact bytes matters: verifying against a re-rendered context
// would check a different claim than the one the answer was built from.
func buildFaithfulnessPrompt(question, answer, contextSection string) string {
	var b strings.Builder
	b.WriteString("You are checking whether an answer is supported by the source passages it was drawn from.\n")
	b.WriteString("Reply with exactly one word: ")
	b.WriteString(faithfulSupportedToken)
	b.WriteString(" if every factual claim in the answer is stated in or directly entailed by the passages, or ")
	b.WriteString(faithfulUnsupportedToken)
	b.WriteString(" if any claim is not.\n")
	b.WriteString("A claim that is merely PLAUSIBLE given the passages, or that renames something the passages describe differently, is ")
	b.WriteString(faithfulUnsupportedToken)
	b.WriteString(".\n\n")
	b.WriteString(promptfence.Guard("check"))
	b.WriteString("\n\nQuestion:\n")
	b.WriteString(question)
	b.WriteString("\n\nPassages:\n")
	b.WriteString(contextSection)
	b.WriteString("\n\nAnswer to check:\n")
	b.WriteString(promptfence.Wrap("", answer))
	// Restated last, per #892: the nearest instruction wins, and the passages
	// sit between the instruction and the reply.
	b.WriteString("\n\nReply with exactly one word, ")
	b.WriteString(faithfulSupportedToken)
	b.WriteString(" or ")
	b.WriteString(faithfulUnsupportedToken)
	b.WriteString(".")
	return b.String()
}

// parseFaithfulnessVerdict reads the verifier's reply.
//
// UNSUPPORTED is checked FIRST because it is a superstring of SUPPORTED: a
// reply of "UNSUPPORTED" contains "SUPPORTED", so testing the other way round
// would read every refusal as an approval. That is the one parsing mistake here
// that fails in the dangerous direction.
//
// A reply that says neither is unchecked rather than unsupported: the verifier
// did not answer the question asked, and inventing a refusal from a
// non-response would suppress good answers on a chatty model.
func parseFaithfulnessVerdict(reply string) faithfulnessVerdict {
	upper := strings.ToUpper(strings.TrimSpace(reply))
	switch {
	case strings.Contains(upper, faithfulUnsupportedToken):
		return faithfulnessUnsupported
	case strings.Contains(upper, faithfulSupportedToken):
		return faithfulnessSupported
	default:
		return faithfulnessUnchecked
	}
}

// verifyFaithfulness runs the grounding check and reports its verdict. It
// returns faithfulnessUnchecked when the feature is off, when there is nothing
// to check, or when the verifier could not be reached.
func (s *Service) verifyFaithfulness(ctx context.Context, question, answer, contextSection string) faithfulnessVerdict {
	s.metaMu.RLock()
	enabled := s.verifyFaithfulnessEnabled
	gen := s.gen
	s.metaMu.RUnlock()
	if !enabled || gen == nil {
		return faithfulnessUnchecked
	}
	if strings.TrimSpace(answer) == "" || strings.TrimSpace(contextSection) == "" {
		// Nothing was answered, or nothing was shown. Either way there is no
		// claim-to-context pair to check, and the callers above already refuse
		// to publish an answer grounded in an empty context (#403 F1).
		return faithfulnessUnchecked
	}

	var reply string
	err := usage.TimeStage(ctx, usage.StageGenerate, func() error {
		var gErr error
		reply, gErr = gen.Generate(ctx, buildFaithfulnessPrompt(question, answer, contextSection))
		return gErr
	})
	if err != nil {
		s.logf("faithfulness: verifier unavailable (%v); publishing the answer unchecked", err)
		return faithfulnessUnchecked
	}
	return parseFaithfulnessVerdict(reply)
}

// unfaithfulAnswer is the refusal published in place of an answer whose claims
// the verifier could not support.
//
// It reuses the abstention SHAPE (empty citations, the retrieved candidates
// still visible in hits) but says plainly why, because the two refusals have
// different causes: §9.4.3's abstention means "I found material and judged it
// too weak", and this one means "I found material, answered, and could not
// support the answer against it". §9.4.3 lets a server distinguish its
// refusals in the answer text, which is what this does, so no tool-contract
// field is added and no spec change is needed.
func unfaithfulAnswer() string {
	return "I could not verify the answer against the retrieved passages, so I am not reporting it. " +
		"The passages below were retrieved and may be relevant, but the answer drawn from them " +
		"made at least one claim they do not support."
}
