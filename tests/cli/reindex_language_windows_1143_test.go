package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// --redecode-language-windows arms the SPEC §8.6.13 reconcile pass (spec
// 0.88.0, #1143): transcripts stored before the low-confidence rule are
// checked, and only their windows in another language are decoded again.

func TestReindexLanguageWindows_RejectsEmbeddingsOnly(t *testing.T) {
	var stderr strings.Builder
	app := cli.NewAppWithIO(&strings.Builder{}, &stderr)
	if code := app.Run([]string{"reindex", "--embeddings-only", "--redecode-language-windows"}); code == 0 {
		t.Fatal("the combination must be refused, got exit 0")
	}
	if !strings.Contains(stderr.String(), "--redecode-language-windows") || !strings.Contains(stderr.String(), "runs no transcription") {
		t.Errorf("the refusal does not name the flag and why: %q", stderr.String())
	}
}

// runLanguageWindowsReindex runs the flag with the given extra config lines
// on a whisper endpoint that is never called, and returns exit code and
// stderr.
func runLanguageWindowsReindex(t *testing.T, extraCfg string) (int, string) {
	t.Helper()
	return runLanguageWindowsReindexWith(t, "stt:\n  provider: \"whisper\"\nproviders:\n  whisper:\n    kind: whisper\n    base_url: \"http://127.0.0.1:9/v1\"\n"+extraCfg)
}

// runLanguageWindowsReindexWith runs the flag with the whole config body.
func runLanguageWindowsReindexWith(t *testing.T, cfgBody string) (int, string) {
	t.Helper()
	dir := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(dir, ".dir2mcp.yaml"), []byte(cfgBody), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr strings.Builder
	var code int
	testutil.WithWorkingDir(t, dir, func() {
		app := cli.NewAppWithIO(&stdout, &stderr)
		code = app.Run([]string{"--non-interactive", "reindex", "--redecode-language-windows"})
	})
	return code, stderr.String()
}

// TestReindexLanguageWindows_RefusesARouteWithoutALanguageHint: an openai
// transcriber cannot take a language hint, so the pass could decode no window
// again. The flag refuses the run and names the provider.
func TestReindexLanguageWindows_RefusesARouteWithoutALanguageHint(t *testing.T) {
	code, stderr := runLanguageWindowsReindexWith(t,
		"stt:\n  provider: \"openai\"\nproviders:\n  openai:\n    kind: openai\n    base_url: \"http://127.0.0.1:9/v1\"\n    api_key: \"test-key\"\n")
	if code == 0 {
		t.Errorf("exit = 0, want a configuration error: a route without a language hint repairs nothing")
	}
	if !strings.Contains(stderr, "cannot take a language hint") || !strings.Contains(stderr, `"openai"`) {
		t.Errorf("the refusal does not name the provider and why: %q", stderr)
	}
}

func TestReindexLanguageWindows_AnnouncesTheCheck(t *testing.T) {
	_, stderr := runLanguageWindowsReindex(t, "")
	if !strings.Contains(stderr, "only windows in another language are decoded again") {
		t.Errorf("the armed run did not say what it checks: %q", stderr)
	}
}

func TestReindexLanguageWindows_RefusesWhenTheRuleIsOff(t *testing.T) {
	for name, cfg := range map[string]string{
		"threshold 0":  "media:\n  stt:\n    redecode_below: 0\n",
		"window scope": "media:\n  stt:\n    language_scope: window\n",
		"pin":          "media:\n  language: uk\n",
		"no detection": "language_detection_enabled: false\n",
	} {
		t.Run(name, func(t *testing.T) {
			code, stderr := runLanguageWindowsReindex(t, cfg)
			if code == 0 {
				t.Errorf("exit = 0, want a configuration error: a run that can repair nothing must not look like a repair")
			}
			if !strings.Contains(stderr, "cannot repair anything") {
				t.Errorf("the refusal does not say why: %q", stderr)
			}
		})
	}
}
