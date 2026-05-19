package tests

import (
	"os"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// SPEC 8.1.4: the resolved embed identity is recorded in the snapshot
// and a changed embed provider/model on reload is refused (no silent
// vector-space mixing).
func TestEmbedIdentity_SnapshotRecordsAndVerifies(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "mk")
	dir := t.TempDir()

	// Fresh state dir (no snapshot) always passes.
	cfg := config.Default()
	cfg.StateDir = dir
	if err := cfg.VerifyEmbedIdentity(dir); err != nil {
		t.Fatalf("fresh state dir must pass: %v", err)
	}

	// Save the snapshot; it must record embed_identity.
	if _, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{}); err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(config.EffectiveSnapshotPath(dir))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	want := "embed_identity: mistral|mistral-embed|codestral-embed"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("snapshot missing %q:\n%s", want, raw)
	}

	// Same identity -> passes.
	if err := cfg.VerifyEmbedIdentity(dir); err != nil {
		t.Fatalf("matching identity must pass: %v", err)
	}

	// Changed embed model -> refused with a CONFIG_INVALID-class error.
	// Post clean-break (#38) the embed model name is provider config
	// (model.embed.text_model per SPEC §16.2), not a Config field.
	changed := loadCfg(t, "model:\n  embed:\n    text_model: some-other-embed-v9\n")
	changed.StateDir = dir
	err = changed.VerifyEmbedIdentity(dir)
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("changed embed identity must be refused, got: %v", err)
	}
}
