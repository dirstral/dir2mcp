package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dirstral/dir2mcp/internal/promptrules"
)

// validateRAGSystemPrompt checks the `${rag.*}` rule references a custom
// rag.system_prompt may carry, and reports a prompt that copies a shipped rule
// instead of referencing it (issue #965).
//
// Two things happen here, and they are deliberately of different strengths.
//
// An unknown or malformed reference is CONFIG_INVALID. The namespace is closed
// and new, so nothing existing can break, and the alternative is the exact
// failure this issue is about: `${rag.answer_language}`, or an unclosed
// `${rag.answer_language_rule`, would travel to the model as literal text, the
// answer-language rule would be absent, the trailing reminder (#892) would
// stand down, and no layer would say a word.
//
// A partial copy of a shipped rule is a WARNING. The server cannot repair it:
// the operator's prompt is the operator's, and text that looks like our rule
// may be text they now own. What the server can do is say that the copy has
// gone stale, name the behaviour that stopped, and name the reference that
// cannot go stale again.
//
// The gate lives in Validate() for the reason validateIngestFormatModes gives:
// Validate() is the one gate every entry point runs, so a prompt that cannot be
// honored is refused wherever it is written, and it runs after the CLI
// overrides are merged.
func (c *Config) validateRAGSystemPrompt() error {
	prompt := c.RAGSystemPrompt
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	if unknown := promptrules.UnknownReferences(prompt); len(unknown) > 0 {
		return fmt.Errorf("CONFIG_INVALID: rag.system_prompt contains %s, which this server will not expand: "+
			"the name matches no shipped rule, or the reference is not closed. "+
			"The references it expands are %s (SPEC §16.1.2)",
			quoteAll(unknown), strings.Join(knownRuleTokens(), " and "))
	}
	for _, stale := range promptrules.StaleCopies(prompt) {
		c.appendWarningOnce(fmt.Errorf(
			"rag.system_prompt reproduces PART of the shipped %s but not all of it. "+
				"That is a copy taken from an older release, and the server matches this rule exactly, so %s is off for "+
				"this deployment. Nothing else changes. Write %s in place of the copied sentences and the prompt follows "+
				"the server from now on. The current text is: %q",
			stale.Name, stale.Behaviour, stale.Token, strings.TrimSpace(stale.Text)))
	}
	return nil
}

// knownRuleTokens lists the references Expand understands, for an error that
// has to tell the operator what they may write.
func knownRuleTokens() []string {
	shipped := promptrules.Rules()
	tokens := make([]string, 0, len(shipped))
	for _, r := range shipped {
		tokens = append(tokens, r.Token)
	}
	return tokens
}

// quoteAll renders the offending occurrences for an error message. They are
// quoted because a malformed one is a fragment of the operator's prompt, and an
// unquoted fragment would be unreadable inside a sentence.
func quoteAll(refs []string) string {
	quoted := make([]string, 0, len(refs))
	for _, ref := range refs {
		quoted = append(quoted, strconv.Quote(ref))
	}
	return strings.Join(quoted, ", ")
}

// appendWarningOnce adds a warning unless the same text is already recorded.
//
// Validate() runs more than once on one Config: `up` re-validates after merging
// its flags onto the loaded config, and the save paths validate again before
// writing. A load-time warning is about the configured VALUE, not about the
// number of times it was checked, so the operator reads it once.
func (c *Config) appendWarningOnce(warn error) {
	if warn == nil {
		return
	}
	for _, existing := range c.Warnings {
		if existing != nil && existing.Error() == warn.Error() {
			return
		}
	}
	c.Warnings = append(c.Warnings, warn)
}
