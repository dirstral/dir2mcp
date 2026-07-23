package config

import (
	"fmt"
	"strings"

	"github.com/dirstral/dir2mcp/internal/provider"
)

// Contextual-retrieval constants (SPEC §8.1.8/§16.2, issue #330).
const (
	// DefaultContextualMaxTokens bounds one generated context. The context is
	// meant to be one or two sentences, so the cap is deliberately tight — it
	// keeps per-chunk generation cheap and prevents a runaway completion from
	// dominating the embed input.
	DefaultContextualMaxTokens = 128

	// ContextualPromptVersionV1 names the shipped, domain-general prompt
	// template. The version tag and the EFFECTIVE prompt text are both folded
	// into the embed identity (SPEC §8.1.4), so bumping the template here is a
	// deliberate re-embed.
	ContextualPromptVersionV1 = "v1"

	// ContextualDocumentPlaceholder / ContextualChunkPlaceholder are the
	// placeholders a prompt template (built-in or operator override) MUST carry.
	ContextualDocumentPlaceholder = "{{DOCUMENT}}"
	ContextualChunkPlaceholder    = "{{CHUNK}}"
)

// contextualPromptV1 is the built-in, DOMAIN-GENERAL context prompt. It makes no
// assumption about corpus subject, language, or document type: the model is asked
// only to situate the chunk inside its own document and to answer with nothing
// but that context. Keeping it domain-free is a hard project constraint — a
// corpus-specific prompt belongs in an operator override, never in the default.
const contextualPromptV1 = `<document>
` + ContextualDocumentPlaceholder + `
</document>

Here is the chunk we want to situate within the whole document:
<chunk>
` + ContextualChunkPlaceholder + `
</chunk>

Give a short, succinct context (one or two sentences) that situates this chunk
within the overall document, so the chunk can be found by a search engine.
Write the context in the same language as the chunk. Answer with the context
only — no preamble, no quotes, no explanation.`

// contextualPromptTemplates maps a prompt_version tag to its built-in template.
// Adding a version here is additive; changing an existing one re-embeds every
// corpus that uses it, so versions are append-only in practice.
var contextualPromptTemplates = map[string]string{
	ContextualPromptVersionV1: contextualPromptV1,
}

// ContextualPromptVersions returns the known built-in prompt-version tags, for
// error messages and docs.
func ContextualPromptVersions() []string {
	return []string{ContextualPromptVersionV1}
}

// ContextualPromptTemplate returns the built-in template for version, or
// ("", false) when the version is unknown.
func ContextualPromptTemplate(version string) (string, bool) {
	tmpl, ok := contextualPromptTemplates[strings.TrimSpace(version)]
	return tmpl, ok
}

// ContextualEffectivePrompt returns the prompt template actually used to
// generate per-chunk context: the operator override verbatim when
// `retrieval.contextual.prompt` is set, else the built-in template named by
// `retrieval.contextual.prompt_version`. The result is what gets HASHED into the
// embed identity (SPEC §8.1.4), so an edited override re-embeds even without a
// prompt_version bump. An unknown version with no override yields ("", false).
func (c Config) ContextualEffectivePrompt() (string, bool) {
	if override := strings.TrimSpace(c.RetrievalContextualPrompt); override != "" {
		return override, true
	}
	return ContextualPromptTemplate(c.contextualPromptVersion())
}

// contextualPromptVersion is the effective prompt_version: the configured value,
// or the v1 default when unset (an empty value in a hand-written config must not
// silently disable the feature).
func (c Config) contextualPromptVersion() string {
	if v := strings.TrimSpace(c.RetrievalContextualPromptVersion); v != "" {
		return v
	}
	return ContextualPromptVersionV1
}

// contextualMaxTokens is the effective generation bound, defaulting when unset.
func (c Config) contextualMaxTokens() int {
	if c.RetrievalContextualMaxTokens > 0 {
		return c.RetrievalContextualMaxTokens
	}
	return DefaultContextualMaxTokens
}

// RenderContextualPrompt substitutes the document and chunk into prompt. Both
// placeholders are replaced wherever they appear; a template lacking one is
// rejected by validation, never silently rendered without its input.
func RenderContextualPrompt(prompt, document, chunk string) string {
	rendered := strings.ReplaceAll(prompt, ContextualDocumentPlaceholder, document)
	return strings.ReplaceAll(rendered, ContextualChunkPlaceholder, chunk)
}

// ContextualBinding is the fully resolved contextual-retrieval activation for a
// config: whether contextualization is EFFECTIVELY on, the generator profile and
// model it runs on, the bound, the effective prompt, and the resulting embed
// identity component (SPEC §8.1.4/§8.1.8).
//
// It is the single source of truth shared by the config snapshot (which records
// the component), the embed identity (which compares it), and the ingest service
// (which generates the context) — so the recorded identity can never drift from
// what actually ran.
type ContextualBinding struct {
	// Active reports whether contextualization is EFFECTIVELY on: enabled in
	// config AND a chat provider resolved. False for both "disabled" and the
	// capability fail-open, which the spec requires to be indistinguishable in
	// the recorded identity (§8.1.4).
	Active bool
	// Profile is the resolved chat profile (zero value when inactive).
	Profile provider.Profile
	// Model is the effective generation model (empty when inactive).
	Model string
	// MaxTokens is the effective per-context generation bound.
	MaxTokens int
	// Prompt is the effective prompt template (empty when inactive).
	Prompt string
	// PromptVersion is the effective built-in template tag.
	PromptVersion string
	// Identity is the embed-identity `contextual` component: provider.
	// EmbedContextualOff when inactive, else the "ctx:<hash>" generator token.
	Identity string
	// FellOpen reports that the operator ENABLED contextual retrieval but no chat
	// provider resolved, so the corpus embeds raw under an `…|off` identity. It
	// drives the one-time warning (SPEC §8.1.8 fail-open); it is deliberately NOT
	// part of the identity.
	FellOpen bool
}

// ContextualBinding resolves the effective contextual-retrieval activation for
// cfg (SPEC §8.1.8). It is capability-driven and fail-open, exactly like OCR/STT:
// with `enabled: false`, or with `enabled: true` but no chat-capable provider
// resolvable, the binding is inactive and its Identity is the literal `off` — the
// corpus embeds raw and its recorded identity says so. Recording an "on" token
// for raw vectors would make the corpus look contextual-compatible the moment a
// chat provider is added, silently mixing raw and contextualized vectors.
func (cfg Config) ContextualBinding() ContextualBinding {
	binding := ContextualBinding{
		MaxTokens:     cfg.contextualMaxTokens(),
		PromptVersion: cfg.contextualPromptVersion(),
		Identity:      provider.EmbedContextualOff,
	}
	if !cfg.RetrievalContextualEnabled {
		return binding
	}
	prompt, ok := cfg.ContextualEffectivePrompt()
	if !ok {
		// Validation rejects an unknown prompt_version, so this is unreachable for
		// a validated config; fail open rather than embed under a prompt we cannot
		// name.
		binding.FellOpen = true
		return binding
	}
	prof, err := cfg.resolveContextualProfile()
	if err != nil {
		binding.FellOpen = true
		return binding
	}
	model := strings.TrimSpace(cfg.RetrievalContextualModel)
	if model == "" {
		model = strings.TrimSpace(prof.ChatModel)
	}
	binding.Active = true
	binding.Profile = prof
	binding.Model = model
	binding.Prompt = prompt
	binding.Identity = provider.ContextualIdentity(provider.ContextualSpec{
		Profile:       prof,
		Model:         model,
		MaxTokens:     binding.MaxTokens,
		PromptVersion: binding.PromptVersion,
		Prompt:        prompt,
	})
	return binding
}

// resolveContextualProfile resolves the chat profile that generates the context:
// the explicitly pinned `retrieval.contextual.provider` when set, else the
// configured chat capability binding (SPEC 8.1.3).
func (cfg Config) resolveContextualProfile() (provider.Profile, error) {
	// baseProviders (not Providers) — Providers stamps the contextual component,
	// which is what we are computing here; going through it would recurse.
	resolution := cfg.baseProviders()
	if pinned := strings.TrimSpace(cfg.RetrievalContextualProvider); pinned != "" {
		// required=false: an unusable pin falls open to `off` (embed raw + warn)
		// like every other contextual-retrieval failure, rather than hard-failing
		// ingest (SPEC §8.1.8).
		return resolution.ResolveExplicit(provider.CapChat, pinned, false)
	}
	return resolution.Resolve(provider.CapChat)
}

// validateRetrievalContextual enforces the static `retrieval.contextual` rules
// (SPEC §16.2). They apply only when the feature is enabled, so a default config
// is never affected. A missing chat provider is deliberately NOT an error here —
// that is the fail-open capability path (§8.1.8), reported as a warning at
// startup instead.
func (c *Config) validateRetrievalContextual() error {
	if !c.RetrievalContextualEnabled {
		return nil
	}
	if c.RetrievalContextualMaxTokens <= 0 {
		return fmt.Errorf(
			"retrieval.contextual.max_tokens must be > 0 when retrieval.contextual.enabled is true: %d",
			c.RetrievalContextualMaxTokens)
	}
	if override := strings.TrimSpace(c.RetrievalContextualPrompt); override != "" {
		for _, placeholder := range []string{ContextualDocumentPlaceholder, ContextualChunkPlaceholder} {
			if !strings.Contains(override, placeholder) {
				return fmt.Errorf(
					"retrieval.contextual.prompt override must contain the %s placeholder", placeholder)
			}
		}
		return nil
	}
	if _, ok := ContextualPromptTemplate(c.contextualPromptVersion()); !ok {
		return fmt.Errorf(
			"retrieval.contextual.prompt_version %q is unknown; use one of %s (or set retrieval.contextual.prompt)",
			c.RetrievalContextualPromptVersion, strings.Join(ContextualPromptVersions(), ", "))
	}
	return nil
}
