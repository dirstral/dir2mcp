package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// TestUpRejectsInvalidChunkingConfig pins the config-validation half of #405 on
// the `up` command surface: an invalid chunking window (overlap_tokens >=
// max_tokens never advances the chunker) must stop startup with a config-invalid
// exit, not be silently accepted. `up` now re-runs cfg.Validate() AFTER the CLI
// flag overlay so a flag-supplied value cannot slip past validation either; this
// exercises the same validation gate end-to-end through the command.
func TestUpRejectsInvalidChunkingConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	cfgPath := filepath.Join(tmp, ".dir2mcp.yaml")
	if err := os.WriteFile(cfgPath, []byte(strings.Join([]string{
		"root_dir: .",
		"state_dir: .dir2mcp",
		"chunking_max_tokens: 200",
		"chunking_overlap_tokens: 200", // overlap == max: rejected by Validate (#405)
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		code := app.RunWithContext(ctx, []string{"--non-interactive", "up", "--read-only"})
		if code != 2 {
			t.Fatalf("expected exit 2 for invalid chunking config, got %d stderr=%s", code, stderr.String())
		}
	})

	if !strings.Contains(stderr.String(), "chunking.overlap_tokens") {
		t.Fatalf("expected chunking.overlap_tokens validation error, got: %s", stderr.String())
	}
}
