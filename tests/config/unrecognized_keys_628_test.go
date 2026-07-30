package tests

// #628: a config key no setter claims used to be discarded in total silence.
// That is how #624 hurt — `.dir2mcp.yaml` said `recognize_provider: serve` (put
// there by `dir2mcp config init`), the loader did not recognise that spelling,
// and recognition stayed off with no error and no warning. Typos, stale keys
// from older releases and wrong nesting depth were all swallowed the same way.
//
// The loader now records one warning naming the ignored keys. The most important
// property under test is the absence of FALSE POSITIVES: a warning on valid
// config would be worse than the silence it replaces.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

func warningsText(cfg config.Config) string {
	var b strings.Builder
	for _, w := range cfg.Warnings {
		b.WriteString(w.Error())
		b.WriteString("\n")
	}
	return b.String()
}

func TestUnrecognizedKey_Warns(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"recognise_provider: serve\n"+ // misspelling of a real key
		"stt.provdier: off\n") // transposed characters

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile must not fail on unknown keys (warn, don't break deployments): %v", err)
	}
	got := warningsText(cfg)
	for _, want := range []string{"recognise_provider", "stt.provdier", "unrecognized"} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings %q missing %q", got, want)
		}
	}
	// The valid key on the same file must still be applied.
	if cfg.RootDir != "./repo" {
		t.Errorf("RootDir=%q; a bad neighbouring key must not discard a good one", cfg.RootDir)
	}
}

// TestGeneratedConfig_ProducesNoWarnings is the false-positive guard that matters
// most: everything `config init` writes, plus the separately-parsed
// providers:/model:/carbon:/cost: blocks, must load silently. The flat parser
// still walks those blocks' lines, so without the side-parsed exemption every
// valid config would warn about them.
func TestGeneratedConfig_ProducesNoWarnings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dir2mcp.yaml")
	if err := config.SaveFile(path, config.Default()); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}
	enriched := string(generated) + "" +
		"providers:\n" +
		"  ollama:\n" +
		"    kind: openai\n" +
		"    base_url: http://localhost:11434/v1\n" +
		"    embed_text_model: bge-m3:latest\n" +
		"    chat_model: qwen2.5:7b-instruct\n" +
		"model:\n" +
		"  embed:\n" +
		"    provider: ollama\n" +
		"  chat:\n" +
		"    provider: ollama\n" +
		"carbon:\n" +
		"  enabled: true\n" +
		"cost:\n" +
		"  prices:\n" +
		"    mistral-embed:\n" +
		"      input_per_1k: 0.01\n" +
		"      output_per_1k: 0.02\n"
	if err := os.WriteFile(path, []byte(enriched), 0o600); err != nil {
		t.Fatalf("write enriched: %v", err)
	}

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := warningsText(cfg); got != "" {
		t.Errorf("a valid config must load silently, got warnings:\n%s", got)
	}
	// Sanity: the enriched blocks really were applied, so the silence above is
	// not the silence of a config that failed to parse at all.
	if _, err := cfg.Providers().Resolve(provider.CapEmbed); err != nil {
		t.Errorf("providers/model block did not load: %v", err)
	}
}

// TestRecognizeUnderscoreKeys_ProduceNoWarnings pins the #624 keys specifically:
// they are legitimate now, so they must not be reported as unrecognized.
func TestRecognizeUnderscoreKeys_ProduceNoWarnings(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"recognize_provider: serve\n"+
		"recognize_serve_url: http://127.0.0.1:8765\n"+
		"recognize_serve_command: dirstral-annotate serve\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := warningsText(cfg); got != "" {
		t.Errorf("the #624 recognize keys are valid and must not warn, got:\n%s", got)
	}
}
