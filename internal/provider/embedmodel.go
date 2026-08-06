package provider

import "strings"

// Adapter embed-model defaults (issue #705).
//
// These MUST equal the DefaultEmbedModel/DefaultModel constant each client
// package substitutes when a profile leaves the model blank. They are
// duplicated here as literals rather than imported so this package stays pure
// resolution logic with no HTTP/client dependency (see the package doc); the
// duplication is fenced by a drift guard in
// tests/providerfactory/effective_embed_models_705_test.go, which fails if a
// client constant ever changes without this table.
const (
	// DefaultOpenAIEmbedModel mirrors internal/openai.DefaultEmbedModel.
	DefaultOpenAIEmbedModel = "text-embedding-3-small"
	// DefaultCohereEmbedModel mirrors internal/cohere.DefaultEmbedModel.
	DefaultCohereEmbedModel = "embed-v4.0"
	// DefaultGeminiEmbedModel mirrors internal/gemini.DefaultEmbedModel.
	DefaultGeminiEmbedModel = "gemini-embedding-001"
	// DefaultOmniEmbedModel mirrors internal/omniembed.DefaultModel.
	DefaultOmniEmbedModel = "omniembed"
)

// KindDefaultEmbedModel returns the embed model an adapter of kind k sends on
// the wire when the profile leaves the model blank. Kinds with no embed
// capability (or no defined default) return "" — their effective model is
// whatever the profile carries.
func KindDefaultEmbedModel(k Kind) string {
	switch k {
	case KindOpenAI:
		return DefaultOpenAIEmbedModel
	case KindCohere:
		return DefaultCohereEmbedModel
	case KindGemini:
		return DefaultGeminiEmbedModel
	case KindOmniEmbed:
		return DefaultOmniEmbedModel
	default:
		return ""
	}
}

// EffectiveEmbedModels returns the concrete text/code embed model ids an
// adapter built from p actually sends on the wire, resolving each empty profile
// field to its adapter kind default (KindDefaultEmbedModel). Built-in profiles
// like `openai`/`cohere`/`local`/`omniembed` ship WITHOUT embed model fields
// (SPEC 8.1.1), so the raw profile does not name the model that produced the
// vectors — the adapter default does.
//
// This is the single resolution point shared by the embed identity (8.1.4,
// EmbedIdentity), the ingest embed worker, and the query-side retrieval
// embedder (providerfactory.EffectiveEmbedModels delegates here), so all four
// name BYTE-IDENTICAL models (issues #440 F2, #705).
func EffectiveEmbedModels(p Profile) (text, code string) {
	def := KindDefaultEmbedModel(p.Kind)
	text = strings.TrimSpace(p.EmbedTextModel)
	if text == "" {
		text = def
	}
	code = strings.TrimSpace(p.EmbedCodeModel)
	if code == "" {
		code = def
	}
	return text, code
}
