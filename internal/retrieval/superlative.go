package retrieval

import (
	"regexp"
	"strings"
)

// Superlative-only HyDE routing (issue #897).
//
// Nine retrieval configurations were measured end to end on the pilot corpus
// and none raised the overall score. Exactly one, HyDE, doubled the weakest
// question category (superlatives, 2/5 to 4/5) while degrading three others,
// so one global flag cannot serve every question shape.
//
// The full per-route table (PR #898) was measured against the same graded
// question set and HELD: the lexical classifier agreed with the human
// categories on 19/37 overall and on 0/8 negative controls, routing six of the
// eight toward MORE query transformation, the direction the issue names as
// fabrication-prone. Absence is a property of the corpus, not of the question
// text, so no lexical pattern can identify a negative control in principle.
//
// Superlatives are the one shape the same audit showed a lexical classifier
// identifies perfectly (5/5), and the one shape HyDE measurably helps. This
// file ships exactly that intersection and nothing else: a question that reads
// as a superlative MAY have HyDE enabled for it; every other question follows
// the global retrieval.hyde.enabled unchanged, and nothing here ever turns
// HyDE off. A misclassification therefore costs one HyDE transform on a
// non-superlative question, never a lost answer.

// superlativePatterns matches the surface form of a superlative question.
// "first" and "last" are deliberately absent: they read as superlatives of
// order but overlap time-scoped phrasing ("in the first inning"), and the #898
// audit measured exactly that collision pulling time-scoped questions toward
// more HyDE.
var superlativePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:most|least|best|worst|highest|lowest|longest|shortest|fastest|slowest|biggest|smallest|largest|greatest|strongest|weakest|hardest|easiest|top|maximum|minimum|deepest|widest|heaviest|lightest|oldest|newest|youngest|nearest|farthest|furthest)\b`),
	regexp.MustCompile(`\bmore\s+than\s+any\b`),
}

// isSuperlativeQuestion reports whether the question's surface form is a
// superlative. Deterministic and a pure function of the text, so a fixture can
// pin the route without a live model.
func isSuperlativeQuestion(question string) bool {
	q := strings.ToLower(strings.TrimSpace(question))
	if q == "" {
		return false
	}
	for _, re := range superlativePatterns {
		if re.MatchString(q) {
			return true
		}
	}
	return false
}
