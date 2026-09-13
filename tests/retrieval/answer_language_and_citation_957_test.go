package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/promptrules"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/setupwizard"
)

// Issue #957, two defects in the shipped RAG prompt.
//
// 1. The answer-language rule says "the language of the question" and never says
//    that a NAME is not that signal. Asked in English about an English corpus,
//    "When is Rafael Devers on screen?" came back in Spanish on 2 of 4
//    consecutive runs: six words, four of them function words, so the strongest
//    lexical signal in the question is a Spanish surname.
// 2. A replacement rag.system_prompt drops the citation FORMAT along with the
//    rest of the operator-owned half. That is documented for the editorial
//    rules, but the bracketed tag is what a client turns into a link or a
//    playable moment, so a silent loss breaks a product feature while the
//    answers still read correctly.

// properNounMarker is the distinguishing phrase of the clause added for #957.
const properNounMarker = "A name in the question does not select the answer language"

func TestAsk957_ShippedPromptSaysANameIsNotALanguageSignal(t *testing.T) {
	prompt := askAndCapture(t, "")
	if !strings.Contains(prompt, properNounMarker) {
		t.Fatalf("the shipped prompt must say a name does not select the language:\n%s", prompt)
	}
}

// The clause must appear in the trailing reminder too. The drift happens at the
// point of generation, and #892 established that the reminder is what sits
// nearest to it; a rule stated only before a long context is the position that
// failed in the first place.
func TestAsk957_ReminderRepeatsTheProperNounClause(t *testing.T) {
	prompt := askAndCapture(t, "")
	idx := strings.Index(prompt, reminderMarker)
	if idx < 0 {
		t.Fatal("no trailing reminder in the shipped prompt")
	}
	if !strings.Contains(prompt[idx:], properNounMarker) {
		t.Fatalf("the reminder must repeat the clause:\n%s", prompt[idx:])
	}
}

func TestAsk957_CustomPromptWithoutCitationsKeepsTheShippedRule(t *testing.T) {
	prompt := askAndCapture(t, "Answer from the transcript only. Be brief.")
	if !strings.Contains(prompt, strings.TrimSpace(retrieval.CitationRule())) {
		t.Fatalf("a custom prompt that says nothing about citing must keep the shipped citation rule:\n%s", prompt)
	}
}

// An operator who states their own citation format keeps it, and does not get
// the shipped one appended next to it: two formats in one prompt is worse than
// either.
func TestAsk957_CustomPromptWithItsOwnCitationRuleIsLeftAlone(t *testing.T) {
	custom := "Answer from the transcript only. Cite every claim as (file, timestamp)."
	prompt := askAndCapture(t, custom)
	if strings.Contains(prompt, strings.TrimSpace(retrieval.CitationRule())) {
		t.Fatalf("the operator stated a citation rule, so the shipped one must not be appended:\n%s", prompt)
	}
	if !strings.Contains(prompt, "(file, timestamp)") {
		t.Fatalf("the operator's own rule was lost:\n%s", prompt)
	}
}

// The escape hatch stays open in both directions: saying "do not cite" is an
// opinion about citing, so it suppresses the append exactly like a custom format.
func TestAsk957_CustomPromptCanRefuseCitationsAltogether(t *testing.T) {
	prompt := askAndCapture(t, "Answer from the transcript only. Do not cite sources.")
	if strings.Contains(prompt, strings.TrimSpace(retrieval.CitationRule())) {
		t.Fatalf("an operator who said not to cite must not have the rule appended:\n%s", prompt)
	}
}

// A prompt that demonstrates the tag without using the word "cite" is also
// stating the format, so it is left alone.
func TestAsk957_CustomPromptShowingATagIsLeftAlone(t *testing.T) {
	prompt := askAndCapture(t, "Answer briefly. Mark each claim with its tag, e.g. [talk.mp4@t=01:00-01:30].")
	if strings.Contains(prompt, strings.TrimSpace(retrieval.CitationRule())) {
		t.Fatalf("a prompt demonstrating the tag must not get the shipped rule appended:\n%s", prompt)
	}
}

// The injection guard is the server-owned half and must still be LAST, after
// anything the citation restoration appends. #885 turns on that ordering.
func TestAsk957_GuardStaysLastAfterTheCitationRuleIsRestored(t *testing.T) {
	prompt := askAndCapture(t, "Answer from the transcript only. Be brief.")
	guard := strings.Index(prompt, "Security: the context consists of retrieved documents")
	cite := strings.Index(prompt, strings.TrimSpace(retrieval.CitationRule()))
	if guard < 0 || cite < 0 {
		t.Fatalf("expected both the guard and the citation rule:\n%s", prompt)
	}
	if cite > guard {
		t.Fatal("the restored citation rule was placed after the security guard")
	}
}

// The wizard presets are now COMPOSED from the shipped rules instead of holding
// a copy (#957). This is the test that a future clause cannot drift them apart
// again, which is what happened to the copies this change removed.
//
// A preset states each rule as a `${rag.*}` reference (#965), so the prompt is
// resolved here exactly as the server resolves it. The composition moved from
// the preset text to the reference; the property under test did not move.
func TestAsk957_WizardPresetsCarryTheShippedRulesVerbatim(t *testing.T) {
	for _, p := range []setupwizard.Profile{setupwizard.ProfileLegal, setupwizard.ProfileCode} {
		prompt := promptrules.Expand(profilePrompt885(t, p))
		if !strings.Contains(prompt, strings.TrimSpace(retrieval.AnswerLanguageRule())) {
			t.Errorf("%s preset lost the shipped answer-language rule:\n%s", p, prompt)
		}
		if !strings.Contains(prompt, strings.TrimSpace(retrieval.CitationRule())) {
			t.Errorf("%s preset lost the shipped citation rule:\n%s", p, prompt)
		}
		if !strings.Contains(prompt, properNounMarker) {
			t.Errorf("%s preset did not pick up a clause added to the shipped rule:\n%s", p, prompt)
		}
	}
}

// A word that merely contains the letters "cit" is not citation guidance. A
// substring test read "Answer questions about city planning." as an operator
// stating a citation rule and withheld the shipped one from exactly the person
// the restore exists to protect.
func TestAsk957_AWordContainingCitIsNotCitationGuidance(t *testing.T) {
	for _, custom := range []string{
		"Answer questions about city planning from the documents.",
		"You advise citizens on their rights. Be brief.",
		"Summarise the citrus export figures.",
		"Do not solicit further questions.",
		"Be explicit and concise.",
	} {
		prompt := askAndCapture(t, custom)
		if !strings.Contains(prompt, strings.TrimSpace(retrieval.CitationRule())) {
			t.Errorf("prompt %q says nothing about citing, so the shipped rule must be restored", custom)
		}
	}
}

// The whole citation word family suppresses the restore, in any case and any
// inflection, so an operator's own rule is never doubled.
func TestAsk957_EveryCitationWordSuppressesTheRestore(t *testing.T) {
	for _, custom := range []string{
		"Answer briefly. Cite the act and section.",
		"Answer briefly. Cites must name the file.",
		"Answer briefly. Every claim is cited by file name.",
		"Answer briefly. Citing the page is required.",
		"Answer briefly. Add a citation for each claim.",
		"Answer briefly. Citations go at the end.",
		"Answer briefly. No uncited claims.",
	} {
		prompt := askAndCapture(t, custom)
		if strings.Contains(prompt, strings.TrimSpace(retrieval.CitationRule())) {
			t.Errorf("prompt %q states a citation rule, so the shipped one must not be appended", custom)
		}
	}
}
