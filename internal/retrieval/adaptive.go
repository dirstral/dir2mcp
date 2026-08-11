package retrieval

import (
	"context"
	"regexp"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/usage"
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

// applyAdaptiveGate resolves the opt-in adaptive retrieval verdict for one ask
// (config retrieval.adaptive.*). It reports whether retrieval must be skipped
// and it writes the effective k into query when retrieval runs. With the gate
// disabled it is a no-op, so the fixed-k path is unchanged.
//
// An ask carries two texts. `query.Query` is the retrieval query. `question` is
// the text the answer must satisfy. Ask copies the question into an empty query,
// so the two agree unless a caller overrides the query. The gate keeps its k
// decision on the retrieval query, because k bounds that search. A SKIP verdict
// needs more care: it removes the evidence from an answer, so the answered
// question must carry no information need either. A caller that pairs a
// substantive question with a trivial query override would otherwise get an
// ungrounded answer to a real question (#685). The gate therefore re-classifies
// the question before it skips, and it retrieves when the question asks for
// something.
func (s *Service) applyAdaptiveGate(question string, query *model.SearchQuery) bool {
	s.metaMu.RLock()
	enabled := s.adaptiveEnabled
	kMin := s.adaptiveKMin
	kMax := s.adaptiveKMax
	s.metaMu.RUnlock()
	if !enabled {
		return false
	}
	decision := adaptiveGate(query.Query, query.K, kMin, kMax)
	s.logf("adaptive gate: class=%s retrieve=%t k=%d", decision.Class, decision.Retrieve, decision.K)
	if decision.Retrieve {
		query.K = decision.K
		return false
	}
	asked := adaptiveGate(question, query.K, kMin, kMax)
	if !asked.Retrieve {
		return true
	}
	s.logf("adaptive gate: skip on the query, class=%s on the question; retrieving with k=%d", asked.Class, asked.K)
	query.K = asked.K
	return false
}

// The no-retrieval answer path (issue #685).
//
// A "skip" verdict means the gate found no information need in the message, so
// it spends neither an embedding nor an index search on it. Skip is NOT the
// insufficient-evidence abstention of SPEC §9.4.3: that one runs AFTER
// retrieval, judges an eligible hit set that exists, and reports that the
// corpus material is too weak. Here retrieval never ran, so the server knows
// nothing about the corpus and must not report anything about it. Returning the
// zero-hit fallback ("No relevant context found in the indexed corpus.") states
// a corpus result the server never obtained, which is why the skip verdict gets
// its own answer path.
//
// The path stays inside the grounding rules of SPEC §9.4.1: the model receives
// no document, so the answer carries an EMPTY citations array, and Ask strips
// every file tag and any Sources footer the model invents.
const (
	// noRetrievalFallbackAnswer is the deterministic reply used when no
	// generator is configured, when generation fails, or when the model
	// returns nothing. It reports the server state truthfully: no lookup ran.
	noRetrievalFallbackAnswer = "No corpus lookup was needed for this message. " +
		"Ask a question about the indexed documents to search them."

	// noRetrievalSystemPrompt instructs a short, source-free reply. The model
	// gets no document here, so every corpus claim it could make would be
	// ungrounded by construction.
	noRetrievalSystemPrompt = "You are the assistant of a document search server.\n" +
		"The message below carries no information need, so no document was retrieved.\n" +
		"Reply in one or two short sentences.\n" +
		"State no fact about the indexed documents.\n" +
		"Write no file name, no source and no citation.\n" +
		"You can offer to search the indexed documents."

	// noRetrievalMaxQuestionRunes bounds the message copied into the prompt.
	// The gate only skips very short messages, but the raw string can still
	// hold long punctuation runs, so the bound is explicit.
	noRetrievalMaxQuestionRunes = 512

	// noRetrievalMaxTokens bounds the completion. A conversational reply needs
	// very few tokens, and the bound keeps a skip verdict cheap.
	noRetrievalMaxTokens = 128
)

// noRetrievalPrompt builds the bounded, non-RAG prompt for a skip verdict. It
// contains no document fence because no document is sent.
func noRetrievalPrompt(question string) string {
	return noRetrievalSystemPrompt +
		"\n\nMessage: " + truncateSnippet(question, noRetrievalMaxQuestionRunes) +
		"\nReply:"
}

// answerWithoutRetrieval produces the answer text for an adaptive skip verdict.
// It calls the configured generator with noRetrievalPrompt and falls back to
// noRetrievalFallbackAnswer when no generator exists, when the call fails, or
// when the reply is blank. It never returns citations or hits: the caller keeps
// both empty.
func (s *Service) answerWithoutRetrieval(ctx context.Context, question string) string {
	if s.gen == nil {
		return noRetrievalFallbackAnswer
	}
	var generated string
	genErr := usage.TimeStage(ctx, usage.StageGenerate, func() error {
		var gErr error
		generated, gErr = boundedGenerate(ctx, s.gen, noRetrievalPrompt(question), noRetrievalMaxTokens)
		return gErr
	})
	if genErr != nil {
		// Log the failure without the full message, which can hold sensitive
		// text, exactly as the grounded path does.
		s.logf("no-retrieval generator error for question %q: %v", truncateQuestion(question), genErr)
		return noRetrievalFallbackAnswer
	}
	trimmed := strings.TrimSpace(generated)
	if trimmed == "" {
		return noRetrievalFallbackAnswer
	}
	// The model saw no document, so the allowed citation set is empty and every
	// file tag it wrote is ungrounded (§9.4.1). Ask removes any Sources footer
	// separately through ensureAnswerAttributions.
	trimmed = strings.TrimSpace(stripHallucinatedCitations(trimmed, nil))
	if trimmed == "" {
		return noRetrievalFallbackAnswer
	}
	return trimmed
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
