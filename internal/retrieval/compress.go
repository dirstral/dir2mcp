package retrieval

import (
	"sort"
	"strings"
	"unicode"
)

// Evidence-guided context compression (issue #335).
//
// This is a deterministic, dependency-free compression pass applied ONLY to the
// per-hit text that is assembled into the RAG prompt sent to the generator. It
// never touches the SearchHit values themselves, the returned citations, or the
// hit snippets surfaced to callers — so citation fidelity is fully preserved.
//
// The pass is inspired by evidentiality-guided compression (ECoRAG) and prompt
// compression (LLMLingua), reduced to two cheap, deterministic heuristics:
//
//  1. query-relevance sentence filtering: rank a chunk's sentences by lexical
//     overlap with the query and keep the most relevant ones up to a budget;
//  2. redundancy dedup: drop a candidate sentence whose token set is nearly a
//     subset of an already-kept sentence.
//
// Kept sentences are re-emitted in their ORIGINAL order so the surviving text
// stays readable and faithful to the source ordering.

const (
	// defaultCompressionTargetRatio is the keep-fraction used when the operator
	// enables compression without specifying a ratio (config 0 ⇒ default).
	defaultCompressionTargetRatio = 0.5
	// redundancyOverlapThreshold is the token-set containment at or above which
	// a candidate sentence is considered redundant with an already-kept one
	// (i.e. most of its words already appear in a kept sentence).
	redundancyOverlapThreshold = 0.8
	// minSentenceRunes guards against compressing already-tiny text: snippets at
	// or below this rune length are returned unchanged (compression cannot
	// meaningfully help and risks dropping the only evidence).
	minSentenceRunes = 80
)

// contextCompressor holds the resolved compression policy. The zero value with
// enabled=false is a pass-through. It is safe for concurrent use (immutable
// after construction).
type contextCompressor struct {
	enabled     bool
	targetRatio float64
}

// newContextCompressor resolves an enable flag and a raw target ratio into a
// usable policy. A ratio outside (0,1] falls back to the built-in default; this
// mirrors the config-layer validation but keeps the retrieval layer robust to
// programmatic construction.
func newContextCompressor(enabled bool, targetRatio float64) contextCompressor {
	if targetRatio <= 0 || targetRatio > 1 {
		targetRatio = defaultCompressionTargetRatio
	}
	return contextCompressor{enabled: enabled, targetRatio: targetRatio}
}

// compressSnippet returns an evidence-filtered version of text relevant to
// query. When compression is disabled, the input is too short to benefit, or no
// query terms are available, the original text is returned unchanged. The result
// is never longer than the input and never empty when the input is non-empty.
func (c contextCompressor) compressSnippet(query, text string) string {
	if !c.enabled {
		return text
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	if len([]rune(trimmed)) <= minSentenceRunes {
		return text
	}

	sentences := splitSentences(trimmed)
	if len(sentences) <= 1 {
		return text
	}

	queryTerms := tokenSet(query)
	if len(queryTerms) == 0 {
		// Without query signal we cannot rank by relevance; leave the text as is
		// rather than guessing (graceful no-op).
		return text
	}

	type scored struct {
		idx    int
		text   string
		tokens map[string]struct{}
		score  float64
	}
	ranked := make([]scored, 0, len(sentences))
	for i, s := range sentences {
		toks := tokenSet(s)
		ranked = append(ranked, scored{
			idx:    i,
			text:   s,
			tokens: toks,
			score:  overlapScore(queryTerms, toks),
		})
	}

	// Rank by descending relevance; ties keep original order for determinism.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].idx < ranked[j].idx
		}
		return ranked[i].score > ranked[j].score
	})

	budget := int(float64(len([]rune(trimmed)))*c.targetRatio + 0.999)
	if budget < 1 {
		budget = 1
	}

	keptIdx := make([]int, 0, len(ranked))
	keptTokens := make([]map[string]struct{}, 0, len(ranked))
	used := 0
	for _, cand := range ranked {
		// Always keep the single most relevant sentence (first in ranked order)
		// so compression never drops all evidence, even if it overruns budget.
		mostRelevant := len(keptIdx) == 0
		if !mostRelevant {
			// Beyond the guaranteed most-relevant sentence, only keep sentences
			// that carry at least some query signal. Pure filler (score 0) is
			// dropped even if budget remains — that is the whole point of
			// evidence-guided compression. ranked is score-descending, so once
			// we hit a zero-score candidate every remaining one is also zero.
			if cand.score <= 0 {
				break
			}
			if used >= budget {
				break
			}
			if isRedundant(cand.tokens, keptTokens) {
				continue
			}
		}
		keptIdx = append(keptIdx, cand.idx)
		keptTokens = append(keptTokens, cand.tokens)
		used += len([]rune(cand.text)) + 1 // +1 for the joining space
	}

	// Re-emit kept sentences in original order for readability/fidelity.
	sort.Ints(keptIdx)
	parts := make([]string, 0, len(keptIdx))
	for _, idx := range keptIdx {
		parts = append(parts, sentences[idx])
	}
	out := strings.Join(parts, " ")
	if strings.TrimSpace(out) == "" {
		// Defensive: never return empty for non-empty input.
		return text
	}
	return out
}

// isRedundant reports whether cand's token set is mostly contained in any
// already-kept sentence's token set (containment >= redundancyOverlapThreshold).
func isRedundant(cand map[string]struct{}, kept []map[string]struct{}) bool {
	if len(cand) == 0 {
		return false
	}
	for _, k := range kept {
		if containment(cand, k) >= redundancyOverlapThreshold {
			return true
		}
	}
	return false
}

// containment returns the fraction of a's tokens that also appear in b.
func containment(a, b map[string]struct{}) float64 {
	if len(a) == 0 {
		return 0
	}
	hits := 0
	for t := range a {
		if _, ok := b[t]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(a))
}

// overlapScore returns the fraction of a sentence's tokens that are query terms.
// Normalizing by sentence length avoids favoring long sentences purely for
// containing more tokens.
func overlapScore(query, sentence map[string]struct{}) float64 {
	if len(sentence) == 0 {
		return 0
	}
	hits := 0
	for t := range sentence {
		if _, ok := query[t]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(sentence))
}

// tokenSet lowercases s and returns the set of word tokens (letters/digits),
// dropping single-character tokens which carry little signal.
func tokenSet(s string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	set := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if len([]rune(f)) <= 1 {
			continue
		}
		set[f] = struct{}{}
	}
	return set
}

// splitSentences breaks text into sentences on terminal punctuation (. ! ?) and
// newlines. It keeps the punctuation with the sentence and trims surrounding
// whitespace. Empty fragments are dropped. This is intentionally simple and
// deterministic rather than linguistically perfect.
func splitSentences(text string) []string {
	var sentences []string
	var b strings.Builder
	flush := func() {
		s := strings.TrimSpace(b.String())
		if s != "" {
			sentences = append(sentences, s)
		}
		b.Reset()
	}
	for _, r := range text {
		b.WriteRune(r)
		switch r {
		case '.', '!', '?', '\n':
			flush()
		}
	}
	flush()
	return sentences
}
