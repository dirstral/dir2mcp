package tests

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/promptrules"
)

// Issue #965, the unit level. tests/config and tests/retrieval pin what an
// operator and a model see; these pin the three contracts those two rest on:
// Expand substitutes, UnknownReferences refuses everything it will not
// substitute, and StaleCopies reports a copy that has gone out of date.

const (
	langToken = promptrules.AnswerLanguageRuleToken
	citeToken = promptrules.CitationRuleToken
)

func TestPromptRules965_Expand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
		want   string
	}{
		{"no reference is returned unchanged", "Answer briefly.", "Answer briefly."},
		{"empty prompt", "", ""},
		{"one reference", "A " + langToken, "A " + promptrules.AnswerLanguageRule},
		{"both references", citeToken + langToken,
			promptrules.CitationRule + promptrules.AnswerLanguageRule},
		{"a repeated reference expands every time", langToken + "|" + langToken,
			promptrules.AnswerLanguageRule + "|" + promptrules.AnswerLanguageRule},
		// Expand substitutes what it knows and nothing else. The config loader
		// refuses the rest, so a near miss never reaches a model; if Expand
		// guessed instead, a typo would silently resolve to a rule the operator
		// did not name.
		{"an unknown name is left alone", "${rag.answer_language}", "${rag.answer_language}"},
		{"a broken reference is left alone", "${rag.citation_rule", "${rag.citation_rule"},
		{"text outside the namespace is untouched", "${HOME} and ${ragged}", "${HOME} and ${ragged}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := promptrules.Expand(tc.prompt); got != tc.want {
				t.Errorf("Expand(%q)\n got: %q\nwant: %q", tc.prompt, got, tc.want)
			}
		})
	}
}

func TestPromptRules965_UnknownReferences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prompt string
		want   []string
	}{
		{"no reference", "Answer briefly.", nil},
		{"both shipped references", citeToken + langToken, nil},
		{"outside the namespace", "${HOME} ${ragged_edge} $rag.citation_rule", nil},
		{"an unknown name", "x ${rag.answer_language} y", []string{"${rag.answer_language}"}},
		{"an empty name", "${rag.}", []string{"${rag.}"}},
		{"unclosed", "x ${rag.citation_rule", []string{"${rag.citation_rule"}},
		{"broken by whitespace", "${rag. citation_rule}", []string{"${rag."}},
		{"one entry per distinct occurrence",
			"${rag.a} ${rag.a} ${rag.b}", []string{"${rag.a}", "${rag.b}"}},
		{"a valid reference beside a broken one",
			langToken + " ${rag.nope}", []string{"${rag.nope}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := promptrules.UnknownReferences(tc.prompt)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("UnknownReferences(%q)\n got: %q\nwant: %q", tc.prompt, got, tc.want)
			}
		})
	}
}

// TestPromptRules965_ABrokenReferenceExcerptStaysShortAndWholeInUTF8: the
// excerpt is quoted back in an error, so it is bounded in RUNES. A byte cut
// would split a multi-byte character and put invalid UTF-8 into a log line.
func TestPromptRules965_ABrokenReferenceExcerptStaysShortAndWholeInUTF8(t *testing.T) {
	unknown := promptrules.UnknownReferences("${rag." + strings.Repeat("é", 200))
	if len(unknown) != 1 {
		t.Fatalf("want one broken reference, got %q", unknown)
	}
	excerpt := unknown[0]
	if !utf8.ValidString(excerpt) {
		t.Errorf("the excerpt is not valid UTF-8: %q", excerpt)
	}
	if n := utf8.RuneCountInString(excerpt); n > 64 {
		t.Errorf("the excerpt is %d runes; an error must not quote a page of prompt", n)
	}
}

// TestPromptRules965_ABrokenReferenceExcerptCopiesNoPromptText is the privacy
// half. A reference holds no whitespace, so the excerpt stops at the first one
// and the words around it never reach a log.
func TestPromptRules965_ABrokenReferenceExcerptCopiesNoPromptText(t *testing.T) {
	unknown := promptrules.UnknownReferences("Answer in ${rag. PATIENT NAME AND DIAGNOSIS")
	if len(unknown) != 1 {
		t.Fatalf("want one broken reference, got %q", unknown)
	}
	if strings.ContainsAny(unknown[0], " \t\r\n") {
		t.Errorf("the excerpt carries prompt text past the reference: %q", unknown[0])
	}
}

func TestPromptRules965_StaleCopies(t *testing.T) {
	stale := strings.Replace(promptrules.AnswerLanguageRule, promptrules.ProperNounClause, "", 1)

	for _, tc := range []struct {
		name   string
		prompt string
		want   []string
	}{
		{"nothing copied", "Answer briefly and cite the file.", nil},
		{"the current rule", promptrules.AnswerLanguageRule, nil},
		{"a reference", langToken + citeToken, nil},
		{"a pre-#963 copy", stale, []string{promptrules.AnswerLanguageRuleName}},
		{"a rewrapped pre-#963 copy", strings.ReplaceAll(stale, " ", "\n"),
			[]string{promptrules.AnswerLanguageRuleName}},
		{"one sentence of the rule is enough evidence of a copy",
			firstSentence(promptrules.AnswerLanguageRule),
			[]string{promptrules.AnswerLanguageRuleName}},
		{"a truncated citation rule",
			strings.TrimSuffix(strings.TrimSpace(promptrules.CitationRule), " or [notes.md]."),
			[]string{promptrules.CitationRuleName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, r := range promptrules.StaleCopies(tc.prompt) {
				got = append(got, r.Name)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("StaleCopies(%q)\n got: %q\nwant: %q", tc.prompt, got, tc.want)
			}
		})
	}
}

func firstSentence(rule string) string {
	if i := strings.Index(rule, ". "); i >= 0 {
		return rule[:i+1]
	}
	return rule
}

// TestPromptRules965_TheRegistryCannotBeEditedByACaller: Expand builds its
// replacer once, at package initialization, while validation reads the registry
// per call. A caller who could append to a shared slice would make the two
// disagree, and the shape of that disagreement is "the config validated and the
// rule did not expand", which is the defect this package removes.
func TestPromptRules965_TheRegistryCannotBeEditedByACaller(t *testing.T) {
	before := promptrules.Rules()
	if len(before) == 0 {
		t.Fatal("no shipped rules")
	}
	mutated := promptrules.Rules()
	mutated[0].Token = "${rag.hijacked}"
	if grown := append(mutated, promptrules.Rule{Name: "rag.injected", Token: "${rag.injected}"}); len(grown) != len(before)+1 {
		t.Fatalf("append to the returned slice did not grow it: %d", len(grown))
	}

	after := promptrules.Rules()
	if len(after) != len(before) || after[0].Token != before[0].Token {
		t.Fatalf("a caller changed the shipped registry: before=%v after=%v", before, after)
	}
	if got := promptrules.UnknownReferences("${rag.injected}"); len(got) != 1 {
		t.Errorf("an injected name must stay unknown, got %q", got)
	}
}

// A CONFIG_INVALID carrying an unknown reference is read by whoever gets the
// error, and for this server that includes an MCP client: internal/mcp returns
// a config fault as a tool error with the message attached. wellFormedReference
// is loose about the NAME by design, so `${rag.` + anything without a brace or a
// space + `}` is well formed and can hold the operator's prompt text at any
// length. These pin what an error may repeat back.
func TestUnknownReferenceQuotesATypoAndDescribesAnythingElse(t *testing.T) {
	secret := strings.Repeat("s3cr3t-", 50)

	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{
			// The case the message exists for. An operator with three
			// references needs to see WHICH one is wrong, so this is repeated
			// exactly.
			name:   "a near miss of a shipped token is quoted exactly",
			prompt: "Answer well. ${rag.answer_language} Cite your sources.",
			want:   "${rag.answer_language}",
		},
		{
			name:   "an unclosed reference is quoted up to the first space",
			prompt: "Answer well. ${rag.citation_rule and then some prose.",
			want:   "${rag.citation_rule",
		},
		{
			// Not a typo of anything. Described by length, never repeated.
			name:   "prompt text inside the braces is described, not repeated",
			prompt: "Answer well. ${rag." + secret + "} Cite your sources.",
			want:   "a ${rag. reference 357 characters long that is not a rule name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := promptrules.UnknownReferences(tc.prompt)
			if len(got) != 1 {
				t.Fatalf("want exactly one unknown reference, got %q", got)
			}
			if got[0] != tc.want {
				t.Errorf("reported %q, want %q", got[0], tc.want)
			}
		})
	}
}

func TestAnUnknownReferenceNeverCarriesPromptTextIntoTheError(t *testing.T) {
	// The shape of a pasted credential: long, no spaces, no braces. It is well
	// formed by wellFormedReference's rules, so nothing upstream stops it.
	secret := "sk-proj-" + strings.Repeat("Ab3", 40)
	prompt := "Follow these rules. ${rag." + secret + "} Always cite."

	got := promptrules.UnknownReferences(prompt)
	if len(got) != 1 {
		t.Fatalf("want exactly one unknown reference, got %q", got)
	}
	if strings.Contains(got[0], "sk-proj-") {
		t.Fatalf("the error would repeat the prompt text: %q", got[0])
	}
	if !strings.Contains(got[0], "not a rule name") {
		t.Errorf("the error must still say what is wrong, got %q", got[0])
	}
}

func TestALongReferenceIsStillRefused(t *testing.T) {
	// Redacting the text must not redact the VERDICT. The reference is still
	// unknown, so the config is still invalid.
	prompt := "${rag." + strings.Repeat("x", 200) + "}"
	if got := promptrules.UnknownReferences(prompt); len(got) != 1 {
		t.Fatalf("a long unknown reference must still be reported, got %q", got)
	}
}

func TestTwoDifferentLongReferencesReportOnce(t *testing.T) {
	// Both describe as "not a rule name" with the same length, so they collapse
	// to one line rather than repeating an identical sentence.
	a := "${rag." + strings.Repeat("a", 100) + "}"
	b := "${rag." + strings.Repeat("b", 100) + "}"
	if got := promptrules.UnknownReferences(a + " and " + b); len(got) != 1 {
		t.Errorf("want one deduplicated line, got %q", got)
	}
}
