package config

// Provider model config (SPEC 0.7.0 §8.1 / §16.2). The legacy flat
// config keys keep using the bespoke hand-rolled parser; the dynamic
// `providers:` map + `model:` bindings are decoded here with yaml.v3
// (one self-contained subtree — the rest of config.go is unchanged).
//
// This file is additive: it exposes ResolveProvider/EmbedIdentity for
// the CLI wiring (C2-iii) without altering existing behavior.

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/usage"
	"gopkg.in/yaml.v3"
)

type providerProfileYAML struct {
	Kind           string  `yaml:"kind"`
	BaseURL        string  `yaml:"base_url"`
	APIKey         *string `yaml:"api_key"` // pointer: distinguish "absent" (credential-less) from ""
	EmbedTextModel string  `yaml:"embed_text_model"`
	EmbedCodeModel string  `yaml:"embed_code_model"`
	EmbedTextDim   int     `yaml:"embed_text_dim"`
	EmbedCodeDim   int     `yaml:"embed_code_dim"`
	ChatModel      string  `yaml:"chat_model"`
	OCRModel       string  `yaml:"ocr_model"`
	STTModel       string  `yaml:"stt_model"`
	STTLanguage    string  `yaml:"stt_language"`
	TTSModel       string  `yaml:"tts_model"`
	TTSVoice       string  `yaml:"tts_voice"`
	RerankModel    string  `yaml:"rerank_model"`
}

type capBindingYAML struct {
	Provider   string `yaml:"provider"`
	TextModel  string `yaml:"text_model"`
	CodeModel  string `yaml:"code_model"`
	TextDim    int    `yaml:"text_dim"`
	CodeDim    int    `yaml:"code_dim"`
	Multimodal string `yaml:"multimodal"`
	Model      string `yaml:"model"`
}

type providersDoc struct {
	Providers map[string]providerProfileYAML `yaml:"providers"`
	Model     struct {
		Embed capBindingYAML `yaml:"embed"`
		Chat  capBindingYAML `yaml:"chat"`
		OCR   capBindingYAML `yaml:"ocr"`
	} `yaml:"model"`
}

// builtinProfiles ship per SPEC 8.1.1 so operators usually only supply a
// credential. `local` is credential-less (no api_key). Users may
// override any of these or add new named profiles via the `providers:`
// map (merged per-field over these).
func builtinProfiles() map[string]providerProfileYAML {
	s := func(v string) *string { return &v }
	return map[string]providerProfileYAML{
		"mistral": {Kind: "openai", BaseURL: "https://api.mistral.ai/v1", APIKey: s("${MISTRAL_API_KEY}"),
			EmbedTextModel: "mistral-embed", EmbedCodeModel: "codestral-embed", ChatModel: "mistral-small-2506"},
		"mistral-ocr": {Kind: "mistral", APIKey: s("${MISTRAL_API_KEY}"), OCRModel: "mistral-ocr-latest", STTModel: "voxtral-mini-latest"},
		"openai":      {Kind: "openai", APIKey: s("${OPENAI_API_KEY}")},
		"openrouter":  {Kind: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKey: s("${OPENROUTER_API_KEY}")},
		"anthropic":   {Kind: "anthropic", APIKey: s("${ANTHROPIC_API_KEY}")},
		"gemini": {Kind: "gemini", APIKey: s("${GEMINI_API_KEY}"),
			EmbedTextModel: "gemini-embedding-001", EmbedCodeModel: "gemini-embedding-001", ChatModel: "gemini-2.5-flash"},
		"cohere":     {Kind: "cohere", APIKey: s("${COHERE_API_KEY}")},
		"elevenlabs": {Kind: "elevenlabs", APIKey: s("${ELEVENLABS_API_KEY}")},
		"local":      {Kind: "openai", BaseURL: "http://localhost:11434/v1"}, // credential-less
		// whisper: self-hosted OpenAI-compatible STT (GPU-VPS path,
		// dir2mcp#240). Credential-less by default (no api_key); operators
		// point base_url (and optionally set api_key/stt_model) via a
		// providers: entry or the WHISPER_BASE_URL env default. Excluded from
		// builtinPrecedence (like `local`) so it never silently wins auto
		// selection; reach it via stt_provider: whisper.
		"whisper": {Kind: "whisper", BaseURL: "${WHISPER_BASE_URL}"},
	}
}

// builtinPrecedence is the deterministic auto-selection order (SPEC
// 8.1.3): Mistral first (historical default), then the rest. User-only
// profiles are appended in declared order after these.
// `local` is intentionally excluded: it is credential-less and would
// otherwise silently win auto-selection when no real credential is
// set, masking a missing-credential misconfig (and pointing at a
// localhost endpoint that is usually not running). It remains fully
// usable via an explicit `model.<cap>.provider: local` binding.
var builtinPrecedence = []string{
	"mistral", "mistral-ocr", "openai", "gemini", "cohere",
	"anthropic", "elevenlabs", "openrouter",
}

// extractProvidersSubtree returns only the top-level `providers:` and
// `model:` blocks (key line + its indented/blank/comment continuation)
// so the new schema can be yaml.v3-parsed in isolation — the rest of
// the file uses the bespoke non-strict-YAML flat parser and must not
// reach yaml.v3. Returns nil when neither block is present.
func extractProvidersSubtree(raw []byte) []byte {
	var out []string
	capturing := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmedLeft := strings.TrimLeft(line, " \t")
		indented := line != trimmedLeft
		blank := strings.TrimSpace(line) == ""
		comment := strings.HasPrefix(trimmedLeft, "#")
		topKey := !indented && !blank && !comment && strings.Contains(trimmedLeft, ":")

		if topKey {
			key := strings.TrimSpace(strings.SplitN(trimmedLeft, ":", 2)[0])
			capturing = key == "providers" || key == "model"
		}
		if capturing {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return []byte(strings.Join(out, "\n"))
}

// extractTopLevelSubtree returns only the named top-level block (its key line
// plus the indented/blank/comment continuation), mirroring
// extractProvidersSubtree so a single block can be yaml.v3-parsed in isolation
// without the bespoke flat parser. Returns nil when the key is absent.
func extractTopLevelSubtree(raw []byte, want string) []byte {
	var out []string
	capturing := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmedLeft := strings.TrimLeft(line, " \t")
		indented := line != trimmedLeft
		blank := strings.TrimSpace(line) == ""
		comment := strings.HasPrefix(trimmedLeft, "#")
		topKey := !indented && !blank && !comment && strings.Contains(trimmedLeft, ":")

		if topKey {
			key := strings.TrimSpace(strings.SplitN(trimmedLeft, ":", 2)[0])
			capturing = key == want
		}
		if capturing {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return []byte(strings.Join(out, "\n"))
}

// costDoc is the yaml shape of the optional top-level `cost:` block:
//
//	cost:
//	  prices:
//	    my-model:
//	      input_per_1k: 0.0005
//	      output_per_1k: 0.0015
type costDoc struct {
	Cost struct {
		Prices map[string]struct {
			InputPer1K  float64 `yaml:"input_per_1k"`
			OutputPer1K float64 `yaml:"output_per_1k"`
		} `yaml:"prices"`
	} `yaml:"cost"`
}

// parseCostPriceOverrides decodes the optional cost.prices block into a price
// override map for per-query metrics (issue #327). Absent block ⇒ nil, no error.
func parseCostPriceOverrides(raw []byte) (map[string]usage.ModelPrice, error) {
	sub := extractTopLevelSubtree(raw, "cost")
	if len(sub) == 0 {
		return nil, nil
	}
	var doc costDoc
	if err := yaml.Unmarshal(sub, &doc); err != nil {
		return nil, fmt.Errorf("parse cost.prices config: %w", err)
	}
	if len(doc.Cost.Prices) == 0 {
		return nil, nil
	}
	out := make(map[string]usage.ModelPrice, len(doc.Cost.Prices))
	for name, p := range doc.Cost.Prices {
		out[name] = usage.ModelPrice{InputPer1K: p.InputPer1K, OutputPer1K: p.OutputPer1K}
	}
	return out, nil
}

func parseProvidersDoc(raw []byte) (providersDoc, error) {
	var doc providersDoc
	sub := extractProvidersSubtree(raw)
	if len(sub) == 0 {
		return doc, nil
	}
	if err := yaml.Unmarshal(sub, &doc); err != nil {
		return providersDoc{}, fmt.Errorf("parse providers/model config: %w", err)
	}
	return doc, nil
}

// mergeProfiles overlays user-declared profiles per-field over the
// built-ins and returns the merged set plus the deterministic
// precedence order (built-ins first, then user-only names in declared
// order — Go map order is non-deterministic so user-only ordering falls
// back to sorted names for stability).
func mergeProfiles(base, user map[string]providerProfileYAML) (map[string]providerProfileYAML, []string) {
	merged := base
	for name, up := range user {
		base, ok := merged[name]
		if !ok {
			merged[name] = up
			continue
		}
		if up.Kind != "" {
			base.Kind = up.Kind
		}
		if up.BaseURL != "" {
			base.BaseURL = up.BaseURL
		}
		if up.APIKey != nil {
			base.APIKey = up.APIKey
		}
		for _, f := range []struct {
			dst *string
			src string
		}{
			{&base.EmbedTextModel, up.EmbedTextModel}, {&base.EmbedCodeModel, up.EmbedCodeModel},
			{&base.ChatModel, up.ChatModel}, {&base.OCRModel, up.OCRModel},
			{&base.STTModel, up.STTModel}, {&base.STTLanguage, up.STTLanguage},
			{&base.TTSModel, up.TTSModel}, {&base.TTSVoice, up.TTSVoice},
			{&base.RerankModel, up.RerankModel},
		} {
			if f.src != "" {
				*f.dst = f.src
			}
		}
		merged[name] = base
	}
	order := append([]string(nil), builtinPrecedence...)
	var extra []string
	for name := range user {
		if _, isBuiltin := builtinProfiles()[name]; !isBuiltin {
			extra = append(extra, name)
		}
	}
	// stable order for user-only profiles
	for i := 0; i < len(extra); i++ {
		for j := i + 1; j < len(extra); j++ {
			if extra[j] < extra[i] {
				extra[i], extra[j] = extra[j], extra[i]
			}
		}
	}
	return merged, append(order, extra...)
}

// expandEnv resolves ${VAR} / $VAR references via getenv (SPEC 16.1.1
// env-sourced credentials). Empty getenv defaults to os.Getenv.
func expandEnv(v string, getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	return os.Expand(v, getenv)
}

// toProfiles converts the merged YAML profiles into resolved
// provider.Profile values (env-expanded). A profile whose api_key key
// is absent in YAML is credential-less (SPEC 8.1.1).
func toProfiles(merged map[string]providerProfileYAML, getenv func(string) string) map[string]provider.Profile {
	out := make(map[string]provider.Profile, len(merged))
	for name, p := range merged {
		key := ""
		credLess := p.APIKey == nil
		if p.APIKey != nil {
			key = strings.TrimSpace(expandEnv(*p.APIKey, getenv))
		}
		out[name] = provider.Profile{
			Name:           name,
			Kind:           provider.Kind(strings.TrimSpace(p.Kind)),
			BaseURL:        strings.TrimSpace(expandEnv(p.BaseURL, getenv)),
			APIKey:         key,
			CredentialLess: credLess,
			EmbedTextModel: p.EmbedTextModel,
			EmbedCodeModel: p.EmbedCodeModel,
			EmbedTextDim:   p.EmbedTextDim,
			EmbedCodeDim:   p.EmbedCodeDim,
			ChatModel:      p.ChatModel,
			OCRModel:       p.OCRModel,
			STTModel:       p.STTModel,
			STTLanguage:    p.STTLanguage,
			TTSModel:       p.TTSModel,
			TTSVoice:       p.TTSVoice,
			RerankModel:    p.RerankModel,
		}
	}
	return out
}

// ProviderResolution is the resolved provider model for a loaded config.
type ProviderResolution struct {
	byName     map[string]provider.Profile
	precedence []provider.Profile
	doc        providersDoc
}

// providersResolution builds the resolution from the parsed doc + env.
func (d providersDoc) resolve(base map[string]providerProfileYAML, getenv func(string) string) ProviderResolution {
	merged, order := mergeProfiles(base, d.Providers)
	byName := toProfiles(merged, getenv)
	prec := make([]provider.Profile, 0, len(order))
	for _, n := range order {
		if p, ok := byName[n]; ok {
			prec = append(prec, p)
		}
	}
	return ProviderResolution{byName: byName, precedence: prec, doc: d}
}

// ByName returns the resolved (built-in + user, env-expanded) profiles
// keyed by name. Used by tests and CLI introspection (C2-iii).
func (r ProviderResolution) ByName() map[string]provider.Profile {
	return r.byName
}

// OCRProviderName returns the explicit `model.ocr.provider` binding (SPEC
// §16.2), trimmed, or "" when unset (auto). The ingest OCR resolution uses
// it to honor a self-hosted bespoke-OCR profile (a `kind: mistral` profile on
// a custom `base_url`, dir2mcp#240) instead of always assuming the built-in
// `mistral-ocr` profile. An empty result means "use the historical default
// profile".
func (r ProviderResolution) OCRProviderName() string {
	return strings.TrimSpace(r.doc.Model.OCR.Provider)
}

func (r ProviderResolution) explicit(cap provider.Capability) string {
	switch cap {
	case provider.CapEmbed:
		return r.doc.Model.Embed.Provider
	case provider.CapChat:
		return r.doc.Model.Chat.Provider
	case provider.CapOCR:
		return r.doc.Model.OCR.Provider
	default:
		return ""
	}
}

// Resolve selects the profile for cap (SPEC 8.1.3). embed is required;
// other capabilities are optional (caller decides preflight failure).
func (r ProviderResolution) Resolve(cap provider.Capability) (provider.Profile, error) {
	required := cap == provider.CapEmbed
	p, err := provider.Select(r.precedence, r.byName, cap, r.explicit(cap), required)
	if err != nil {
		return p, err
	}
	return r.applyModelOverrides(cap, p), nil
}

// applyModelOverrides overlays model.<cap>.{text_model,code_model,model}
// onto the selected profile (SPEC §16.2). These were parsed into
// capBindingYAML but previously ignored.
func (r ProviderResolution) applyModelOverrides(cap provider.Capability, p provider.Profile) provider.Profile {
	set := func(dst *string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	switch cap {
	case provider.CapEmbed:
		set(&p.EmbedTextModel, r.doc.Model.Embed.TextModel)
		set(&p.EmbedCodeModel, r.doc.Model.Embed.CodeModel)
		if r.doc.Model.Embed.TextDim > 0 {
			p.EmbedTextDim = r.doc.Model.Embed.TextDim
		}
		if r.doc.Model.Embed.CodeDim > 0 {
			p.EmbedCodeDim = r.doc.Model.Embed.CodeDim
		}
		set(&p.EmbedMultimodal, r.doc.Model.Embed.Multimodal)
	case provider.CapChat:
		set(&p.ChatModel, r.doc.Model.Chat.Model)
	case provider.CapOCR:
		set(&p.OCRModel, r.doc.Model.OCR.Model)
	}
	return p
}

// ResolveExplicit selects for cap with an explicit profile name (or ""
// for auto), applying the same matrix/eligibility rules as Resolve plus
// the model-name overrides. Used for capabilities whose selector is not
// in the model: block (e.g. the legacy stt.provider) during the
// transition.
func (r ProviderResolution) ResolveExplicit(cap provider.Capability, explicit string, required bool) (provider.Profile, error) {
	p, err := provider.Select(r.precedence, r.byName, cap, strings.TrimSpace(explicit), required)
	if err != nil {
		return p, err
	}
	return r.applyModelOverrides(cap, p), nil
}

// resolveSTTProfileForCapability resolves the active STT provider profile the
// same way the ingest service does (SPEC 8.1.3), so config-time diarization
// gating observes the exact backend that will actually transcribe. It returns
// ok=false when STT is off, when no STT-capable profile resolves, or when the
// selector is unrecognised. Kept package-local and selector-table-driven so its
// cyclomatic complexity stays flat.
func resolveSTTProfileForCapability(cfg Config) (provider.Profile, bool) {
	sel := strings.ToLower(strings.TrimSpace(cfg.STTProvider))
	if sel == "" {
		sel = "auto"
	}
	switch sel {
	case "off", "none", "disabled":
		return provider.Profile{}, false
	}
	r := cfg.Providers()
	explicitBySelector := map[string]string{
		"mistral":    "mistral-ocr",
		"elevenlabs": "elevenlabs",
		"whisper":    "whisper",
	}
	var (
		prof provider.Profile
		err  error
	)
	if sel == "auto" {
		prof, err = r.Resolve(provider.CapSTT)
	} else if explicit, ok := explicitBySelector[sel]; ok {
		prof, err = r.ResolveExplicit(provider.CapSTT, explicit, true)
	} else {
		return provider.Profile{}, false
	}
	if err != nil {
		return provider.Profile{}, false
	}
	return prof, true
}

// diarizeStateForProfile resolves the effective diarization activation for the
// given (already-resolved) STT profile under the tri-state config (SPEC §8.6.8):
//
//   - enabled == nil (auto): active iff the backend advertises CapDiarize
//     (capability-driven activation).
//   - enabled == false: always inactive (the kill switch).
//   - enabled == true: REQUIRED — active when the backend is capable; when it is
//     NOT, capable is false and the caller (ValidateDiarization) maps that to
//     CONFIG_INVALID.
//
// It returns (active, capable): active is the resolved on/off; capable reports
// whether the backend advertises the capability (used to detect the
// required-but-incapable error).
func diarizeStateForProfile(enabled *bool, prof provider.Profile) (active, capable bool) {
	capable = provider.Can(prof.Kind, provider.CapDiarize) != provider.Unsupported
	switch {
	case enabled != nil && !*enabled:
		return false, capable
	case enabled != nil && *enabled:
		return capable, capable
	default: // auto
		return capable, capable
	}
}

// DiarizationActive reports whether speaker diarization is active for
// model-derived transcripts given the config and the resolved STT profile (SPEC
// §8.6.8). It is the single decision point the ingest service uses to decide
// whether to record diarize provenance and fold the diarize identity into the
// transcript derivation identity. It never errors: the required-but-incapable
// case is surfaced as CONFIG_INVALID by ValidateDiarization at startup, not here.
func DiarizationActive(cfg Config, prof provider.Profile) bool {
	active, _ := diarizeStateForProfile(cfg.MediaDiarizeEnabled, prof)
	return active
}

// validateMediaDiarize enforces the diarization config invariants (SPEC §8.6.8):
// diarization requires a diarization-capable STT backend. When
// media.diarize.enabled is true but no configured STT backend advertises the
// capability, startup MUST fail CONFIG_INVALID with remediation. The auto (nil)
// and explicit-false states never fail: auto simply stays off on an incapable
// backend, and false forces it off. With STT off entirely, an explicit true is
// also CONFIG_INVALID (there is no backend to diarize).
func (c *Config) validateMediaDiarize() error {
	enabled := c.MediaDiarizeEnabled
	// Only an explicit `true` can fail validation; auto/false are always valid.
	if enabled == nil || !*enabled {
		return nil
	}
	prof, ok := resolveSTTProfileForCapability(*c)
	if !ok {
		return fmt.Errorf(
			"CONFIG_INVALID: media.diarize.enabled=true requires a diarization-capable STT backend, but speech-to-text is not configured; set stt.provider to a diarization-capable backend (e.g. a self-hosted WhisperX/pyannote endpoint via stt.provider=whisper) or remove media.diarize.enabled")
	}
	if _, capable := diarizeStateForProfile(enabled, prof); !capable {
		return fmt.Errorf(
			"CONFIG_INVALID: media.diarize.enabled=true but the active STT provider %q (kind %q) does not advertise speaker diarization; use a diarization-capable backend (e.g. a self-hosted WhisperX/pyannote endpoint via stt.provider=whisper), set media.diarize.enabled=false, or omit it to auto-enable only when supported",
			prof.Name, prof.Kind)
	}
	return nil
}

// readSnapshotEmbedIdentity returns the embed_identity recorded in the
// effective snapshot for stateDir, or "" if there is no snapshot / no
// recorded identity (a fresh index — VerifyEmbedIdentity treats that as
// always-compatible).
func readSnapshotEmbedIdentity(stateDir string) string {
	raw, err := os.ReadFile(EffectiveSnapshotPath(stateDir))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line != strings.TrimLeft(line, " \t") { // top-level only
			continue
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "embed_identity:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// VerifyEmbedIdentity enforces the SPEC 8.1.4 corpus-lifetime invariant:
// if a prior snapshot recorded a different embed identity than the one
// resolved now, refuse to serve (the index's vectors are not comparable
// across embed providers/models). A fresh state dir (no snapshot)
// always passes. Returns a *provider.ConfigError on mismatch.
func (cfg Config) VerifyEmbedIdentity(stateDir string) error {
	recorded := readSnapshotEmbedIdentity(stateDir)
	current := cfg.Providers().EmbedIdentity()
	return provider.VerifyEmbedIdentity(recorded, current)
}

// EmbedIdentity is the corpus-lifetime identity of the resolved embed
// provider (SPEC 8.1.4), or "" if embed cannot be resolved.
func (r ProviderResolution) EmbedIdentity() string {
	p, err := r.Resolve(provider.CapEmbed)
	if err != nil {
		return ""
	}
	return provider.EmbedIdentity(p)
}

// Providers returns the provider resolution for cfg using os.Getenv for
// credential expansion (SPEC §8.1). The CLI wiring (C2-iii) calls
// Resolve per capability and builds the adapter via providerfactory.
func (cfg Config) Providers() ProviderResolution {
	base := builtinProfiles()
	seedLegacy(base, cfg)
	return cfg.providersDoc.resolve(base, nil)
}

// ProviderEnvVarRefs returns the distinct environment variable names
// referenced in api_key fields across all provider profiles (builtin +
// user-defined). For example, a builtin profile with api_key "${MISTRAL_API_KEY}"
// contributes "MISTRAL_API_KEY", and a user profile with api_key "${MY_KEY}"
// contributes "MY_KEY". Used by `dir2mcp service install` to auto-persist
// every relevant credential to .env.local, not just the hardcoded list.
func (cfg Config) ProviderEnvVarRefs() []string {
	base := builtinProfiles()
	seedLegacy(base, cfg)
	merged, _ := mergeProfiles(base, cfg.providersDoc.Providers)

	seen := make(map[string]struct{})
	var refs []string
	for _, p := range merged {
		if p.APIKey == nil {
			continue
		}
		os.Expand(*p.APIKey, func(key string) string {
			if key != "" {
				if _, ok := seen[key]; !ok {
					seen[key] = struct{}{}
					refs = append(refs, key)
				}
			}
			return ""
		})
	}
	sort.Strings(refs)
	return refs
}

// litStr returns a pointer to s, used to set a providerProfileYAML
// api_key to a concrete literal (distinct from an absent/credential-less
// nil api_key).
func litStr(s string) *string { return &s }

// setStr overwrites *dst with v only when v is non-empty, so a blank
// flat-config value never clobbers a built-in profile default.
func setStr(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// seedLegacy overlays the spec-retained flat stt:/rerank: config
// (SPEC 0.7.0 §16.2 retains these shapes) onto the built-in profiles
// so the resolver honors them. The monolithic mistral chat/embed
// surface was removed in the clean break (C2-iii); only the
// STT/rerank-relevant settings are bridged here. User `providers:`
// entries still take precedence (merged on top of this seed).
func seedLegacy(m map[string]providerProfileYAML, cfg Config) {
	seed := func(name string, fn func(*providerProfileYAML)) {
		if p, ok := m[name]; ok {
			fn(&p)
			m[name] = p
		}
	}
	// The monolithic `mistral` chat/embed profile is fully removed
	// config (clean break): its credential resolves via the built-in
	// ${MISTRAL_API_KEY} placeholder or an explicit providers: entry.
	seed("mistral-ocr", func(p *providerProfileYAML) { seedMistralOCR(p, cfg) })
	seed("cohere", func(p *providerProfileYAML) { seedCohere(p, cfg) })
	seed("elevenlabs", func(p *providerProfileYAML) { seedElevenLabs(p, cfg) })
}

// seedMistralOCR bridges only the spec-retained Mistral STT model
// (stt.mistral.model, §16.2) onto the mistral-ocr profile. The
// credential and base URL are no longer flat config — the profile
// keeps its built-in ${MISTRAL_API_KEY} / default base URL.
func seedMistralOCR(p *providerProfileYAML, cfg Config) {
	setStr(&p.STTModel, cfg.STTMistralModel)
}

// seedCohere bridges the spec-retained flat rerank.cohere config
// (api_key/base_url/model, SPEC 0.7.0 §16.2) onto the cohere profile.
func seedCohere(p *providerProfileYAML, cfg Config) {
	if cfg.CohereAPIKey != "" {
		p.APIKey = litStr(cfg.CohereAPIKey)
	}
	setStr(&p.BaseURL, cfg.CohereBaseURL)
	setStr(&p.RerankModel, cfg.RerankModel)
}

// seedElevenLabs bridges the spec-retained flat stt.elevenlabs config
// (api_key/base_url/voice/model/language, SPEC 0.7.0 §16.2) onto the
// elevenlabs profile.
func seedElevenLabs(p *providerProfileYAML, cfg Config) {
	if cfg.ElevenLabsAPIKey != "" {
		p.APIKey = litStr(cfg.ElevenLabsAPIKey)
	}
	setStr(&p.BaseURL, cfg.ElevenLabsBaseURL)
	setStr(&p.TTSVoice, cfg.ElevenLabsTTSVoiceID)
	setStr(&p.STTModel, cfg.STTElevenLabsModel)
	setStr(&p.STTLanguage, cfg.STTElevenLabsLanguageCode)
}
