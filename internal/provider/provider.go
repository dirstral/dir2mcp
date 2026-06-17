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
	// KindWhisper is a self-hosted, OpenAI-compatible STT endpoint
	// (the GPU-VPS path, dir2mcp#240): POST {base_url}/v1/audio/transcriptions.
	// Distinct from KindOpenAI so STT auto-selection and the matrix can
	// treat it as statically STT-capable (Supported) and credential-optional.
	KindWhisper Kind = "whisper"
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
	// CapDiarize is speaker diarization on a transcript (SPEC §8.6.8). It is
	// optional and provider-dependent: only a diarization-capable STT backend
	// (a self-hosted WhisperX / pyannote-backed endpoint, KindWhisper) advertises
	// it. Binding diarization to a backend that lacks the capability is rejected
	// as CONFIG_INVALID, consistent with the capability matrix (8.1.2).
	CapDiarize Capability = "diarize"
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
	KindWhisper: {
		// Self-hosted OpenAI-compatible STT (dir2mcp#240). Statically
		// STT-capable: the standard /v1/audio/transcriptions contract is
		// fixed, so unlike kind:openai (EndpointDependent) we mark it
		// Supported. A self-hosted WhisperX / pyannote-backed endpoint is the
		// diarization-capable backend (SPEC §8.6.8), so it advertises
		// CapDiarize; no other kind does, which keeps diarization opt-in and
		// provider-dependent.
		CapSTT:     Supported,
		CapDiarize: Supported,
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
	// EmbedTextDim / EmbedCodeDim request a specific embedding output
	// dimensionality for Matryoshka/MRL models (SPEC 8.1.6), per axis.
	// Zero means "the model's native dimension". Part of the embed
	// identity (SPEC 8.1.4), so a change is reindex-bound.
	EmbedTextDim int
	EmbedCodeDim int
	// EmbedMultimodal is the multimodal embedding mode (SPEC 8.1.7):
	// "" / "off" (text-only), "augment", or "replace". augment/replace
	// require a multimodal model on every axis; part of the embed identity
	// (SPEC 8.1.4), so a change is reindex-bound.
	EmbedMultimodal string
	ChatModel       string
	OCRModel        string
	STTModel        string
	STTLanguage     string // optional STT language hint (e.g. ElevenLabs)
	// STTVAD requests a provider-side voice-activity-detection filter where
	// supported (dir2mcp#258, config `media.vad`). For the self-hosted whisper
	// provider this maps to the OpenAI-compatible `vad_filter` form field;
	// providers without VAD support ignore it. It is not part of the STT
	// identity, so toggling it is not reindex-bound.
	STTVAD      bool
	TTSModel    string
	TTSVoice    string // TTS voice id/name (e.g. ElevenLabs voice, OpenAI voice)
	RerankModel string
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
// provider name + text/code model + requested text/code output dimension
// (8.1.6) + multimodal mode (8.1.7). It is recorded in the config
// snapshot/index and compared on load. Role (8.1.5) is deliberately
// excluded — it does not affect vector-space compatibility, but the
// requested dimension and multimodal mode do.
func EmbedIdentity(p Profile) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d|%s",
		strings.TrimSpace(p.Name),
		strings.TrimSpace(p.EmbedTextModel),
		strings.TrimSpace(p.EmbedCodeModel),
		p.EmbedTextDim,
		p.EmbedCodeDim,
		NormalizeEmbedMultimodal(p.EmbedMultimodal))
}

// NormalizeEmbedMultimodal lower-cases/trims the multimodal mode and maps
// the empty value to "off", so "" and "off" are equivalent everywhere
// (identity comparison and validation).
func NormalizeEmbedMultimodal(mode string) string {
	m := strings.ToLower(strings.TrimSpace(mode))
	if m == "" {
		return "off"
	}
	return m
}

// VerifyEmbedIdentity returns a *ConfigError when a recorded embed
// identity differs from the current one (SPEC 8.1.4 — the server MUST
// NOT silently mix vector spaces). An empty recorded identity (fresh
// index) always passes. A legacy 3-field identity (pre-8.1.6, no
// dimension) is normalized to the native "|0|0" form before comparison so
// upgrading an existing native-dimension corpus does not force a spurious
// reindex.
func VerifyEmbedIdentity(recorded, current string) error {
	recorded = normalizeEmbedIdentity(strings.TrimSpace(recorded))
	current = strings.TrimSpace(current)
	if recorded == "" || recorded == current {
		return nil
	}
	return &ConfigError{
		Capability: CapEmbed,
		Profile:    current,
		Reason: fmt.Sprintf(
			"embed identity changed (index built with %q, configured %q); embeddings are corpus-lifetime — reindex to change the embed provider/model/dimension",
			recorded, current),
	}
}

// normalizeEmbedIdentity upgrades a legacy recorded identity to the
// current 6-field form before comparison, so upgrading an existing corpus
// that used native dimensions and no multimodal mode does not force a
// spurious reindex:
//   - pre-8.1.6 (3 fields: provider|text|code) → append "|0|0|off"
//   - pre-8.1.7 (5 fields: …|tdim|cdim)        → append "|off"
//
// Empty (fresh index) and already-6-field values are returned unchanged.
func normalizeEmbedIdentity(id string) string {
	if id == "" {
		return ""
	}
	switch strings.Count(id, "|") {
	case 2:
		return id + "|0|0|off"
	case 4:
		return id + "|off"
	default:
		return id
	}
}
