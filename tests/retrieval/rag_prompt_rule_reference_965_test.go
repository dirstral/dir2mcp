package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/promptrules"
)

// Issue #965, the retrieval half: a `${rag.*}` reference in rag.system_prompt
// must reach the model as the rule the RUNNING server ships.
//
// #892 gates the trailing answer-language reminder on the prompt in force
// carrying the shipped rule, matched exactly, so the server can never
// contradict an operator who fixed one answer language. The gate is right and
// stays as it is. What changes is that an operator no longer has to reproduce
// our wording to pass it: a reference is expanded where the prompt is
// installed, so it carries whatever this binary ships, and a later rewording
// reaches the operator without an edit.

// TestAsk965_AReferencedRuleReachesTheModel is the substitution itself.
func TestAsk965_AReferencedRuleReachesTheModel(t *testing.T) {
	prompt := askAndCapture(t, "You answer questions about baseball broadcasts.\n"+
		promptrules.AnswerLanguageRuleToken+promptrules.CitationRuleToken)

	if !strings.Contains(prompt, strings.TrimSpace(promptrules.AnswerLanguageRule)) {
		t.Fatalf("the referenced answer-language rule did not reach the model:\n%s", prompt)
	}
	if !strings.Contains(prompt, strings.TrimSpace(promptrules.CitationRule)) {
		t.Fatalf("the referenced citation rule did not reach the model:\n%s", prompt)
	}
	if !strings.Contains(prompt, "You answer questions about baseball broadcasts.") {
		t.Fatalf("the operator's own wording was lost:\n%s", prompt)
	}
}

// TestAsk965_NoReferenceSurvivesIntoThePrompt: an unexpanded `${rag.…}` in the
// text a model reads is the failure this feature exists to prevent, dressed as
// the fix. The config loader refuses an unknown reference, so nothing in this
// namespace should ever be sent.
func TestAsk965_NoReferenceSurvivesIntoThePrompt(t *testing.T) {
	prompt := askAndCapture(t, "Answer briefly.\n"+
		promptrules.AnswerLanguageRuleToken+promptrules.CitationRuleToken)
	if strings.Contains(prompt, "${rag.") {
		t.Fatalf("a rule reference was sent to the model verbatim:\n%s", prompt)
	}
}

// TestAsk965_AReferenceArmsTheTrailingReminder is the behaviour the issue is
// about. The reminder is gated on the prompt in force carrying the rule; a
// reference carries it, so the reminder fires, and it keeps firing after the
// rule is reworded because the reference is resolved per run.
func TestAsk965_AReferenceArmsTheTrailingReminder(t *testing.T) {
	prompt := askAndCapture(t, "You answer questions about baseball broadcasts.\n"+
		promptrules.AnswerLanguageRuleToken+promptrules.CitationRuleToken)
	if !strings.Contains(prompt, reminderMarker) {
		t.Fatalf("a referenced answer-language rule did not arm the #892 reminder:\n%s", prompt)
	}
}

// TestAsk965_AReferencePicksUpAClauseAddedToTheRule is the anti-drift pin. A
// COPY of the rule is what #963 broke: it froze at the wording of the day it
// was pasted. The clause added by #957/#963 is the newest one, so a reference
// that carries it is carrying today's text and not a snapshot.
func TestAsk965_AReferencePicksUpAClauseAddedToTheRule(t *testing.T) {
	prompt := askAndCapture(t, "Answer briefly.\n"+promptrules.AnswerLanguageRuleToken)
	if !strings.Contains(prompt, strings.TrimSpace(promptrules.ProperNounClause)) {
		t.Fatalf("the referenced rule is missing a clause the shipped rule states:\n%s", prompt)
	}
}

// TestAsk965_AReferencedCitationRuleIsNotDoubled: composeSystemPrompt restores
// the shipped citation rule to a prompt that says nothing about citing (#957).
// A prompt that REFERENCES the rule has already stated it, so the restore must
// stand down. Two copies of one format is worse than either.
func TestAsk965_AReferencedCitationRuleIsNotDoubled(t *testing.T) {
	prompt := askAndCapture(t, "Answer briefly.\n"+promptrules.CitationRuleToken)
	rule := strings.TrimSpace(promptrules.CitationRule)
	if n := strings.Count(prompt, rule); n != 1 {
		t.Fatalf("want the citation rule exactly once, got %d:\n%s", n, prompt)
	}
}

// TestAsk965_TheShippedRulesAreOneDefinition pins the move that makes all of
// the above possible. retrieval exported the two rules for the setup wizard
// (#957); they now live in internal/promptrules so the config loader can read
// them too, and these are the values retrieval still composes its prompt from.
// A second definition anywhere is the drift this package removes.
func TestAsk965_TheShippedRulesAreOneDefinition(t *testing.T) {
	prompt := askAndCapture(t, "")
	if !strings.Contains(prompt, strings.TrimSpace(promptrules.AnswerLanguageRule)) {
		t.Errorf("the shipped prompt does not state promptrules.AnswerLanguageRule:\n%s", prompt)
	}
	if !strings.Contains(prompt, strings.TrimSpace(promptrules.CitationRule)) {
		t.Errorf("the shipped prompt does not state promptrules.CitationRule:\n%s", prompt)
	}
}
