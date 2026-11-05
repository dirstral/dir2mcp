package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// Issue #1118: a YAML example in the docs must load through the real config
// validation. The self-hosted example set stt_provider to a profile name
// (`local-stt`), which validateSTTProvider rejects, so a reader who copied it
// got CONFIG_INVALID. These tests read every ```yaml block in docs/*.md and
// README.md and load it with config.LoadFile + Validate, so a docs example
// that the code rejects breaks CI.

// docsYAMLExample is one fenced ```yaml block from a markdown file.
type docsYAMLExample struct {
	file string // path relative to the repo root
	line int    // 1-based line of the first line inside the fence
	body string // block text, with the fence indentation removed
}

// foreignYAMLExamples are YAML blocks in the docs that are not dir2mcp
// configuration (a third-party tool's config). The key is the file and the
// first non-empty line of the block. Each entry must still match a block, so
// the list cannot go stale.
var foreignYAMLExamples = map[string]string{
	"docs/deployment.md|ingress:":               "cloudflared ingress rules",
	"docs/deployment.md|# ngrok traffic policy": "ngrok traffic policy",
}

func docsYAMLExamples(t *testing.T) []docsYAMLExample {
	t.Helper()
	root := filepath.Join("..", "..")
	files, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}
	files = append(files, filepath.Join(root, "README.md"))

	var out []docsYAMLExample
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("rel %s: %v", path, err)
		}
		rel = filepath.ToSlash(rel)

		inBlock := false
		indent := ""
		start := 0
		var body []string
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimLeft(line, " ")
			switch {
			case !inBlock && strings.HasPrefix(trimmed, "```yaml"):
				inBlock = true
				indent = line[:len(line)-len(trimmed)]
				start = i + 2
				body = nil
			case inBlock && strings.HasPrefix(trimmed, "```"):
				inBlock = false
				out = append(out, docsYAMLExample{file: rel, line: start, body: strings.Join(body, "\n") + "\n"})
			case inBlock:
				body = append(body, strings.TrimPrefix(line, indent))
			}
		}
		if inBlock {
			t.Fatalf("%s:%d: unclosed ```yaml block", rel, start)
		}
	}
	return out
}

// firstContentLine returns the first non-empty line of a block, trimmed.
func firstContentLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

func loadDocsYAML(t *testing.T, body string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, body)
	cfg, err := config.LoadFile(path)
	if err != nil {
		return err
	}
	return cfg.Validate()
}

func TestDocsYAMLExamples_PassConfigValidation(t *testing.T) {
	examples := docsYAMLExamples(t)
	// Assert a positive signal: an extractor that finds nothing must not pass.
	if len(examples) < 20 {
		t.Fatalf("found %d ```yaml blocks in docs, want at least 20; the extractor is broken", len(examples))
	}

	matchedForeign := map[string]bool{}
	checked := 0
	for _, ex := range examples {
		key := ex.file + "|" + firstContentLine(ex.body)
		if _, ok := foreignYAMLExamples[key]; ok {
			matchedForeign[key] = true
			continue
		}
		checked++
		if err := loadDocsYAML(t, ex.body); err != nil {
			t.Errorf("%s:%d: docs YAML example fails config validation: %v", ex.file, ex.line, err)
		}
	}
	for key, why := range foreignYAMLExamples {
		if !matchedForeign[key] {
			t.Errorf("foreign YAML example %q (%s) no longer matches a docs block; remove it from the list", key, why)
		}
	}
	if checked == 0 {
		t.Fatal("no docs YAML example was checked")
	}
}

// TestDocsYAMLExamples_STTSelectorExamplesLoadVerbatim loads every docs
// example that sets stt_provider exactly as written, comments included,
// because those are the examples a reader copies to set up self-hosted STT.
func TestDocsYAMLExamples_STTSelectorExamplesLoadVerbatim(t *testing.T) {
	checked := 0
	for _, ex := range docsYAMLExamples(t) {
		if !strings.Contains(ex.body, "stt_provider:") {
			continue
		}
		checked++
		if err := loadDocsYAML(t, ex.body); err != nil {
			t.Errorf("%s:%d: docs example that sets stt_provider fails config validation as written: %v", ex.file, ex.line, err)
		}
	}
	// The self-hosted examples in docs/configuration.md and
	// docs/dual-machine-deployment.md set stt_provider; finding none means the
	// test lost its subject.
	if checked < 3 {
		t.Fatalf("found %d docs YAML examples that set stt_provider, want at least 3", checked)
	}
}

// TestDocsYAMLExamples_ProfileNameAsSTTSelectorIsRejected pins the reason for
// the #1118 docs fix: stt_provider takes a built-in selector name, not a
// profile name, so a docs example must override the built-in `whisper`
// profile instead of naming its own profile.
func TestDocsYAMLExamples_ProfileNameAsSTTSelectorIsRejected(t *testing.T) {
	const profiles = "providers:\n" +
		"  local-stt:\n" +
		"    kind: whisper\n" +
		"    base_url: http://127.0.0.1:9001\n" +
		"    stt_model: large-v3\n"
	err := loadDocsYAML(t, profiles+"stt_provider: local-stt\n")
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "local-stt") {
		t.Fatalf("err = %v, want CONFIG_INVALID naming the profile name local-stt", err)
	}

	const override = "providers:\n" +
		"  whisper:\n" +
		"    kind: whisper\n" +
		"    base_url: http://127.0.0.1:9001\n" +
		"    stt_model: large-v3\n" +
		"stt_provider: whisper\n"
	if err := loadDocsYAML(t, override); err != nil {
		t.Fatalf("overriding the built-in whisper profile: %v", err)
	}
	// The override must reach the profile the selector maps to, or the
	// example would validate yet still point STT at ${WHISPER_BASE_URL}.
	cfg := loadCfg(t, override)
	prof := cfg.Providers().ByName()["whisper"]
	if prof.BaseURL != "http://127.0.0.1:9001" || prof.STTModel != "large-v3" {
		t.Fatalf("whisper profile = base_url %q stt_model %q, want the override values", prof.BaseURL, prof.STTModel)
	}
}
