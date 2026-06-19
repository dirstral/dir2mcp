package retrieval

import (
	"regexp"
	"strings"
)

// adaptiveDecision is the verdict of the training-free retrieval gate for a
// single query. It is intentionally small and value-typed so the gate stays a
// pure function of its inputs (query text + bounds): identical input always
// yields identical output, which makes the behavior deterministic and testable
// without any model or network call.
type adaptiveDecision struct {
	// Retrieve reports whether retrieval should run at all. When false the
	// caller should answer without retrieved context (the "skip" verdict for
	// trivial, non-informational queries such as greetings).
	Retrieve bool
	// K is the effective number of hits to request when Retrieve is true. It is
	// always within the configured [kMin, kMax] window. When Retrieve is false
	// K is 0 and carries no meaning.
	K int
	// Class is a short, stable label for the chosen policy ("skip", "narrow",
	// "default", "widen"). It exists for logging/observability only and is not
	// part of any external contract.
	Class string
}

var (
	// adaptiveWordRe extracts word-like tokens (letters/digits across Unicode)
	// so the gate's length/keyword signals ignore punctuation and whitespace.
	adaptiveWordRe = regexp.MustCompile(`[\p{L}\p{N}]+`)

	// adaptiveTrivialTokens are stand-alone conversational tokens that, when a
	// query consists solely of them (plus punctuation), carry no information
	// need and so warrant skipping retrieval entirely. Kept deliberately small
	// and high-precision to avoid skipping real questions.
	adaptiveTrivialTokens = map[string]bool{
		"hi": true, "hello": true, "hey": true, "yo": true,
		"thanks": true, "thank": true, "thx": true, "ty": true,
		"ok": true, "okay": true, "k": true, "cool": true, "nice": true,
		"yes": true, "no": true, "yep": true, "nope": true, "sure": true,
		"bye": true, "goodbye": true, "cya": true,
		"please": true, "sorry": true,
	}

	// adaptiveInterrogatives signal a genuine information need; their presence
	// blocks the "skip" verdict even for short queries.
	adaptiveInterrogatives = map[string]bool{
		"who": true, "what": true, "when": true, "where": true, "why": true,
		"how": true, "which": true, "whom": true, "whose": true,
		"is": true, "are": true, "do": true, "does": true, "did": true,
		"can": true, "could": true, "should": true, "would": true, "will": true,
		"explain": true, "describe": true, "list": true, "summarize": true,
		"define": true, "find": true, "show": true, "give": true, "tell": true,
	}

	// adaptiveHardConnectors signal a multi-part or comparative query that
	// typically benefits from a wider evidence pool.
	adaptiveHardConnectors = map[string]bool{
		"and": true, "or": true, "vs": true, "versus": true,
		"compare": true, "comparison": true, "difference": true,
		"differences": true, "between": true, "relationship": true,
		"tradeoff": true, "tradeoffs": true, "pros": true, "cons": true,
		"because": true, "therefore": true,
	}
)

// adaptiveGate is the core, training-free retrieval decision. Given the raw
// query and the resolved bounds (kMin <= kMax, both > 0) and the base k the
// caller would otherwise use, it returns whether to retrieve and, if so, a
// dynamic k clamped into [kMin, kMax].
//
// The policy uses only cheap, local query signals (length, question-likeness,
// code-likeness, multi-clause connectors) so it adds no latency and is fully
// deterministic:
//
//   - skip:    a short query made up solely of conversational filler tokens
//     (greetings/acknowledgements) with no interrogative — no information need,
//     so retrieval is skipped.
//   - narrow:  a short, single-clause question — bias k toward kMin to cut
//     noise and cost on easy lookups.
//   - widen:   a long or multi-clause/comparative query — bias k toward kMax to
//     pull more evidence for harder asks.
//   - default: everything else — keep the base k (clamped into the window).
//
// base <= 0 is treated as "unspecified" and replaced by the midpoint-ish base
// before clamping; callers normally pass rag.k_default.
func adaptiveGate(query string, base, kMin, kMax int) adaptiveDecision {
	// Defensive normalization so the gate is correct even if bounds arrive
	// unsanitized; the engine also clamps these at wiring time.
	if kMin < 1 {
		kMin = 1
	}
	if kMax < kMin {
		kMax = kMin
	}
	if base <= 0 {
		base = kMin + (kMax-kMin)/2
	}

	tokens := adaptiveWordRe.FindAllString(strings.ToLower(query), -1)
	n := len(tokens)

	// Empty/whitespace-only query: nothing to act on. Skip retrieval rather
	// than burn an embedding+search on no signal.
	if n == 0 {
		return adaptiveDecision{Retrieve: false, K: 0, Class: "skip"}
	}

	hasInterrogative := false
	hardConnectors := 0
	for _, t := range tokens {
		if adaptiveInterrogatives[t] {
			hasInterrogative = true
		}
		if adaptiveHardConnectors[t] {
			hardConnectors++
		}
	}
	hasQuestionMark := strings.Contains(query, "?")
	codey := LooksLikeCodeQuery(query)

	// SKIP: only for short queries with no information need. A query qualifies
	// when every token is conversational filler and there is no interrogative
	// word, no question mark, and no code signal. Capped at 3 tokens so a real
	// short question is never skipped.
	if n <= 3 && !hasInterrogative && !hasQuestionMark && !codey {
		allTrivial := true
		for _, t := range tokens {
			if !adaptiveTrivialTokens[t] {
				allTrivial = false
				break
			}
		}
		if allTrivial {
			return adaptiveDecision{Retrieve: false, K: 0, Class: "skip"}
		}
	}

	// HARD: long queries or multi-clause/comparative ones widen k toward kMax.
	// Two independent signals (length OR connectors/length combos) keep this
	// robust without a trained model.
	hard := n >= 16 || hardConnectors >= 2 || (hardConnectors >= 1 && n >= 10)
	if hard {
		return adaptiveDecision{Retrieve: true, K: kMax, Class: "widen"}
	}

	// EASY: a short, single-clause question narrows k toward kMin. Requires a
	// question signal so we don't aggressively narrow ambiguous statements.
	easy := n <= 6 && hardConnectors == 0 && (hasInterrogative || hasQuestionMark)
	if easy {
		return adaptiveDecision{Retrieve: true, K: kMin, Class: "narrow"}
	}

	// DEFAULT: clamp the caller's base k into the configured window.
	k := base
	if k < kMin {
		k = kMin
	}
	if k > kMax {
		k = kMax
	}
	return adaptiveDecision{Retrieve: true, K: k, Class: "default"}
}
