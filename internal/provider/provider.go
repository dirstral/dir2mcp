// Package provider implements the spec 0.7.0 provider model: the
// normative capability matrix (SPEC 8.1.2), per-capability provider
// selection (8.1.3), and the embeddings corpus-lifetime identity
// (8.1.4). It is pure resolution logic with no HTTP or config-file
// dependency: the config layer builds Profiles and a deterministic
// precedence order, calls Select per capability, and constructs the
// concrete model.* adapter from the chosen Profile.Kind.
package provider

import (
	"errors"
	"fmt"
	"strings"
)

// Kind is a provider adapter / wire protocol (SPEC 8.1.1).
type Kind string

const (
	KindOpenAI     Kind = "openai" // OpenAI-compatible backbone (incl. Mistral chat/embed, local)
	KindMistral    Kind = "mistral"
	KindAnthropic  Kind = "anthropic"
	KindGemini     Kind = "gemini"
	KindCohere     Kind = "cohere"
	KindElevenLabs Kind = "elevenlabs"
)

// Capability is a model capability bound to a provider profile.
type Capability string

const (
	CapEmbed  Capability = "embed"
	CapChat   Capability = "chat"
	CapOCR    Capability = "ocr"
	CapSTT    Capability = "stt"
	CapTTS    Capability = "tts"
	CapRerank Capability = "rerank"
)

// Support is a capability-matrix cell state.
type Support int

const (
	// Unsupported: binding this capability to this kind MUST be rejected
	// as CONFIG_INVALID (static validation, SPEC 8.1.2).
	Unsupported Support = iota
	// Supported: statically valid.
	Supported
	// EndpointDependent: implemented by the adapter but not statically
	// verifiable (kind: openai audio). Validated at first use, never
	// CONFIG_INVALID.
	EndpointDependent
)

// matrix is the normative SPEC 8.1.2 capability table. Absent cells are
// Unsupported.
var matrix = map[Kind]map[Capability]Support{
	KindOpenAI: {
		CapEmbed: Supported, CapChat: Supported,
		CapSTT: EndpointDependent, CapTTS: EndpointDependent,
	},
	KindMistral: {
		CapOCR: Supported, CapSTT: Supported,
	},
	KindAnthropic: {
		CapChat: Supported,
	},
	KindGemini: {
		CapEmbed: Supported, CapChat: Supported,
		CapSTT: Supported, CapTTS: Supported,
	},
	KindCohere: {
		CapEmbed: Supported, CapChat: Supported, CapRerank: Supported,
	},
	KindElevenLabs: {
		CapSTT: Supported, CapTTS: Supported,
	},
}

// Can reports the matrix support for (kind, cap). Unknown kinds/caps are
// Unsupported.
func Can(k Kind, c Capability) Support {
	if caps, ok := matrix[k]; ok {
		return caps[c]
	}
	return Unsupported
}

// Profile is a resolved provider profile. APIKey is the already-resolved
// secret (never persisted; see SPEC 16.1.1). CredentialLess marks a
// profile that declares no api_key requirement at all (e.g. a local
// Ollama/vLLM endpoint) — such profiles are eligible even with an empty
// key, unlike a profile that expects a key whose env var is unset.
type Profile struct {
	Name           string
	Kind           Kind
	BaseURL        string
	APIKey         string
	CredentialLess bool

	EmbedTextModel string
	EmbedCodeModel string
	ChatModel      string
	OCRModel       string
	STTModel       string
	TTSModel       string
	RerankModel    string
}

// Eligible reports whether the profile may be selected/preflighted
// (SPEC 8.1.3): a credential is present, or it is credential-less.
func (p Profile) Eligible() bool {
	return strings.TrimSpace(p.APIKey) != "" || p.CredentialLess
}

// ErrNoProvider means no eligible+capable profile exists for a
// capability in auto mode (SPEC 8.1.3 case 3). Callers decide: a
// required capability fails preflight; an optional one stays off.
var ErrNoProvider = errors.New("no eligible provider for capability")

// ConfigError is a static-validation failure that the CLI surfaces as
// CONFIG_INVALID (SPEC 8.1.2 / 8.1.3 case 1).
type ConfigError struct {
	Capability Capability
	Profile    string
	Reason     string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("CONFIG_INVALID: %s.provider %q: %s", e.Capability, e.Profile, e.Reason)
}

// Select resolves the profile for cap (SPEC 8.1.3).
//
//   - explicit != "": that profile must exist, the matrix must not mark
//     the pair Unsupported, and (if required) it must be Eligible —
//     otherwise a *ConfigError.
//   - explicit == "" (auto): the first profile in precedence order that
//     is Eligible and not Unsupported for cap. If none and required is
//     false → ErrNoProvider; if required → ErrNoProvider too (the CLI
//     turns that into a preflight failure).
func Select(precedence []Profile, byName map[string]Profile, cap Capability, explicit string, required bool) (Profile, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		p, ok := byName[explicit]
		if !ok {
			return Profile{}, &ConfigError{Capability: cap, Profile: explicit, Reason: "no such provider profile"}
		}
		if Can(p.Kind, cap) == Unsupported {
			// SPEC 8.1.2: incapable binding is always CONFIG_INVALID,
			// regardless of required-ness (static validation).
			return Profile{}, &ConfigError{Capability: cap, Profile: explicit,
				Reason: fmt.Sprintf("provider kind %q cannot serve %s", p.Kind, cap)}
		}
		if !p.Eligible() {
			// SPEC 8.1.3 case 1: CONFIG_INVALID only when the capability
			// is required; an optional capability with an explicit but
			// credential-less profile stays off (the rerank rule).
			if required {
				return Profile{}, &ConfigError{Capability: cap, Profile: explicit,
					Reason: "required capability but the profile has no credential (set its API key)"}
			}
			return Profile{}, ErrNoProvider
		}
		return p, nil
	}
	for _, p := range precedence {
		if p.Eligible() && Can(p.Kind, cap) != Unsupported {
			return p, nil
		}
	}
	return Profile{}, ErrNoProvider
}

// EmbedIdentity is the corpus-lifetime embed identity (SPEC 8.1.4):
// provider name + text/code model. It is recorded in the config
// snapshot/index and compared on load. Role (8.1.5) is deliberately
// excluded — it does not affect vector-space compatibility.
func EmbedIdentity(p Profile) string {
	return fmt.Sprintf("%s|%s|%s",
		strings.TrimSpace(p.Name),
		strings.TrimSpace(p.EmbedTextModel),
		strings.TrimSpace(p.EmbedCodeModel))
}

// VerifyEmbedIdentity returns a *ConfigError when a recorded embed
// identity differs from the current one (SPEC 8.1.4 — the server MUST
// NOT silently mix vector spaces). An empty recorded identity (fresh
// index) always passes.
func VerifyEmbedIdentity(recorded, current string) error {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" || recorded == strings.TrimSpace(current) {
		return nil
	}
	return &ConfigError{
		Capability: CapEmbed,
		Profile:    current,
		Reason: fmt.Sprintf(
			"embed identity changed (index built with %q, configured %q); embeddings are corpus-lifetime — reindex to change the embed provider/model",
			recorded, current),
	}
}
