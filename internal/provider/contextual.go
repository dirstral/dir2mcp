package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// EmbedContextualOff is the value of the terminal `contextual` component of the
// embed identity (SPEC 8.1.4/8.1.8) when contextual retrieval is NOT in effect —
// either because it is disabled, or because it is enabled but no chat provider
// resolved (capability fail-open, §8.1.8). It is a NAMED token rather than a
// bare boolean so future modes can be added without another identity-field
// migration, and it is the value every pre-feature recorded identity migrates to
// (normalizeEmbedIdentity), so no existing corpus spuriously reindexes.
const EmbedContextualOff = "off"

// embedContextualPrefix prefixes the generator-identity hash so the component is
// self-describing in a recorded identity and can never be confused with the
// "off" token.
const embedContextualPrefix = "ctx:"

// ContextualSpec is the canonical set of context-GENERATOR inputs the embed
// identity's `contextual` component hashes (SPEC 8.1.4/8.1.8). Every input that
// can change the generated context string belongs here; anything else (e.g. the
// per-chunk embedding_mode, or retrieval.contextual.bm25, which changes only the
// lexical index) deliberately does NOT.
type ContextualSpec struct {
	// Profile is the resolved chat profile that generates the context. Its Name
	// and its NORMALIZED base_url (the same normalization the embed identity
	// applies, §8.1.4) enter the hash: a different endpoint serving the same
	// model name is a different generator.
	Profile Profile
	// Model is the effective generation model (the profile's chat model, or an
	// operator override).
	Model string
	// MaxTokens is the generation bound; a different cap can yield a different
	// context, so it is part of the identity.
	MaxTokens int
	// PromptVersion names the built-in prompt template.
	PromptVersion string
	// Prompt is the EFFECTIVE prompt text (built-in template, or the operator
	// override verbatim). Folding the text in means an edited override re-embeds
	// even without a prompt_version bump — a version tag alone cannot detect it.
	Prompt string
}

// ContextualIdentity renders spec as the opaque `ctx:<hash>` token used as the
// terminal `contextual` component of the embed identity (SPEC 8.1.4/8.1.8) and
// as the generator half of the per-chunk context cache key (§8.6.7).
//
// The serialization is deterministic — a fixed field order joined by an explicit
// NUL separator that cannot occur in any of the inputs — and the digest is the
// corpus's standard content hash (sha256, §7.6). Hashing (rather than nesting
// the fields inline) keeps the component a SINGLE opaque token, so it can never
// collide with the outer "|" identity delimiter and no escaping is needed.
func ContextualIdentity(spec ContextualSpec) string {
	fields := []string{
		strings.TrimSpace(spec.Profile.Name),
		normalizeEmbedBaseURL(spec.Profile),
		strings.TrimSpace(spec.Model),
		strconv.Itoa(spec.MaxTokens),
		strings.TrimSpace(spec.PromptVersion),
		spec.Prompt,
	}
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x00")))
	return embedContextualPrefix + hex.EncodeToString(sum[:])
}

// NormalizeEmbedContextual maps the empty value to EmbedContextualOff so "" and
// "off" are equivalent everywhere (identity construction and comparison), and
// trims a recorded/computed token. A non-empty value is otherwise returned
// verbatim: it is an opaque generator-identity token whose case and content are
// significant.
func NormalizeEmbedContextual(contextual string) string {
	c := strings.TrimSpace(contextual)
	if c == "" {
		return EmbedContextualOff
	}
	return c
}

// ContextualActive reports whether a contextual component names an actual
// generator (i.e. contextualization was effectively ON for the corpus) rather
// than the disabled/fail-open "off" token.
func ContextualActive(contextual string) bool {
	return strings.HasPrefix(NormalizeEmbedContextual(contextual), embedContextualPrefix)
}
