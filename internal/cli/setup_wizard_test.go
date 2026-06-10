package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

func TestApplyCorpusProfile_Legal(t *testing.T) {
	cfg := config.Default()
	applyCorpusProfile(&cfg, corpusProfileLegal)
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
	applyCorpusProfile(&cfg, corpusProfileCode)
	if cfg.RAGKDefault != 8 {
		t.Errorf("RAGKDefault=%d want 8", cfg.RAGKDefault)
	}
	// Code does not override the context window; it must equal the default,
	// not whatever a previously applied profile might have left behind.
	if cfg.RAGMaxContextChars != config.Default().RAGMaxContextChars {
		t.Errorf("RAGMaxContextChars=%d want default %d", cfg.RAGMaxContextChars, config.Default().RAGMaxContextChars)
	}
	if !strings.Contains(strings.ToLower(cfg.RAGSystemPrompt), "source code") {
		t.Errorf("code profile system prompt missing code grounding: %q", cfg.RAGSystemPrompt)
	}
}

// Profiles must be self-contained: switching from legal to code must not leave
// legal's wider context window (40000) behind.
func TestApplyCorpusProfile_SwitchingDoesNotInheritStaleValues(t *testing.T) {
	cfg := config.Default()
	applyCorpusProfile(&cfg, corpusProfileLegal)
	if cfg.RAGMaxContextChars != 40000 {
		t.Fatalf("precondition: legal RAGMaxContextChars=%d want 40000", cfg.RAGMaxContextChars)
	}
	applyCorpusProfile(&cfg, corpusProfileCode)
	if cfg.RAGMaxContextChars != config.Default().RAGMaxContextChars {
		t.Errorf("code after legal inherited stale RAGMaxContextChars=%d; want default %d",
			cfg.RAGMaxContextChars, config.Default().RAGMaxContextChars)
	}
	if cfg.RAGKDefault != 8 {
		t.Errorf("code after legal RAGKDefault=%d want 8", cfg.RAGKDefault)
	}
	if !strings.Contains(strings.ToLower(cfg.RAGSystemPrompt), "source code") {
		t.Errorf("code after legal system prompt not switched: %q", cfg.RAGSystemPrompt)
	}
}

// general resets the managed fields back to defaults even after another profile
// was applied, so it is a true baseline rather than "keep whatever".
func TestApplyCorpusProfile_GeneralResetsAfterLegal(t *testing.T) {
	def := config.Default()
	cfg := config.Default()
	applyCorpusProfile(&cfg, corpusProfileLegal)
	applyCorpusProfile(&cfg, corpusProfileGeneral)
	if cfg.RAGKDefault != def.RAGKDefault ||
		cfg.RAGMaxContextChars != def.RAGMaxContextChars ||
		cfg.RAGSystemPrompt != def.RAGSystemPrompt {
		t.Errorf("general after legal did not reset to defaults: k=%d ctx=%d prompt=%q",
			cfg.RAGKDefault, cfg.RAGMaxContextChars, cfg.RAGSystemPrompt)
	}
}

func TestApplyCorpusProfile_GeneralNoChange(t *testing.T) {
	base := config.Default()
	cfg := base
	applyCorpusProfile(&cfg, corpusProfileGeneral)
	if cfg.RAGKDefault != base.RAGKDefault ||
		cfg.RAGMaxContextChars != base.RAGMaxContextChars ||
		cfg.RAGSystemPrompt != base.RAGSystemPrompt {
		t.Errorf("general profile must not alter retrieval defaults: got k=%d ctx=%d prompt=%q",
			cfg.RAGKDefault, cfg.RAGMaxContextChars, cfg.RAGSystemPrompt)
	}
}

func TestPersistWizardKeys_WritesNonEmptyInOrder(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")
	keys := map[string]string{
		"MISTRAL_API_KEY": "  sk-mistral  ", // surrounding whitespace must be trimmed
		"COHERE_API_KEY":  "co-rerank",
		"OPENAI_API_KEY":  "   ", // whitespace-only must be skipped
	}

	saved, err := persistWizardKeys(envPath, keys)
	if err != nil {
		t.Fatalf("persistWizardKeys: %v", err)
	}

	// Deterministic order follows wizardProviderKeys: Mistral before Cohere.
	want := []string{"MISTRAL_API_KEY", "COHERE_API_KEY"}
	if strings.Join(saved, ",") != strings.Join(want, ",") {
		t.Fatalf("saved=%v want %v", saved, want)
	}

	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read .env.local: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "MISTRAL_API_KEY=sk-mistral") {
		t.Errorf(".env.local missing trimmed Mistral key:\n%s", text)
	}
	if !strings.Contains(text, "COHERE_API_KEY=co-rerank") {
		t.Errorf(".env.local missing Cohere key:\n%s", text)
	}
	if strings.Contains(text, "OPENAI_API_KEY") {
		t.Errorf("whitespace-only key must not be written:\n%s", text)
	}

	// 0600 perms — the file holds secrets.
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat .env.local: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf(".env.local perm=%o want 0600", perm)
	}
}

func TestPersistWizardKeys_OverwritesWithoutDuplicates(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env.local")

	if _, err := persistWizardKeys(envPath, map[string]string{"MISTRAL_API_KEY": "old"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := persistWizardKeys(envPath, map[string]string{"MISTRAL_API_KEY": "new"}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read .env.local: %v", err)
	}
	if n := strings.Count(string(raw), "MISTRAL_API_KEY="); n != 1 {
		t.Fatalf("expected single MISTRAL_API_KEY line, got %d:\n%s", n, raw)
	}
	if !strings.Contains(string(raw), "MISTRAL_API_KEY=new") {
		t.Errorf("expected overwritten value, got:\n%s", raw)
	}
}

func TestBuildSetupForm_ConstructsWithoutPanic(t *testing.T) {
	keyValues := make(map[string]*string, len(wizardProviderKeys))
	for _, spec := range wizardProviderKeys {
		keyValues[spec.EnvVar] = new(string)
	}
	var more, save bool
	profile := string(corpusProfileGeneral)

	// Both the fresh-config and existing-config (keep option) shapes must build.
	for _, existed := range []bool{false, true} {
		form := buildSetupForm(keyValues, &more, &profile, &save, wizardInput{
			ExistingKeys:  map[string]bool{"MISTRAL_API_KEY": existed},
			ConfigExisted: existed,
		})
		if form == nil {
			t.Fatalf("buildSetupForm returned nil (configExisted=%t)", existed)
		}
	}
}

func TestApplyCorpusProfile_KeepLeavesConfigUntouched(t *testing.T) {
	cfg := config.Default()
	cfg.RAGKDefault = 99
	cfg.RAGMaxContextChars = 12345
	cfg.RAGSystemPrompt = "custom"
	applyCorpusProfile(&cfg, corpusProfileKeep)
	if cfg.RAGKDefault != 99 || cfg.RAGMaxContextChars != 12345 || cfg.RAGSystemPrompt != "custom" {
		t.Errorf("keep must not change retrieval settings: k=%d ctx=%d prompt=%q",
			cfg.RAGKDefault, cfg.RAGMaxContextChars, cfg.RAGSystemPrompt)
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
		if got := dotenvHasKey(content, key); got != want {
			t.Errorf("dotenvHasKey(%q)=%t want %t", key, got, want)
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

	existing := detectExistingKeys(envPath)
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
	// Pre-existing file with one of the entries already present.
	gi := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gi, []byte(".DS_Store\n.env.local\n"), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := ensureGitignoreEntries(dir, ".env.local", ".dir2mcp/"); err != nil {
		t.Fatalf("ensureGitignoreEntries: %v", err)
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

	// Idempotent: a second run adds nothing.
	before := text
	if err := ensureGitignoreEntries(dir, ".env.local", ".dir2mcp/"); err != nil {
		t.Fatalf("second ensureGitignoreEntries: %v", err)
	}
	raw2, _ := os.ReadFile(gi)
	if string(raw2) != before {
		t.Errorf("second run changed file:\nbefore=%q\nafter=%q", before, raw2)
	}
}

func TestContainsString(t *testing.T) {
	list := []string{"MISTRAL_API_KEY", "COHERE_API_KEY"}
	if !containsString(list, "COHERE_API_KEY") {
		t.Error("expected COHERE_API_KEY to be found")
	}
	if containsString(list, "OPENAI_API_KEY") {
		t.Error("did not expect OPENAI_API_KEY to be found")
	}
	if containsString(nil, "x") {
		t.Error("nil list must not contain anything")
	}
}
