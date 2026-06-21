package retrieval

import "strings"

// HyDE (Hypothetical Document Embeddings) query-transform modes. These mirror
// the config values config.HyDEModeFuse / config.HyDEModeReplace; the retrieval
// package keeps its own copies so it does not import the config package.
//
//   - hydeModeFuse:    RRF-fuse the hypothetical-document hits with the
//     raw-query hits (the default; improves recall while keeping the raw
//     query's precision).
//   - hydeModeReplace: retrieve with the hypothetical-document embedding alone.
const (
	hydeModeFuse    = "fuse"
	hydeModeReplace = "replace"
)

// hydeMaxAnswerChars bounds the hypothetical answer used for retrieval. A short
// passage is enough to close the query↔document style gap (HyDE, Gao et al.);
// trimming keeps the follow-up embedding call cheap and bounded.
const hydeMaxAnswerChars = 1000

// buildHyDEPrompt builds the instruction that asks the generator for a concise
// hypothetical answer to the query. The generated text is embedded and used as
// the retrieval vector (the HyDE transform); it is never shown to the end user,
// so the prompt optimizes for a focused, document-style passage rather than a
// conversational reply. The query is embedded verbatim — callers pass an already
// trimmed, non-empty query.
func buildHyDEPrompt(query string) string {
	var b strings.Builder
	b.WriteString("Write a short, factual passage (2-4 sentences) that directly answers ")
	b.WriteString("the question below, as it might appear in a reference document. ")
	b.WriteString("Do not add preamble, caveats, or citations; output only the passage.\n\n")
	b.WriteString("Question: ")
	b.WriteString(query)
	b.WriteString("\n\nPassage:")
	return b.String()
}

// truncateHyDEAnswer bounds a generated hypothetical answer to hydeMaxAnswerChars
// runes so an over-long generation does not bloat the embedding request. Returns
// the trimmed answer unchanged when already within the bound.
func truncateHyDEAnswer(answer string) string {
	answer = strings.TrimSpace(answer)
	r := []rune(answer)
	if len(r) <= hydeMaxAnswerChars {
		return answer
	}
	return strings.TrimSpace(string(r[:hydeMaxAnswerChars]))
}
