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
	// The default embed provider is the `mistral` built-in (kind openai, base
	// https://api.mistral.ai/v1) — its base_url is canonical, so the 2nd
	// identity field (base_url, SPEC 8.1.4 / issue #560) normalizes to "" and
	// an existing hosted-default corpus does NOT spuriously reindex.
	want := "embed_identity: mistral||mistral-embed|codestral-embed"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("snapshot missing %q:\n%s", want, raw)
	}
	// A canonical/default endpoint has nothing to disambiguate, so the discrete
	// embed_base_url line is omitted (only an operator-overridden endpoint records it).
	if strings.Contains(string(raw), "embed_base_url:") {
		t.Fatalf("canonical default must not record embed_base_url:\n%s", raw)
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

// TestEmbedIdentity_CustomBaseURL_RecordsAndReindexes pins the SPEC 8.1.4
// base_url rule end-to-end (issue #560/#440 F3): an operator-overridden,
// non-canonical embed endpoint is recorded (both as embed_base_url and inside
// embed_identity) and a corpus built on one endpoint refuses to serve on a
// different one — the two vector spaces must not silently mix.
func TestEmbedIdentity_CustomBaseURL_RecordsAndReindexes(t *testing.T) {
	dir := t.TempDir()

	embedAt := func(base string) config.Config {
		cfg := loadCfg(t, ""+
			"providers:\n"+
			"  gpu-embed:\n"+
			"    kind: openai\n"+
			"    base_url: "+base+"\n"+
			"    embed_text_model: bge-m3\n"+
			"model:\n"+
			"  embed:\n"+
			"    provider: gpu-embed\n")
		cfg.StateDir = dir
		return cfg
	}

	// Build a corpus on custom endpoint A and snapshot it.
	cfgA := embedAt("http://gpu-vps-a:8080/v1/")
	if _, err := config.SaveEffectiveSnapshot(cfgA, config.SecretSourceMetadata{}); err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(config.EffectiveSnapshotPath(dir))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	// The non-canonical endpoint is recorded discretely (canonicalized: the
	// trailing slash is stripped) and folded into the identity's 2nd field.
	if !strings.Contains(string(raw), "embed_base_url: http://gpu-vps-a:8080/v1\n") {
		t.Fatalf("snapshot missing discrete embed_base_url:\n%s", raw)
	}
	if !strings.Contains(string(raw), "embed_identity: gpu-embed|http://gpu-vps-a:8080/v1|bge-m3|") {
		t.Fatalf("snapshot embed_identity missing base_url field:\n%s", raw)
	}

	// Same endpoint reloads without a spurious reindex.
	if err := cfgA.VerifyEmbedIdentity(dir); err != nil {
		t.Fatalf("same custom endpoint must pass: %v", err)
	}
	// A DIFFERENT custom endpoint is refused (CONFIG_INVALID): the index's
	// vectors are pinned to endpoint A's vector space.
	err = embedAt("http://gpu-vps-b:8080/v1").VerifyEmbedIdentity(dir)
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") {
		t.Fatalf("different custom endpoint must be refused, got: %v", err)
	}
}
