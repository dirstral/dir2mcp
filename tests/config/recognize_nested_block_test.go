package tests

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestRecognize_NestedBlockLoads pins the nested form of the recognize keys,
// the form that docs/configuration.md and the SPEC §16.2 template show. The
// line parser only prefixes the children of a REGISTERED section, so before
// `recognize` was registered a nested block loaded as nothing: recognition
// stayed off with no error, and an operator who copied the docs never knew.
func TestRecognize_NestedBlockLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	writeFile(t, path, "recognize:\n  provider: serve\n  base_url: http://127.0.0.1:8765\n"+
		"  serve_command: dirstral-annotator serve\n  timeout: 90m\n  timeout_per_media_second: 3.5\n")
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RecognizeProvider != "serve" || cfg.RecognizeServeURL != "http://127.0.0.1:8765" ||
		cfg.RecognizeServeCommand != "dirstral-annotator serve" || cfg.RecognizeTimeout != 90*time.Minute ||
		cfg.RecognizeTimeoutPerMediaSecond != 3.5 {
		t.Fatalf("nested recognize block not loaded: provider=%q base_url=%q command=%q timeout=%s ratio=%v",
			cfg.RecognizeProvider, cfg.RecognizeServeURL, cfg.RecognizeServeCommand,
			cfg.RecognizeTimeout, cfg.RecognizeTimeoutPerMediaSecond)
	}
}
