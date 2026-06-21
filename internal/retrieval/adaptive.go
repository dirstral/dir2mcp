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
	kMin, kMax = normalizeAdaptiveBounds(kMin, kMax)
	if base <= 0 {
		base = kMin + (kMax-kMin)/2
	}

	sig := computeQuerySignals(query)

	// SKIP: empty queries, or short conversational filler with no information
	// need. Avoids an embedding+search when there is nothing to retrieve.
	if sig.tokens == 0 || isTrivialSkip(sig) {
		return adaptiveDecision{Retrieve: false, K: 0, Class: "skip"}
	}
	// HARD: long or multi-clause/comparative queries widen k toward kMax.
	if isHardQuery(sig) {
		return adaptiveDecision{Retrieve: true, K: kMax, Class: "widen"}
	}
	// EASY: short single-clause questions narrow k toward kMin.
	if isEasyQuery(sig) {
		return adaptiveDecision{Retrieve: true, K: kMin, Class: "narrow"}
	}
	// DEFAULT: clamp the caller's base k into the configured window.
	return adaptiveDecision{Retrieve: true, K: clampInt(base, kMin, kMax), Class: "default"}
}

// querySignals holds the cheap, local features the gate derives from a query.
type querySignals struct {
	tokens           int  // word-like token count
	hasInterrogative bool // a question word is present
	hasQuestionMark  bool // a literal '?' is present
	codey            bool // looks code-oriented (LooksLikeCodeQuery)
	hardConnectors   int  // count of multi-clause/comparative connectors
	allTrivial       bool // every token is conversational filler
}

// computeQuerySignals extracts the gate's decision features from query in a
// single pass over its tokens.
func computeQuerySignals(query string) querySignals {
	toks := adaptiveWordRe.FindAllString(strings.ToLower(query), -1)
	sig := querySignals{
		tokens:          len(toks),
		hasQuestionMark: strings.Contains(query, "?"),
		codey:           LooksLikeCodeQuery(query),
		allTrivial:      len(toks) > 0,
	}
	for _, t := range toks {
		if adaptiveInterrogatives[t] {
			sig.hasInterrogative = true
		}
		if adaptiveHardConnectors[t] {
			sig.hardConnectors++
		}
		if !adaptiveTrivialTokens[t] {
			sig.allTrivial = false
		}
	}
	return sig
}

// isTrivialSkip reports whether a query is short conversational filler with no
// information need (≤3 filler-only tokens, no question/code signal).
func isTrivialSkip(sig querySignals) bool {
	return sig.tokens <= 3 && sig.allTrivial &&
		!sig.hasInterrogative && !sig.hasQuestionMark && !sig.codey
}

// isHardQuery reports whether a query is long or multi-clause/comparative and so
// benefits from a wider evidence pool.
func isHardQuery(sig querySignals) bool {
	return sig.tokens >= 16 || sig.hardConnectors >= 2 ||
		(sig.hardConnectors >= 1 && sig.tokens >= 10)
}

// isEasyQuery reports whether a query is a short, single-clause question that
// can be answered from a narrow evidence pool.
func isEasyQuery(sig querySignals) bool {
	return sig.tokens <= 6 && sig.hardConnectors == 0 &&
		(sig.hasInterrogative || sig.hasQuestionMark)
}

// normalizeAdaptiveBounds repairs a possibly-unsanitized [kMin, kMax] window to
// 1 <= kMin <= kMax so the gate never emits an out-of-range k.
func normalizeAdaptiveBounds(kMin, kMax int) (int, int) {
	if kMin < 1 {
		kMin = 1
	}
	if kMax < kMin {
		kMax = kMin
	}
	return kMin, kMax
}

// clampInt clamps v into [lo, hi] (assumes lo <= hi).
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
