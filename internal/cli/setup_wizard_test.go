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
	if !strings.Contains(strings.ToLower(cfg.RAGSystemPrompt), "source code") {
		t.Errorf("code profile system prompt missing code grounding: %q", cfg.RAGSystemPrompt)
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
	var more bool
	profile := string(corpusProfileGeneral)

	form := buildSetupForm(keyValues, &more, &profile)
	if form == nil {
		t.Fatal("buildSetupForm returned nil")
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
