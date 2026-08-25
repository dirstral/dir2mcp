package setupwizard_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/setupwizard"
)

func TestApplyCorpusProfile_Legal(t *testing.T) {
	cfg := config.Default()
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileLegal)
	if cfg.RAGKDefault != 12 {
		t.Errorf("RAGKDefault=%d want 12", cfg.RAGKDefault)
	}
	if cfg.RAGMaxContextChars != 40000 {
		t.Errorf("RAGMaxContextChars=%d want 40000", cfg.RAGMaxContextChars)
	}
	if !strings.Contains(strings.ToLower(cfg.RAGSystemPrompt), "legal documents") {
		t.Errorf("legal profile system prompt missing grounding: %q", cfg.RAGSystemPrompt)
	}
}

func TestApplyCorpusProfile_Code(t *testing.T) {
	cfg := config.Default()
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileCode)
	if cfg.RAGKDefault != 8 {
		t.Errorf("RAGKDefault=%d want 8", cfg.RAGKDefault)
	}
	// Code sets RAGMaxContextChars explicitly to the default (self-contained).
	if cfg.RAGMaxContextChars != config.Default().RAGMaxContextChars {
		t.Errorf("RAGMaxContextChars=%d want default %d", cfg.RAGMaxContextChars, config.Default().RAGMaxContextChars)
	}
	if !strings.Contains(strings.ToLower(cfg.RAGSystemPrompt), "source code") {
		t.Errorf("code profile system prompt missing code grounding: %q", cfg.RAGSystemPrompt)
	}
}

func TestApplyCorpusProfile_GeneralNoChange(t *testing.T) {
	base := config.Default()
	cfg := base
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileGeneral)
	if cfg.RAGKDefault != base.RAGKDefault ||
		cfg.RAGMaxContextChars != base.RAGMaxContextChars ||
		cfg.RAGSystemPrompt != base.RAGSystemPrompt {
		t.Errorf("general profile must not alter retrieval defaults: got k=%d ctx=%d prompt=%q",
			cfg.RAGKDefault, cfg.RAGMaxContextChars, cfg.RAGSystemPrompt)
	}
}

func TestApplyCorpusProfile_KeepLeavesConfigUntouched(t *testing.T) {
	cfg := config.Default()
	cfg.RAGKDefault = 99
	cfg.RAGMaxContextChars = 12345
	cfg.RAGSystemPrompt = "custom"
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileKeep)
	if cfg.RAGKDefault != 99 || cfg.RAGMaxContextChars != 12345 || cfg.RAGSystemPrompt != "custom" {
		t.Errorf("keep must not change retrieval settings: k=%d ctx=%d prompt=%q",
			cfg.RAGKDefault, cfg.RAGMaxContextChars, cfg.RAGSystemPrompt)
	}
}

// Profiles must be self-contained: switching from legal to code must not leave
// legal's wider context window (40000) behind.
func TestApplyCorpusProfile_SwitchingDoesNotInheritStaleValues(t *testing.T) {
	cfg := config.Default()
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileLegal)
	if cfg.RAGMaxContextChars != 40000 {
		t.Fatalf("precondition: legal RAGMaxContextChars=%d want 40000", cfg.RAGMaxContextChars)
	}
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileCode)
	if cfg.RAGMaxContextChars != config.Default().RAGMaxContextChars {
		t.Errorf("code after legal inherited stale RAGMaxContextChars=%d; want default %d",
			cfg.RAGMaxContextChars, config.Default().RAGMaxContextChars)
	}
	if cfg.RAGKDefault != 8 {
		t.Errorf("code after legal RAGKDefault=%d want 8", cfg.RAGKDefault)
	}
}

func TestApplyCorpusProfile_GeneralResetsAfterLegal(t *testing.T) {
	def := config.Default()
	cfg := config.Default()
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileLegal)
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileGeneral)
	if cfg.RAGKDefault != def.RAGKDefault ||
		cfg.RAGMaxContextChars != def.RAGMaxContextChars ||
		cfg.RAGSystemPrompt != def.RAGSystemPrompt {
		t.Errorf("general after legal did not reset to defaults: k=%d ctx=%d prompt=%q",
			cfg.RAGKDefault, cfg.RAGMaxContextChars, cfg.RAGSystemPrompt)
	}
}

func TestPersistKeys_WritesNonEmptyInOrder(t *testing.T) {
	type write struct{ key, val string }
	var writes []write
	writer := func(_ /*path*/ string, key, val string) error {
		writes = append(writes, write{key, val})
		return nil
	}
	keys := map[string]string{
		"MISTRAL_API_KEY": "  sk-mistral  ", // trimmed
		"COHERE_API_KEY":  "co-rerank",
		"OPENAI_API_KEY":  "   ", // whitespace-only skipped
	}

	saved, err := setupwizard.PersistKeys(".env.local", keys, writer)
	if err != nil {
		t.Fatalf("PersistKeys: %v", err)
	}
	if strings.Join(saved, ",") != "MISTRAL_API_KEY,COHERE_API_KEY" {
		t.Fatalf("saved=%v want [MISTRAL_API_KEY COHERE_API_KEY]", saved)
	}
	if len(writes) != 2 {
		t.Fatalf("expected 2 writes, got %d: %+v", len(writes), writes)
	}
	if writes[0] != (write{"MISTRAL_API_KEY", "sk-mistral"}) {
		t.Errorf("first write=%+v want trimmed Mistral", writes[0])
	}
	if writes[1] != (write{"COHERE_API_KEY", "co-rerank"}) {
		t.Errorf("second write=%+v want Cohere", writes[1])
	}
}

func TestDotenvHasKey(t *testing.T) {
	content := "export MISTRAL_API_KEY=abc\nCOHERE_API_KEY=\n# OPENAI_API_KEY=x\nGEMINI_API_KEY=g\nANTHROPIC_API_KEY=\"\"\nELEVENLABS_API_KEY=''\n"
	cases := map[string]bool{
		"MISTRAL_API_KEY":    true,  // export prefix, non-empty
		"COHERE_API_KEY":     false, // present but empty
		"OPENAI_API_KEY":     false, // commented out
		"GEMINI_API_KEY":     true,
		"ANTHROPIC_API_KEY":  false, // double-quoted empty
		"ELEVENLABS_API_KEY": false, // single-quoted empty
	}
	for key, want := range cases {
		if got := setupwizard.DotenvHasKey(content, key); got != want {
			t.Errorf("DotenvHasKey(%q)=%t want %t", key, got, want)
		}
	}
}

func TestDetectExistingKeys_FromEnvAndDotenv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	if err := os.WriteFile(envPath, []byte("COHERE_API_KEY=co\n"), 0o600); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}
	t.Setenv("MISTRAL_API_KEY", "from-env")
	t.Setenv("OPENAI_API_KEY", "")

	existing := setupwizard.DetectExistingKeys(envPath)
	if !existing["MISTRAL_API_KEY"] {
		t.Error("Mistral key from environment not detected")
	}
	if !existing["COHERE_API_KEY"] {
		t.Error("Cohere key from .env.local not detected")
	}
	if existing["OPENAI_API_KEY"] {
		t.Error("empty OpenAI env var must not count as set")
	}
}

func TestEnsureGitignoreEntries(t *testing.T) {
	dir := t.TempDir()
	gi := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gi, []byte(".DS_Store\n.env.local\n"), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := setupwizard.EnsureGitignoreEntries(dir, ".env.local", ".dir2mcp/"); err != nil {
		t.Fatalf("EnsureGitignoreEntries: %v", err)
	}
	raw, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	text := string(raw)
	if c := strings.Count(text, ".env.local"); c != 1 {
		t.Errorf("expected .env.local exactly once (no dup), got %d:\n%s", c, text)
	}
	if !strings.Contains(text, ".dir2mcp/") {
		t.Errorf("expected .dir2mcp/ appended:\n%s", text)
	}
	if !strings.Contains(text, ".DS_Store") {
		t.Errorf("existing content must be preserved:\n%s", text)
	}

	before := text
	if err := setupwizard.EnsureGitignoreEntries(dir, ".env.local", ".dir2mcp/"); err != nil {
		t.Fatalf("second EnsureGitignoreEntries: %v", err)
	}
	raw2, _ := os.ReadFile(gi)
	if string(raw2) != before {
		t.Errorf("second run changed file:\nbefore=%q\nafter=%q", before, raw2)
	}
}

func TestBuildForm_ConstructsWithoutPanic(t *testing.T) {
	keyValues := make(map[string]*string, len(setupwizard.ProviderKeys))
	for _, spec := range setupwizard.ProviderKeys {
		keyValues[spec.EnvVar] = new(string)
	}
	var more, save bool
	profile := string(setupwizard.ProfileGeneral)
	dest := string(setupwizard.DestFile)

	for _, existed := range []bool{false, true} {
		form := setupwizard.BuildForm(keyValues, &more, &profile, &dest, &save, setupwizard.Input{
			ExistingKeys:  map[string]bool{"MISTRAL_API_KEY": existed},
			ConfigExisted: existed,
		})
		if form == nil {
			t.Fatalf("BuildForm returned nil (configExisted=%t)", existed)
		}
	}
}

func TestSecretDestConstants(t *testing.T) {
	if setupwizard.DestFile == setupwizard.DestKeychain {
		t.Fatal("DestFile and DestKeychain must be distinct")
	}
	if setupwizard.DestFile != "file" || setupwizard.DestKeychain != "keychain" {
		t.Fatalf("unexpected dest values: file=%q keychain=%q", setupwizard.DestFile, setupwizard.DestKeychain)
	}
}

// TestApplyCorpusProfile_PresetsStateTheCitationContract is the #889
// regression. The server builds the ask response's machine-readable citations
// by parsing inline [rel_path] tags out of the answer
// (citationsReferencedByAnswer in internal/retrieval); an answer that cites
// only in prose yields citations: []. Both domain presets replaced the shipped
// prompt and asked for prose citations only, so choosing legal or code
// silently disabled citations.
//
// The sentence is pinned in the SHIPPED WORDING, not just any mention of the
// tag: identical wording is what keeps the presets and defaultRAGDomainRules
// from drifting apart in meaning.
func TestApplyCorpusProfile_PresetsStateTheCitationContract(t *testing.T) {
	const citationRule = "Include concise source attributions in the form [rel_path]."
	for _, profile := range []setupwizard.Profile{setupwizard.ProfileLegal, setupwizard.ProfileCode} {
		cfg := config.Default()
		setupwizard.ApplyCorpusProfile(&cfg, profile)
		if !strings.Contains(cfg.RAGSystemPrompt, citationRule) {
			t.Errorf("%s preset does not state the citation contract %q; the ask response would carry citations: []\nprompt:\n%s",
				profile, citationRule, cfg.RAGSystemPrompt)
		}
	}
	// The general profile inherits the shipped prompt (empty override), so the
	// server default applies and already carries the rule; pin that the wizard
	// did not start overriding it.
	cfg := config.Default()
	setupwizard.ApplyCorpusProfile(&cfg, setupwizard.ProfileGeneral)
	if cfg.RAGSystemPrompt != config.Default().RAGSystemPrompt {
		t.Errorf("general profile overrides the system prompt; it must inherit the shipped default")
	}
}
