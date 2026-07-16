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
	"net"
	"net/url"
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
	// KindOmniEmbed is a self-hosted, OpenAI-compatible UNIFIED MULTIMODAL
	// embedding endpoint (dir2mcp#334): POST {base_url}/v1/embeddings serving
	// an OmniEmbed / Qwen2.5-Omni model (vLLM). Distinct from KindOpenAI
	// because, unlike a plain OpenAI-compatible embed endpoint, it embeds
	// text AND non-text media (images/audio/video/PDFs) into one shared
	// vector space (SPEC 8.1.7), so the matrix marks it statically
	// embed-capable and credential-optional, and the embed factory builds
	// the multimodal-capable adapter for it.
	KindOmniEmbed Kind = "omniembed"
	// KindColBERT is a self-hosted late-interaction / multi-vector
	// (ColBERT-style) reranking endpoint (dir2mcp#337): POST
	// {base_url}/rerank with a query + candidate documents, returning a
	// relevance score per document. Distinct from KindCohere so rerank
	// selection and the matrix can treat it as statically rerank-capable
	// (Supported) and credential-optional — the same self-hosted shape as
	// KindWhisper for STT. It pairs with self-hosted embeddings (#334) for a
	// fully-offline high-precision retrieval pipeline.
	KindColBERT Kind = "colbert"
)

// knownKinds is the set of recognized provider kinds (SPEC 8.1.1), in a
// stable order for deterministic error messages. A profile whose kind is not
// in this set has no row in the capability matrix, so it is silently
// un-selectable for every capability in auto selection and surfaces only as a
// generic ErrNoProvider far from its cause (issue #440 F7).
var knownKinds = []Kind{
	KindOpenAI, KindMistral, KindAnthropic, KindGemini, KindCohere,
	KindElevenLabs, KindWhisper, KindOmniEmbed, KindColBERT,
}

// IsKnownKind reports whether k is a recognized provider kind (SPEC 8.1.1).
// A profile declaring an unrecognized/typo kind is un-selectable for every
// capability; the config layer rejects it as CONFIG_INVALID at startup rather
// than letting it surface as a downstream "no provider" error (issue #440 F7).
func IsKnownKind(k Kind) bool {
	for _, known := range knownKinds {
		if k == known {
			return true
		}
	}
	return false
}

// KnownKindsString renders the recognized provider kinds as a comma-separated
// list for actionable CONFIG_INVALID remediation (issue #440 F7).
func KnownKindsString() string {
	names := make([]string, len(knownKinds))
	for i, k := range knownKinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

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
	// CapTranslate is transcript translation (SPEC §8.6.2). It is not a distinct
	// provider capability in the matrix — translation runs on a chat provider
	// (CapChat) — but it is a distinct DERIVATION kind: a translated transcript's
	// provenance and content-addressed cache key fold {translate, provider, model,
	// target-language} so a provider/model/target-language change re-derives and
	// never reads another derivation's cached bytes (§8.6.7). It is defined here so
	// translation reuses the one canonical derivationIdentity scheme rather than a
	// parallel ad-hoc one.
	CapTranslate Capability = "translate"
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
	KindOmniEmbed: {
		// Self-hosted OpenAI-compatible UNIFIED MULTIMODAL embed
		// (dir2mcp#334). Statically embed-capable: the standard
		// /v1/embeddings contract is fixed and the served model embeds text
		// and media into one shared vector space (SPEC 8.1.7), so — like
		// kind:whisper for STT — it is marked Supported rather than
		// EndpointDependent.
		CapEmbed: Supported,
	},
	KindColBERT: {
		// Self-hosted late-interaction (ColBERT-style) reranker (dir2mcp#337).
		// Statically rerank-capable: the self-hosted endpoint speaks a fixed
		// JSON rerank contract (query + documents -> per-document score), so
		// like KindWhisper for STT we mark it Supported and credential-optional
		// (a box on a private network needs no api_key).
		CapRerank: Supported,
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
	STTVAD bool
	// STTMaxPayloadMB / STTRequestTimeoutSec tune the self-hosted whisper client's
	// request limits (config `media.stt.max_payload_mb` / `media.stt.request_timeout_sec`,
	// dir2mcp#510/#511). 0 means "use the client's built-in default". Like STTVAD
	// these are operational knobs, not part of the STT identity, so changing them
	// is not reindex-bound.
	STTMaxPayloadMB      int
	STTRequestTimeoutSec int
	TTSModel             string
	TTSVoice             string // TTS voice id/name (e.g. ElevenLabs voice, OpenAI voice)
	RerankModel          string
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
// provider name + normalized embed base_url + text/code model + requested
// text/code output dimension (8.1.6) + multimodal mode (8.1.7) + late-chunking
// mode (issue #332/#446). It is recorded in the config snapshot/index and
// compared on load. Role (8.1.5) is deliberately excluded — it does not affect
// vector-space compatibility, but the base_url, requested dimension, multimodal
// mode, and late-chunking mode do.
//
// The normalized base_url (2nd field, SPEC 8.1.4 order
// provider|base_url|text_model|…) disambiguates two same-kind/same-model
// profiles that point at DIFFERENT endpoints (issue #560/#440 F3): without it
// they collapse to one identity and their vectors silently mix in one index.
// It enters in NORMALIZED form (normalizeEmbedBaseURL): a not-meaningful kind
// (native gemini/cohere) or a canonical/default/unset endpoint normalizes to
// "" so that an existing hosted-default corpus — and any index built before
// this rule — does NOT spuriously reindex; only an operator-overridden,
// non-canonical endpoint yields a non-empty component.
//
// lateChunking is the resolved value of ingest.late_chunking (config, not a
// provider attribute). It enters the identity because late chunking, once its
// pooling path is wired, produces context-pooled chunk vectors that are NOT
// comparable to chunk-then-embed vectors — so building a corpus with the mode
// on then off (or a distributed worker with a different setting) MUST re-derive
// rather than silently mix vector spaces (SPEC 8.1.4). The gate is conservative:
// it keys off the config flag, not the runtime TokenEmbedder capability (which
// EmbedIdentity cannot observe), so toggling the flag re-derives even in a build
// with no token-embedding provider — the safe direction.
func EmbedIdentity(p Profile, lateChunking bool) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%d|%s|%s",
		strings.TrimSpace(p.Name),
		normalizeEmbedBaseURL(p),
		strings.TrimSpace(p.EmbedTextModel),
		strings.TrimSpace(p.EmbedCodeModel),
		p.EmbedTextDim,
		p.EmbedCodeDim,
		NormalizeEmbedMultimodal(p.EmbedMultimodal),
		normalizeLateChunking(lateChunking))
}

// canonicalEmbedBaseURLByBuiltin maps a built-in embed profile NAME to the
// base_url it ships with, so an effective base_url equal to *that profile's own*
// shipped default canonicalizes to "" (SPEC 8.1.4 rule 2: "equals the built-in
// profile's shipped canonical base_url **for that provider**"). Matching is
// per-profile, NOT kind-wide: all four entries are `kind: openai`, but a profile
// is canonical only at its own endpoint. A `mistral` profile pointed at
// api.openai.com is therefore a non-canonical override that keeps a distinct,
// non-empty base_url component — a kind-wide set would instead collapse both to
// "" and let vectors from two different backends silently share one index (the
// exact mis-bind 8.1.4 exists to prevent). Native-surface kinds (gemini, cohere)
// never consult this table (rule 1); self-hosted kinds (omniembed) and any
// operator-named custom profile ship no canonical default, so any configured
// endpoint is meaningful.
//
// MUST stay in sync with config.builtinProfiles. The `openai` built-in ships no
// base_url; its client defaults to the OpenAI wire endpoint, recorded here so an
// explicit api.openai.com on an `openai` profile also collapses to "".
var canonicalEmbedBaseURLByBuiltin = map[string]string{
	"openai":     "https://api.openai.com/v1",    // client default for an empty base_url
	"mistral":    "https://api.mistral.ai/v1",    // the DEFAULT embed provider
	"openrouter": "https://openrouter.ai/api/v1", //
	"local":      "http://localhost:11434/v1",    // credential-less
}

// NormalizeEmbedBaseURL renders the profile's embed base_url as the stable
// component used in the embed identity (SPEC 8.1.4). See normalizeEmbedBaseURL;
// exported so the config layer can persist the same normalized value alongside
// the recorded identity (§6.4).
func NormalizeEmbedBaseURL(p Profile) string { return normalizeEmbedBaseURL(p) }

// normalizeEmbedBaseURL implements the SPEC 8.1.4 base_url normalization:
//
//   - Rule 1 (not meaningful): a kind whose embed endpoint is a single
//     canonical provider surface (native gemini / cohere) → "". base_url does
//     not participate.
//   - Rule 2 (canonical/default): an unset base_url, or one equal to *this
//     profile's own* built-in shipped default (canonicalEmbedBaseURLByBuiltin,
//     matched by profile name), → "". Only an operator-overridden, non-canonical
//     endpoint — including a built-in profile pointed at a *different* vendor's
//     host — yields a non-empty component.
//   - Rule 3 (canonicalization, non-empty case): canonicalizeEmbedBaseURL.
//
// "" is a first-class value (not "unknown"): an index built before this rule
// recorded no base_url and MUST stay valid against any provider that also
// normalizes to "" — so no default-endpoint corpus spuriously reindexes.
func normalizeEmbedBaseURL(p Profile) string {
	switch p.Kind {
	case KindGemini, KindCohere:
		return "" // Rule 1: native single-surface embed.
	}
	raw := strings.TrimSpace(p.BaseURL)
	if raw == "" {
		return "" // Rule 2: unset → canonical/default.
	}
	norm := canonicalizeEmbedBaseURL(raw)
	// Rule 2: collapse to "" only when the endpoint equals THIS profile's own
	// shipped default (per-profile, not kind-wide) — so a built-in pointed at a
	// foreign host stays distinct and cannot mix vector spaces.
	if def, ok := canonicalEmbedBaseURLByBuiltin[strings.TrimSpace(p.Name)]; ok {
		if norm == canonicalizeEmbedBaseURL(def) {
			return ""
		}
	}
	return norm
}

// canonicalizeEmbedBaseURL applies SPEC 8.1.4 rule 3 so endpoints that differ
// only cosmetically compare equal: lowercase scheme+host, drop the default
// port (80/http, 443/https), drop userinfo/query/fragment, strip trailing and
// collapse duplicate path slashes while PRESERVING the remaining path (e.g.
// /v1), and apply canonical percent-encoding. Path case is preserved (only the
// host is lowercased). Dropping userinfo also keeps credentials out of the
// recorded identity / any log line. An input that does not parse as an
// absolute URL (e.g. an unexpanded ${VAR}) is returned trimmed so the
// comparison stays deterministic.
func canonicalizeEmbedBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Not an absolute URL (e.g. an unexpanded ${VAR}, or a value url.Parse
		// rejects). Return a sanitized form rather than the raw string: it must
		// never carry a "|" (the embed-identity field delimiter — a leaked "|"
		// would corrupt normalizeEmbedIdentity's field count), a control
		// character, or a userinfo credential (this value flows into the recorded
		// identity, the config snapshot, and CLI output).
		return sanitizeOpaqueBaseURL(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.User = nil    // drop userinfo (never record/log a credential)
	u.RawQuery = "" // drop query
	u.Fragment = "" // drop fragment
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	// net.JoinHostPort brackets an IPv6 literal ("fe80::1" → "[fe80::1]:443");
	// a bare host-only IPv6 literal needs the brackets too (u.Hostname() strips
	// them). Manual host+":"+port would mis-serialize both.
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	// Collapse duplicate slashes and strip trailing slashes on the ESCAPED path,
	// so a genuine "/" separator is normalized while a percent-encoded slash
	// (%2F) — a distinct, non-separator path byte — is preserved rather than
	// silently folded into a separator (which would let two different endpoints
	// canonicalize to one identity).
	if esc := u.EscapedPath(); esc != "" {
		for strings.Contains(esc, "//") {
			esc = strings.ReplaceAll(esc, "//", "/")
		}
		if esc != "/" {
			esc = strings.TrimRight(esc, "/")
		} else {
			esc = ""
		}
		// Set Path (decoded) and RawPath (encoded) consistently so String()
		// re-emits the preserved encoding.
		u.RawPath = esc
		if dec, derr := url.PathUnescape(esc); derr == nil {
			u.Path = dec
		} else {
			u.Path = esc
		}
	}
	u.Opaque = ""
	return u.String()
}

// sanitizeOpaqueBaseURL renders a base_url that does not parse as an absolute URL
// into a value safe to record as an embed-identity component: it drops a
// userinfo credential prefix (…@ before the first path slash) and strips the
// identity delimiter "|" and control characters. It deliberately does not try to
// "fix" the value — an unexpanded ${VAR} passes through minus those hazards — so
// the comparison in normalizeEmbedBaseURL stays deterministic.
func sanitizeOpaqueBaseURL(raw string) string {
	if at := strings.IndexByte(raw, '@'); at >= 0 {
		if slash := strings.IndexByte(raw, '/'); slash < 0 || at < slash {
			raw = raw[at+1:] // drop a leading user[:pass]@ credential
		}
	}
	return strings.Map(func(r rune) rune {
		if r == '|' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, raw)
}

// normalizeLateChunking renders the late-chunking mode as the stable token
// used in the embed identity: "on" when enabled, "off" otherwise. Keeping it a
// named token (rather than a bare bool) leaves room for future pooling modes
// without another identity-field migration.
func normalizeLateChunking(on bool) string {
	if on {
		return "on"
	}
	return "off"
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

// normalizeEmbedIdentity upgrades a legacy recorded identity to the current
// 8-field form (provider|base_url|text|code|tdim|cdim|multimodal|late-chunking)
// before comparison, so upgrading an existing corpus that used native
// dimensions, no multimodal mode, no late chunking, and a hosted-default
// endpoint does not force a spurious reindex.
//
// EVERY pre-base_url form gains an EMPTY base_url component inserted at
// position 2 (issue #560/#440 F3) — an index built before the base_url rule
// recorded no endpoint, which is identity-equal to any current profile whose
// base_url normalizes to "" (all built-in/hosted-default deployments):
//   - pre-8.1.6 (3 fields: provider|text|code)             → insert "" + append "|0|0|off|off"
//   - pre-8.1.7 (5 fields: …|tdim|cdim)                    → insert "" + append "|off|off"
//   - pre-late-chunking (6 fields: …|multimodal, #446)     → insert "" + append "|off"
//   - pre-base_url (7 fields: …|multimodal|late-chunking)  → insert "" only
//
// Empty (fresh index) and already-8-field values are returned unchanged. A
// base_url component never contains "|" (canonicalizeEmbedBaseURL drops query/
// fragment and percent-encodes the path), so field counting is unambiguous.
func normalizeEmbedIdentity(id string) string {
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "|")
	switch len(parts) {
	case 3: // pre-8.1.6
		return insertEmbedBaseURLField(parts) + "|0|0|off|off"
	case 5: // pre-8.1.7
		return insertEmbedBaseURLField(parts) + "|off|off"
	case 6: // pre-late-chunking
		return insertEmbedBaseURLField(parts) + "|off"
	case 7: // pre-base_url (had the late-chunking field, no base_url)
		return insertEmbedBaseURLField(parts)
	default: // already 8 fields (current), or an unrecognized shape → unchanged
		return id
	}
}

// insertEmbedBaseURLField inserts an empty base_url component immediately after
// the provider name (position 2), the slot base_url now occupies in the embed
// identity (SPEC 8.1.4 order provider|base_url|…).
func insertEmbedBaseURLField(parts []string) string {
	out := make([]string, 0, len(parts)+1)
	out = append(out, parts[0], "")
	out = append(out, parts[1:]...)
	return strings.Join(out, "|")
}
