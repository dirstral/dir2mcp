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
	// STTLanguages is the profile's OPTIONAL declared STT language coverage
	// (SPEC §8.2.1, dir2mcp #566): a non-empty set of BCP-47 tags the model is
	// known to transcribe well. Unset or empty = open/unknown (no coverage
	// assertion); a declared, non-empty set drives the honest-coverage warning
	// when the effective source language falls outside it.
	STTLanguages []string `yaml:"stt_languages"`
	TTSModel     string   `yaml:"tts_model"`
	TTSVoice     string   `yaml:"tts_voice"`
	RerankModel  string   `yaml:"rerank_model"`
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
	// declOrder is the order the profile names appear in the `providers:`
	// mapping. yaml.v3 decodes the mapping into a Go map, which loses key
	// order, so declared order is recovered separately (see
	// providerDeclOrder) and used to order user-only profiles in the
	// auto-selection precedence (SPEC 8.1.3). Empty when there is no
	// providers block or order recovery failed.
	declOrder []string
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
		// omniembed: self-hosted OpenAI-compatible UNIFIED MULTIMODAL embed
		// (dir2mcp#334). Credential-less by default (no api_key); operators
		// point base_url (and optionally api_key/embed models) via a
		// providers: entry or the OMNIEMBED_BASE_URL env default. Excluded
		// from builtinPrecedence (like `local`/`whisper`) so it never silently
		// wins embed auto-selection; reach it via model.embed.provider:
		// omniembed. Multimodal embedding is opt-in via model.embed.multimodal.
		"omniembed": {Kind: "omniembed", BaseURL: "${OMNIEMBED_BASE_URL}"},
		// colbert: self-hosted late-interaction / multi-vector reranker
		// (dir2mcp#337). Credential-less by default (no api_key); operators
		// point base_url (and optionally set api_key/rerank_model) via a
		// providers: entry or the COLBERT_BASE_URL env default. Excluded from
		// builtinPrecedence (like `local`/`whisper`) so it never silently wins
		// rerank auto-selection over the hosted cohere path; reach it via
		// rerank.provider: colbert.
		"colbert": {Kind: "colbert", BaseURL: "${COLBERT_BASE_URL}"},
	}
}

// builtinPrecedence is the deterministic auto-selection order (SPEC
// 8.1.3): Mistral first (historical default), then the rest. User-only
// profiles are appended after these in the order they are declared in the
// YAML `providers:` mapping (recovered by providerDeclOrder; issue #440 F8) —
// or, if declared order cannot be recovered, in sorted name order for
// determinism.
//
// `mistral-ocr` (kind: mistral — the Voxtral STT / Mistral-OCR path) precedes
// `mistral` (kind: openai — the OpenAI-compatible chat/embed path). This
// ordering is what makes `stt_provider: auto` resolve to the intended Voxtral
// backend: for STT, `mistral` is only EndpointDependent (an arbitrary
// api.mistral.ai/v1 base URL is not guaranteed to serve /v1/audio/transcriptions)
// while `mistral-ocr` is statically Supported, and 8.1.3 auto selection takes the
// FIRST eligible+capable profile in precedence order. With `mistral` first, auto
// STT silently bound the OpenAI-compat transcriber and never used the seeded
// Voxtral model (issue #440 F4). `mistral-ocr` carries no embed/chat capability,
// so ordering it ahead of `mistral` leaves embed/chat/ocr auto selection
// unchanged — it is skipped for those and `mistral` still wins.
//
// `local` is intentionally excluded: it is credential-less and would
// otherwise silently win auto-selection when no real credential is
// set, masking a missing-credential misconfig (and pointing at a
// localhost endpoint that is usually not running). It remains fully
// usable via an explicit `model.<cap>.provider: local` binding.
var builtinPrecedence = []string{
	"mistral-ocr", "mistral", "openai", "gemini", "cohere",
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

// parseMediaTranslateGlossary decodes the OPTIONAL, per-target-language
// `media.translate.glossary` nested map (SPEC §8.6.2, issue #574):
//
//	media:
//	  translate:
//	    glossary:
//	      es:
//	        "United Nations": "Naciones Unidas"
//
// It is a map-of-maps, so — unlike the scalar/list `media.*` keys handled by the
// bespoke flat parser — it is decoded with yaml.v3 from the glossary block
// extracted in isolation (extractMediaTranslateGlossarySubtree), never exposing
// unrelated `media:` scalars to a strict YAML decode. Language keys are
// lower-cased to match the normalized target_langs used at lookup time; blank
// languages/terms/renderings are dropped. Absent block ⇒ nil (no guidance).
func parseMediaTranslateGlossary(raw []byte) (map[string]map[string]string, error) {
	sub := extractMediaTranslateGlossarySubtree(raw)
	if len(sub) == 0 {
		return nil, nil
	}
	var doc struct {
		Glossary map[string]map[string]string `yaml:"glossary"`
	}
	if err := yaml.Unmarshal(sub, &doc); err != nil {
		return nil, fmt.Errorf("parse media.translate.glossary config: %w", err)
	}
	return normalizeTranslateGlossary(doc.Glossary)
}

// normalizeTranslateGlossary lower-cases each target-language key (matching the
// normalized media.translate.target_langs used at lookup), trims each source
// term and rendering, and drops blank languages/terms/renderings. Source-term and
// rendering CASE is preserved (only the language tag is folded). Returns nil when
// nothing survives so an empty/whitespace-only glossary is indistinguishable from
// unset (today's no-guidance behaviour).
//
// Two raw keys that collide after normalization ("es" and " ES " → "es"; " term "
// and "term" → "term") are a config error, not a silent last-writer-wins overwrite
// (which would be nondeterministic under Go map iteration): return CONFIG_INVALID
// so the operator disambiguates.
func normalizeTranslateGlossary(in map[string]map[string]string) (map[string]map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]map[string]string, len(in))
	langSeen := make(map[string]string, len(in)) // normalized lang → first raw key
	for lang, terms := range in {
		l := strings.ToLower(strings.TrimSpace(lang))
		if l == "" || len(terms) == 0 {
			continue
		}
		if prev, dup := langSeen[l]; dup {
			return nil, fmt.Errorf("CONFIG_INVALID: media.translate.glossary target languages %q and %q both normalize to %q", prev, lang, l)
		}
		langSeen[l] = lang
		m := make(map[string]string, len(terms))
		srcSeen := make(map[string]string, len(terms)) // trimmed source term → first raw key
		for src, rendering := range terms {
			s := strings.TrimSpace(src)
			r := strings.TrimSpace(rendering)
			if s == "" || r == "" {
				continue
			}
			if prev, dup := srcSeen[s]; dup {
				return nil, fmt.Errorf("CONFIG_INVALID: media.translate.glossary[%q] source terms %q and %q both normalize to %q", l, prev, src, s)
			}
			srcSeen[s] = src
			m[s] = r
		}
		if len(m) > 0 {
			out[l] = m
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// extractMediaTranslateGlossarySubtree returns the `glossary:` block nested under
// `media:` → `translate:` (its key line plus indented continuation), dedented to
// column 0 so yaml.v3 can decode it in isolation, or nil when absent. It tracks
// the media→translate→glossary nesting by indentation so it does NOT match the
// unrelated, list-valued `media.subtitles.glossary` (§8.6.3).
func extractMediaTranslateGlossarySubtree(raw []byte) []byte {
	lines := strings.Split(string(raw), "\n")
	start, base := findMediaTranslateGlossaryHeader(lines)
	if start < 0 {
		return nil
	}
	out := []string{lines[start][base:]} // dedented `glossary:` header
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, "")
			continue
		}
		if len(line)-len(trimmed) <= base {
			break // dedented back to/above the header: end of the glossary block
		}
		out = append(out, line[base:]) // strip the header indent, keep relative nesting
	}
	return []byte(strings.Join(out, "\n"))
}

// findMediaTranslateGlossaryHeader locates the `glossary:` line nested exactly
// under a top-level `media:` → `translate:` mapping, returning its line index and
// leading-space indent (or -1 when absent). It walks an indentation stack of
// (indent, key) frames so it matches only the translate glossary, never
// media.subtitles.glossary.
func findMediaTranslateGlossaryHeader(lines []string) (idx, indent int) {
	type frame struct {
		indent int
		key    string
	}
	var stack []frame
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := len(line) - len(trimmed)
		for len(stack) > 0 && stack[len(stack)-1].indent >= ind {
			stack = stack[:len(stack)-1]
		}
		key := strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[0])
		stack = append(stack, frame{indent: ind, key: key})
		if len(stack) == 3 && stack[0].indent == 0 &&
			stack[0].key == "media" && stack[1].key == "translate" && stack[2].key == "glossary" {
			return i, ind
		}
	}
	return -1, -1
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

// carbonDoc is the yaml shape of the optional top-level `carbon:` block for the
// opt-in energy/CO2e estimate (issue #328):
//
//	carbon:
//	  enabled: true
//	  grid_g_co2e_per_wh: 0.35   # optional; omit/<=0 to skip the CO2e estimate
//	  energy:                     # optional per-model Wh/1K-token overrides
//	    my-model:
//	      wh_per_1k: 0.5
//
// gridSet distinguishes "operator set 0/negative" from "unset" so the loader
// can apply the built-in default grid factor only when the key is absent.
type carbonDoc struct {
	Carbon struct {
		Enabled        bool     `yaml:"enabled"`
		GridGCO2ePerWh *float64 `yaml:"grid_g_co2e_per_wh"`
		Energy         map[string]struct {
			WhPer1K float64 `yaml:"wh_per_1k"`
		} `yaml:"energy"`
	} `yaml:"carbon"`
}

// CarbonConfig is the resolved, opt-in energy/carbon estimate configuration
// (issue #328). Disabled by default; all factors operator-overridable.
type CarbonConfig struct {
	// Enabled gates the entire estimate. Off by default.
	Enabled bool
	// EnergyOverrides maps a model name to a Wh-per-1K-token factor, overriding
	// the built-in defaults. nil/empty ⇒ built-in defaults only.
	EnergyOverrides map[string]usage.EnergyFactor
	// GridGramsCO2ePerWh is the grid carbon-intensity factor in gCO2e/Wh. When
	// the key is absent it defaults to usage.DefaultGridIntensityGramsPerWh; a
	// value <= 0 disables the CO2e estimate (Wh is still surfaced).
	GridGramsCO2ePerWh float64
}

// parseCarbonConfig decodes the optional top-level `carbon:` block (issue #328).
// Absent block ⇒ zero value (disabled), no error. When present, an unset grid
// factor falls back to the built-in default so an enabled-but-minimal config
// still produces a CO2e estimate.
func parseCarbonConfig(raw []byte) (CarbonConfig, error) {
	var cfg CarbonConfig
	sub := extractTopLevelSubtree(raw, "carbon")
	if len(sub) == 0 {
		return cfg, nil
	}
	var doc carbonDoc
	if err := yaml.Unmarshal(sub, &doc); err != nil {
		return cfg, fmt.Errorf("parse carbon config: %w", err)
	}
	cfg.Enabled = doc.Carbon.Enabled
	if doc.Carbon.GridGCO2ePerWh != nil {
		cfg.GridGramsCO2ePerWh = *doc.Carbon.GridGCO2ePerWh
	} else {
		cfg.GridGramsCO2ePerWh = usage.DefaultGridIntensityGramsPerWh
	}
	if len(doc.Carbon.Energy) > 0 {
		cfg.EnergyOverrides = make(map[string]usage.EnergyFactor, len(doc.Carbon.Energy))
		for name, f := range doc.Carbon.Energy {
			cfg.EnergyOverrides[name] = usage.EnergyFactor{WhPer1K: f.WhPer1K}
		}
	}
	return cfg, nil
}

// mediaSTTLanguageProvidersDoc is the yaml shape of the optional
// media.stt.language_providers nested map (SPEC §8.2.1, #566):
//
//	media:
//	  stt:
//	    language_providers:
//	      ru: whisper-ru
//	      en: whisper
//
// Unknown sibling keys under media/stt are ignored (yaml.v3 default), so this
// coexists with the flat parser that reads the scalar media.stt.* fields.
type mediaSTTLanguageProvidersDoc struct {
	Media struct {
		STT struct {
			LanguageProviders map[string]string `yaml:"language_providers"`
		} `yaml:"stt"`
	} `yaml:"media"`
}

// parseMediaSTTLanguageProviders decodes the optional media.stt.language_providers
// map into a route table keyed by the BCP-47 PRIMARY language subtag (SPEC §8.2.1,
// #566), so a "ru" route matches a "ru-RU" pin and vice versa — the same matching
// rule the honest-coverage check uses. Absent block ⇒ nil, no error. Two keys that
// collapse to the same primary subtag but name DIFFERENT profiles are rejected
// (ambiguous route), so lookup is deterministic.
func parseMediaSTTLanguageProviders(raw []byte) (map[string]string, error) {
	sub := extractTopLevelSubtree(raw, "media")
	if len(sub) == 0 {
		return nil, nil
	}
	var doc mediaSTTLanguageProvidersDoc
	if err := yaml.Unmarshal(sub, &doc); err != nil {
		return nil, fmt.Errorf("parse media.stt.language_providers config: %w", err)
	}
	in := doc.Media.STT.LanguageProviders
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for lang, name := range in {
		key := provider.PrimarySubtag(lang)
		name = strings.TrimSpace(name)
		if key == "" {
			continue
		}
		if existing, dup := out[key]; dup && existing != name {
			return nil, fmt.Errorf(
				"CONFIG_INVALID: media.stt.language_providers has conflicting routes for language %q (%q vs %q)",
				key, existing, name)
		}
		out[key] = name
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
	doc.declOrder = providerDeclOrder(sub)
	return doc, nil
}

// providerDeclOrder recovers the profile names in the order they are declared
// in the `providers:` mapping (issue #440 F8). yaml.v3 decodes a mapping into a
// Go map, which loses key order, so the declared order — the SPEC 8.1.3
// precedence for user-only profiles — is recovered by walking the mapping
// node's Content (keys sit at even indices). Returns nil when there is no
// `providers:` block or the subtree cannot be node-decoded, in which case the
// caller falls back to a deterministic sorted order.
func providerDeclOrder(sub []byte) []string {
	var root yaml.Node
	if err := yaml.Unmarshal(sub, &root); err != nil || len(root.Content) == 0 {
		return nil
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "providers" {
			continue
		}
		pmap := top.Content[i+1]
		if pmap.Kind != yaml.MappingNode {
			return nil
		}
		order := make([]string, 0, len(pmap.Content)/2)
		for j := 0; j+1 < len(pmap.Content); j += 2 {
			order = append(order, pmap.Content[j].Value)
		}
		return order
	}
	return nil
}

// mergeProfiles overlays user-declared profiles per-field over the
// built-ins and returns the merged set plus the deterministic
// precedence order: built-ins first (builtinPrecedence), then user-only
// names in declared order (SPEC 8.1.3). declOrder carries the order the
// profiles appear in the YAML `providers:` mapping (recovered by
// providerDeclOrder, since a decoded Go map loses key order); any user-only
// profile not present in declOrder — the defensive path when order recovery
// fails — is appended in sorted name order so the result is always
// deterministic (issue #440 F8).
func mergeProfiles(base, user map[string]providerProfileYAML, declOrder []string) (map[string]providerProfileYAML, []string) {
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
		// Carry the per-axis requested embedding dimensions (SPEC 8.1.6,
		// Matryoshka/MRL). These are ints, not strings, so they are merged
		// separately: a non-zero override wins, zero leaves the built-in dim
		// intact. Omitting them here silently dropped a `providers:` profile
		// override of embed_text_dim/embed_code_dim (issue #440 F1), resetting
		// the effective embedding dimension and recording dim=0 in the embed
		// identity.
		if up.EmbedTextDim != 0 {
			base.EmbedTextDim = up.EmbedTextDim
		}
		if up.EmbedCodeDim != 0 {
			base.EmbedCodeDim = up.EmbedCodeDim
		}
		// STTLanguages (declared STT coverage, #566) is a slice: a non-empty
		// override replaces the built-in's set wholesale; an omitted/empty one
		// leaves the base intact (matching the "unset => open/unknown" contract).
		if len(up.STTLanguages) > 0 {
			base.STTLanguages = append([]string(nil), up.STTLanguages...)
		}
		merged[name] = base
	}
	order := append([]string(nil), builtinPrecedence...)
	return merged, append(order, userOnlyOrder(user, declOrder)...)
}

// userOnlyOrder returns the non-built-in profile names in auto-selection
// precedence order (SPEC 8.1.3): first the names present in declOrder (the YAML
// declaration order recovered by providerDeclOrder, de-duplicated), then any
// user-only profile declOrder did not cover — the defensive path when order
// recovery fails or is partial — in sorted name order so the result is always
// deterministic regardless of Go map iteration (issue #440 F8).
func userOnlyOrder(user map[string]providerProfileYAML, declOrder []string) []string {
	builtins := builtinProfiles()
	isUserOnly := func(name string) bool {
		if _, ok := user[name]; !ok {
			return false
		}
		_, isBuiltin := builtins[name]
		return !isBuiltin
	}
	var extra []string
	seen := make(map[string]struct{}, len(user))
	for _, name := range declOrder {
		if _, dup := seen[name]; dup || !isUserOnly(name) {
			continue
		}
		seen[name] = struct{}{}
		extra = append(extra, name)
	}
	var leftover []string
	for name := range user {
		if _, ok := seen[name]; ok || !isUserOnly(name) {
			continue
		}
		leftover = append(leftover, name)
	}
	sort.Strings(leftover)
	return append(extra, leftover...)
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
			STTLanguages:   append([]string(nil), p.STTLanguages...),
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
	// lateChunking is the resolved ingest.late_chunking flag (issue #332/#446),
	// stamped by Config.Providers so EmbedIdentity folds the late-chunking mode
	// into the corpus-lifetime identity (SPEC 8.1.4). It is not a provider
	// attribute, so it lives on the resolution rather than on a Profile.
	lateChunking bool
	// contextual is the EFFECTIVE contextual-retrieval component of the embed
	// identity (SPEC 8.1.4/8.1.8, issue #330), stamped by Config.Providers from
	// the resolved ContextualBinding: provider.EmbedContextualOff when the
	// feature is off OR fell open for want of a chat provider, else the
	// "ctx:<hash>" generator token. Like lateChunking it is not a provider
	// attribute, so it lives on the resolution.
	contextual string
}

// providersResolution builds the resolution from the parsed doc + env.
func (d providersDoc) resolve(base map[string]providerProfileYAML, getenv func(string) string) ProviderResolution {
	merged, order := mergeProfiles(base, d.Providers, d.declOrder)
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
	var (
		prof provider.Profile
		err  error
	)
	if sel == "auto" {
		prof, err = r.Resolve(provider.CapSTT)
	} else if profileName, ok := STTSelectorProfile(sel); ok {
		prof, err = r.ResolveExplicit(provider.CapSTT, profileName, true)
	} else {
		return provider.Profile{}, false
	}
	if err != nil {
		return provider.Profile{}, false
	}
	return prof, true
}

// RouteSTTProfile applies media.stt.language_providers language-based routing
// (SPEC §8.2.1, #566) to an already-resolved DEFAULT STT profile. Routing is
// PIN-BASED and per-run: langdetect is text-based and cannot know an audio file's
// language before transcription, so the route keys off the profile's configured
// source-language pin (def.STTLanguage), resolved once — never per-item
// auto-detection. When that pin matches a language_providers entry (BCP-47
// primary-subtag match), the mapped STT-capable profile is re-resolved (required)
// and REPLACES the default, carrying the source-language pin onto it so the routed
// backend still transcribes the right language AND the honest-coverage check (§8.2.1
// Slice A) runs against the ROUTED profile's declared coverage. With no pin, no
// route table, or no matching route, def is returned unchanged (single-provider
// behaviour). It is the single routing point shared by the ingest transcriber build
// and the expected-language / derivation-identity resolver, so the routed profile
// propagates consistently. A route to an unknown or non-STT-capable profile is
// rejected as CONFIG_INVALID at startup (validateSTTLanguageProviders), so the only
// error here is a routed profile whose credential is unset.
func (c Config) RouteSTTProfile(def provider.Profile) (provider.Profile, error) {
	pin := strings.TrimSpace(def.STTLanguage)
	if pin == "" || len(c.MediaSTTLanguageProviders) == 0 {
		return def, nil
	}
	mapped, ok := c.MediaSTTLanguageProviders[provider.PrimarySubtag(pin)]
	if !ok {
		return def, nil
	}
	mapped = strings.TrimSpace(mapped) // defensive: values are trimmed at parse, but never route to a blank name
	routed, err := c.Providers().ResolveExplicit(provider.CapSTT, mapped, true)
	if err != nil {
		return provider.Profile{}, err
	}
	routed.STTLanguage = pin
	return routed, nil
}

// sttSelectorProfile is the SINGLE mapping from a normalized legacy stt.provider
// selector (SPEC 8.1.3) to the built-in provider profile that serves it. It is
// shared by every STT resolver — ingest transcription
// (ingest.TranscriberFromConfigWithLanguage), the expected-language/derivation
// resolver (ingest.resolveSTTProfile), and config-time diarization/translate
// validation (resolveSTTProfileForCapability) — so an explicit selector resolves
// to the SAME backend on every path (issue #440 F6). Before unification, these
// tables diverged: only mistral/elevenlabs/whisper were mapped, so an explicit
// `openai`/`gemini` selector errored loudly at first transcription while
// silently resolving STT-OFF for expected-language, derivation identity, and
// diarization gating — even though providerfactory.Transcriber builds those
// kinds. The special `auto` selector and the off aliases are handled by callers,
// not this table.
var sttSelectorProfile = map[string]string{
	"mistral":    "mistral-ocr", // Voxtral STT (kind: mistral)
	"elevenlabs": "elevenlabs",
	"whisper":    "whisper",
	"openai":     "openai",
	"gemini":     "gemini",
}

// STTSelectorProfile resolves a normalized (lower-cased, trimmed) legacy
// stt.provider selector to the built-in profile name that serves it. known is
// false for an unrecognized selector; `auto` and the off aliases are NOT in the
// table (callers handle them before consulting it). See sttSelectorProfile.
func STTSelectorProfile(sel string) (profileName string, known bool) {
	name, ok := sttSelectorProfile[sel]
	return name, ok
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

// validateProviderKinds fails fast (CONFIG_INVALID) when any resolved provider
// profile declares an unrecognized `kind:` (issue #440 F7). An unknown/typo
// kind has no row in the capability matrix (SPEC 8.1.2), so the profile is
// silently un-selectable in `auto` and surfaces only as a generic
// "no eligible provider" error far from its cause — a typo the operator cannot
// act on. Validating here names the offending profile and the bad kind at
// startup so the misconfiguration is actionable immediately. Profiles are
// scanned in sorted order so the reported error is stable across runs (Go map
// iteration is non-deterministic).
func (c *Config) validateProviderKinds() error {
	byName := c.Providers().ByName()
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := byName[name]
		if !provider.IsKnownKind(p.Kind) {
			return fmt.Errorf(
				"CONFIG_INVALID: provider profile %q declares unrecognized kind %q; use one of %s",
				name, p.Kind, provider.KnownKindsString())
		}
	}
	return nil
}

// validateSTTLanguageProviders fails fast (CONFIG_INVALID) when a
// media.stt.language_providers route (SPEC §8.2.1, #566) names a profile that
// does not exist or is not STT-capable (per the capability matrix, SPEC 8.1.2).
// This is static validation: a route to a missing/incapable backend can never
// transcribe, so it must be caught at startup naming the offending language and
// profile rather than surfacing far downstream. Credential eligibility is NOT
// checked here (a credential-less endpoint is valid; a missing api_key is a
// runtime/preflight concern), matching how explicit provider bindings are gated.
// EndpointDependent kinds (e.g. kind:openai audio) are permitted, consistent with
// the matrix. Routes are scanned in sorted language order so the error is stable.
func (c *Config) validateSTTLanguageProviders() error {
	if len(c.MediaSTTLanguageProviders) == 0 {
		return nil
	}
	byName := c.Providers().ByName()
	langs := make([]string, 0, len(c.MediaSTTLanguageProviders))
	for lang := range c.MediaSTTLanguageProviders {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		name := strings.TrimSpace(c.MediaSTTLanguageProviders[lang])
		if name == "" {
			return fmt.Errorf(
				"CONFIG_INVALID: media.stt.language_providers[%q] has no provider profile name", lang)
		}
		prof, ok := byName[name]
		if !ok {
			return fmt.Errorf(
				"CONFIG_INVALID: media.stt.language_providers[%q] names unknown provider profile %q", lang, name)
		}
		if provider.Can(prof.Kind, provider.CapSTT) == provider.Unsupported {
			return fmt.Errorf(
				"CONFIG_INVALID: media.stt.language_providers[%q] provider %q (kind %q) is not speech-to-text capable; route to an STT-capable profile or remove the entry",
				lang, name, prof.Kind)
		}
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
	return provider.EmbedIdentity(p, r.lateChunking, r.contextual)
}

// EmbedContextual is the terminal `contextual` component of the corpus-lifetime
// embed identity (SPEC 8.1.4/8.1.8), recorded separately in the config snapshot
// as `embed_contextual` (§5.5) so an operator can read the effective
// contextualization mode without parsing the composite identity. It is
// provider.EmbedContextualOff unless contextual retrieval is EFFECTIVELY on.
func (r ProviderResolution) EmbedContextual() string {
	return provider.NormalizeEmbedContextual(r.contextual)
}

// EmbedBaseURL is the normalized embed base_url component of the corpus-lifetime
// identity (SPEC 8.1.4 / §6.4 `embed_base_url`), or "" if embed cannot be
// resolved OR the endpoint is canonical/default (rule 2) — which includes the
// HOSTED native gemini/cohere surface, but no longer a CUSTOM one (issue #702).
// It is persisted alongside the recorded identity so operators can read the
// endpoint that pins the vector space without parsing the composite identity.
func (r ProviderResolution) EmbedBaseURL() string {
	p, err := r.Resolve(provider.CapEmbed)
	if err != nil {
		return ""
	}
	return provider.NormalizeEmbedBaseURL(p)
}

// Providers returns the provider resolution for cfg using os.Getenv for
// credential expansion (SPEC §8.1). The CLI wiring (C2-iii) calls
// Resolve per capability and builds the adapter via providerfactory.
func (cfg Config) Providers() ProviderResolution {
	r := cfg.baseProviders()
	// Fold the EFFECTIVE contextual-retrieval mode onto the resolution so the
	// embed identity captures it (SPEC 8.1.4/8.1.8, issue #330). Resolved from
	// the binding (not the raw config flag) so a fail-open corpus records `off`.
	r.contextual = cfg.ContextualBinding().Identity
	return r
}

// baseProviders builds the provider resolution WITHOUT the contextual-retrieval
// identity component. ContextualBinding itself has to resolve the chat
// capability, so it uses this rather than Providers() — otherwise the two would
// recurse into each other. The component is irrelevant to capability resolution
// (it only enters EmbedIdentity), so the distinction is invisible to callers.
func (cfg Config) baseProviders() ProviderResolution {
	base := builtinProfiles()
	seedLegacy(base, cfg)
	r := cfg.providersDoc.resolve(base, nil)
	// Fold the resolved ingest.late_chunking flag onto the resolution so the
	// embed identity captures the late-chunking mode (issue #332/#446).
	r.lateChunking = cfg.IngestLateChunking
	return r
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
	merged, _ := mergeProfiles(base, cfg.providersDoc.Providers, cfg.providersDoc.declOrder)

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
