package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestContextual_DefaultsOff pins the SPEC §8.1.8 opt-in contract: contextual
// retrieval is off in the baseline config, so a default corpus embeds raw
// chunks and its embed identity's terminal component stays `off`.
func TestContextual_DefaultsOff(t *testing.T) {
	def := config.Default()
	if def.RetrievalContextualEnabled {
		t.Fatal("retrieval.contextual.enabled must default to false (opt-in)")
	}
	if def.RetrievalContextualMaxTokens != config.DefaultContextualMaxTokens {
		t.Fatalf("max_tokens default = %d, want %d",
			def.RetrievalContextualMaxTokens, config.DefaultContextualMaxTokens)
	}
	if def.RetrievalContextualPromptVersion != config.ContextualPromptVersionV1 {
		t.Fatalf("prompt_version default = %q, want %q",
			def.RetrievalContextualPromptVersion, config.ContextualPromptVersionV1)
	}
	binding := def.ContextualBinding()
	if binding.Active || binding.FellOpen {
		t.Fatalf("disabled config must be inactive and NOT report a fail-open: %+v", binding)
	}
	if binding.Identity != provider.EmbedContextualOff {
		t.Fatalf("disabled identity component = %q, want %q", binding.Identity, provider.EmbedContextualOff)
	}
}

// TestContextual_NestedAndFlatKeys verifies the canonical nested block and the
// flat aliases load the knobs (§16.2), consistent with sibling retrieval keys.
func TestContextual_NestedAndFlatKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{name: "nested", yaml: "retrieval:\n  contextual:\n    enabled: true\n"},
		{name: "flat_alias", yaml: "contextual_enabled: true\n"},
		{name: "flat_prefixed", yaml: "retrieval_contextual_enabled: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
				t.Fatalf("write yaml: %v", err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			if !cfg.RetrievalContextualEnabled {
				t.Fatalf("expected RetrievalContextualEnabled=true for %s yaml", tc.name)
			}
		})
	}
}

// TestContextual_NestedScalarsLoad exercises the remaining block keys.
func TestContextual_NestedScalarsLoad(t *testing.T) {
	cfg := loadCfg(t, strings.Join([]string{
		"retrieval:",
		"  contextual:",
		"    enabled: true",
		"    provider: openai",
		"    model: gpt-4o-mini",
		"    max_tokens: 96",
		"    prompt_version: v1",
		"",
	}, "\n"))
	if cfg.RetrievalContextualProvider != "openai" {
		t.Errorf("provider = %q", cfg.RetrievalContextualProvider)
	}
	if cfg.RetrievalContextualModel != "gpt-4o-mini" {
		t.Errorf("model = %q", cfg.RetrievalContextualModel)
	}
	if cfg.RetrievalContextualMaxTokens != 96 {
		t.Errorf("max_tokens = %d", cfg.RetrievalContextualMaxTokens)
	}
}

// TestContextual_SaveLoadRoundTrip verifies the block survives a
// SaveFile/LoadFile cycle through the persisted YAML.
func TestContextual_SaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.RetrievalContextualEnabled = true
	cfg.RetrievalContextualProvider = "openai"
	cfg.RetrievalContextualModel = "gpt-4o-mini"
	cfg.RetrievalContextualMaxTokens = 64

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !loaded.RetrievalContextualEnabled || loaded.RetrievalContextualProvider != "openai" ||
		loaded.RetrievalContextualModel != "gpt-4o-mini" || loaded.RetrievalContextualMaxTokens != 64 {
		t.Fatalf("contextual block did not round-trip: %+v", loaded)
	}
}

// TestContextual_CapabilityFallbackRecordsOff is the load-bearing §8.1.8/§2a
// case: enabling contextual retrieval with NO chat provider available must fail
// OPEN — the corpus embeds raw and records the effective `off` identity. If it
// recorded an "on" token instead, the corpus would look contextual-compatible
// the moment a chat provider was added, silently mixing raw and contextualized
// vectors in one index.
func TestContextual_CapabilityFallbackRecordsOff(t *testing.T) {
	// No provider credential in the environment ⇒ no chat-capable profile is
	// eligible, which is exactly the fail-open condition.
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")

	cfg := loadCfg(t, "retrieval:\n  contextual:\n    enabled: true\n")
	binding := cfg.ContextualBinding()
	if binding.Active {
		t.Fatal("with no chat provider the binding must NOT be active")
	}
	if !binding.FellOpen {
		t.Fatal("an enabled-but-unresolvable binding must report the fail-open so it can be warned about")
	}
	if binding.Identity != provider.EmbedContextualOff {
		t.Fatalf("fail-open identity = %q, want %q (never an on token for raw vectors)",
			binding.Identity, provider.EmbedContextualOff)
	}
}

// TestContextual_EntersEmbedIdentity pins the reindex binding (SPEC §8.1.4):
// enabling contextual retrieval with a resolvable chat provider changes the
// corpus-lifetime embed identity, and only its terminal field.
func TestContextual_EntersEmbedIdentity(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")

	off := loadCfg(t, "version: 1\n")
	offID := off.Providers().EmbedIdentity()
	if !strings.HasSuffix(offID, "|"+provider.EmbedContextualOff) {
		t.Fatalf("default identity %q must end with the off contextual token", offID)
	}
	if got := off.Providers().EmbedContextual(); got != provider.EmbedContextualOff {
		t.Fatalf("EmbedContextual() = %q, want %q", got, provider.EmbedContextualOff)
	}

	on := loadCfg(t, "retrieval:\n  contextual:\n    enabled: true\n")
	binding := on.ContextualBinding()
	if !binding.Active {
		t.Fatalf("with a chat provider available the binding must be active: %+v", binding)
	}
	onID := on.Providers().EmbedIdentity()
	if onID == offID {
		t.Fatalf("enabling contextual retrieval must change the embed identity: %q", onID)
	}
	if !provider.ContextualActive(on.Providers().EmbedContextual()) {
		t.Fatalf("EmbedContextual() = %q, want a ctx:<hash> token", on.Providers().EmbedContextual())
	}
	// Only the terminal component differs — nothing else drifted.
	if strings.TrimSuffix(onID, "|"+binding.Identity) != strings.TrimSuffix(offID, "|"+provider.EmbedContextualOff) {
		t.Fatalf("only the contextual token may differ: on=%q off=%q", onID, offID)
	}
	// And an existing (pre-feature, 8-field) corpus recording must still match a
	// fresh DISABLED build — the no-spurious-reindex guarantee at config level.
	legacy := strings.TrimSuffix(offID, "|"+provider.EmbedContextualOff)
	if err := provider.VerifyEmbedIdentity(legacy, offID); err != nil {
		t.Fatalf("pre-contextual recorded identity must not reindex: %v", err)
	}
	// But it MUST mismatch a contextualized build (refuse to mix vector spaces).
	if err := provider.VerifyEmbedIdentity(legacy, onID); err == nil {
		t.Fatal("a raw corpus must NOT silently serve under a contextualized identity")
	}
}

// TestContextual_SnapshotRecordsEmbedContextual pins SPEC §5.5: the effective
// contextual component is recorded in the config snapshot as `embed_contextual`,
// and is ABSENT for the `off` default (an absent key is treated as `off`).
func TestContextual_SnapshotRecordsEmbedContextual(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")

	readSnapshot := func(t *testing.T, cfg config.Config) string {
		t.Helper()
		cfg.RootDir = t.TempDir()
		cfg.StateDir = filepath.Join(cfg.RootDir, ".dir2mcp")
		path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
		if err != nil {
			t.Fatalf("SaveEffectiveSnapshot: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		return string(raw)
	}

	if snap := readSnapshot(t, config.Default()); strings.Contains(snap, "embed_contextual:") {
		t.Fatal("a non-contextual corpus must not carry embed_contextual (absent ⇒ off, §5.5)")
	}

	on := config.Default()
	on.RetrievalContextualEnabled = true
	snap := readSnapshot(t, on)
	if !strings.Contains(snap, "embed_contextual: ctx:") {
		t.Fatalf("contextual snapshot must record the ctx:<hash> token, got:\n%s", snap)
	}
}

// TestContextual_ValidationRejectsBadKnobs pins the §16.2 static rules. They
// apply only when the feature is enabled, so a default config is never affected.
func TestContextual_ValidationRejectsBadKnobs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{
			name:    "non-positive max_tokens",
			mutate:  func(c *config.Config) { c.RetrievalContextualMaxTokens = 0 },
			wantErr: "max_tokens must be > 0",
		},
		{
			name:    "unknown prompt_version",
			mutate:  func(c *config.Config) { c.RetrievalContextualPromptVersion = "v99" },
			wantErr: "prompt_version",
		},
		{
			name:    "prompt override missing the chunk placeholder",
			mutate:  func(c *config.Config) { c.RetrievalContextualPrompt = "situate {{DOCUMENT}}" },
			wantErr: config.ContextualChunkPlaceholder,
		},
		{
			name:    "prompt override missing the document placeholder",
			mutate:  func(c *config.Config) { c.RetrievalContextualPrompt = "situate {{CHUNK}}" },
			wantErr: config.ContextualDocumentPlaceholder,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.RootDir = t.TempDir()
			cfg.StateDir = filepath.Join(cfg.RootDir, ".dir2mcp")
			cfg.RetrievalContextualEnabled = true
			tc.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected CONFIG_INVALID for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q must mention %q", err, tc.wantErr)
			}
			// The same value is fine while the feature is OFF: the rules are scoped.
			off := config.Default()
			off.RootDir = cfg.RootDir
			off.StateDir = cfg.StateDir
			tc.mutate(&off)
			if err := off.Validate(); err != nil {
				t.Fatalf("disabled config must not be rejected: %v", err)
			}
		})
	}
}

// TestContextual_PromptOverrideRebindsIdentity pins design 0004 §2: an edited
// operator prompt override re-embeds even without a prompt_version bump, because
// the EFFECTIVE prompt text is hashed into the identity.
func TestContextual_PromptOverrideRebindsIdentity(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")

	builtin := loadCfg(t, "retrieval:\n  contextual:\n    enabled: true\n")
	override := builtin
	override.RetrievalContextualPrompt = "Situate {{CHUNK}} within {{DOCUMENT}}."
	edited := override
	edited.RetrievalContextualPrompt = "Situate {{CHUNK}} within {{DOCUMENT}} briefly."

	ids := map[string]string{
		"builtin":  builtin.ContextualBinding().Identity,
		"override": override.ContextualBinding().Identity,
		"edited":   edited.ContextualBinding().Identity,
	}
	for name, id := range ids {
		if !provider.ContextualActive(id) {
			t.Fatalf("%s identity %q must be an active ctx token", name, id)
		}
	}
	if ids["builtin"] == ids["override"] {
		t.Error("an operator prompt override must change the contextual identity")
	}
	if ids["override"] == ids["edited"] {
		t.Error("EDITING the override must change the contextual identity (no prompt_version bump needed)")
	}
}

// TestContextual_EffectivePromptIsDomainGeneral guards the project-wide rule
// that shipped defaults stay general-purpose: the built-in template carries both
// placeholders and no corpus/domain assumptions.
func TestContextual_EffectivePromptIsDomainGeneral(t *testing.T) {
	cfg := config.Default()
	prompt, ok := cfg.ContextualEffectivePrompt()
	if !ok {
		t.Fatal("the default prompt_version must resolve to a built-in template")
	}
	for _, placeholder := range []string{config.ContextualDocumentPlaceholder, config.ContextualChunkPlaceholder} {
		if !strings.Contains(prompt, placeholder) {
			t.Errorf("built-in template must contain %s", placeholder)
		}
	}
	rendered := config.RenderContextualPrompt(prompt, "DOC BODY", "CHUNK BODY")
	if strings.Contains(rendered, config.ContextualDocumentPlaceholder) ||
		strings.Contains(rendered, config.ContextualChunkPlaceholder) {
		t.Error("rendering must substitute every placeholder")
	}
	if !strings.Contains(rendered, "DOC BODY") || !strings.Contains(rendered, "CHUNK BODY") {
		t.Error("rendering must inject both the document and the chunk")
	}
}
