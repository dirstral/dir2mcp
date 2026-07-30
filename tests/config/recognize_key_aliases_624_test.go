package tests

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// Regression guard for #624: the snapshot writer emits the underscore spellings
// (recognize_provider / recognize_serve_url / recognize_serve_command), and
// `dir2mcp config init` uses that writer to generate the user-facing
// .dir2mcp.yaml — but the file loader dispatches on the DOTTED keys. Without
// legacy-key aliases those generated keys were dropped silently: recognition
// stayed off with no error and no warning, and because the provider never
// reached the runtime config, validateRecognizeProvider never fired on an
// incomplete `serve` binding either.

func TestLoadFile_RecognizeUnderscoreKeysRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	// Exactly the spellings `dir2mcp config init` writes.
	writeFile(t, path, ""+
		"recognize_provider: serve\n"+
		"recognize_serve_url: http://127.0.0.1:8765\n"+
		"recognize_serve_command: dirstral-annotate serve --port 8765\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.RecognizeProvider != "serve" {
		t.Errorf("RecognizeProvider=%q want %q — the generated config's recognize keys must round-trip (#624)",
			cfg.RecognizeProvider, "serve")
	}
	if want := "http://127.0.0.1:8765"; cfg.RecognizeServeURL != want {
		t.Errorf("RecognizeServeURL=%q want %q (recognize_serve_url maps to recognize.base_url)",
			cfg.RecognizeServeURL, want)
	}
	if want := "dirstral-annotate serve --port 8765"; cfg.RecognizeServeCommand != want {
		t.Errorf("RecognizeServeCommand=%q want %q", cfg.RecognizeServeCommand, want)
	}
}

// TestLoadFile_RecognizeDottedKeysStillWork pins that adding the aliases did not
// disturb the canonical dotted spellings.
func TestLoadFile_RecognizeDottedKeysStillWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, ""+
		"recognize.provider: serve\n"+
		"recognize.base_url: http://127.0.0.1:9001\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.RecognizeProvider != "serve" || cfg.RecognizeServeURL != "http://127.0.0.1:9001" {
		t.Errorf("dotted keys regressed: provider=%q url=%q",
			cfg.RecognizeProvider, cfg.RecognizeServeURL)
	}
}

// TestLoadFile_RecognizeUnderscoreServeWithoutURLIsInvalid is the sharpest
// consequence of the bug: with the keys silently dropped, `serve` never reached
// validation, so an incomplete binding started up as if recognition were simply
// off. Now it must fail fast, exactly as the dotted form does.
func TestLoadFile_RecognizeUnderscoreServeWithoutURLIsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, "recognize_provider: serve\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("expected CONFIG_INVALID for serve without a base_url, got nil " +
			"(a silently-ignored recognize_provider hides the misconfiguration)")
	}
	if !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Errorf("error = %v, want a CONFIG_INVALID", err)
	}
}
