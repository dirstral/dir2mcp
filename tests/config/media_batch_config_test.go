package tests

import (
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestMediaBatch_DefaultsOff pins that the batch-ergonomics surface (SPEC §8.6.11)
// is fully off by default, so a default ingest is unchanged.
func TestMediaBatch_DefaultsOff(t *testing.T) {
	cfg := config.Default()
	if cfg.MediaBatchTwoPhase || cfg.MediaBatchProgress || cfg.MediaBatchManifest != "" {
		t.Fatalf("media.batch must default off: two_phase=%v progress=%v manifest=%q",
			cfg.MediaBatchTwoPhase, cfg.MediaBatchProgress, cfg.MediaBatchManifest)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

// TestMediaBatch_NestedYAMLApplies pins the nested YAML form (media.batch.*) is
// honored — media.batch must be a recognized map section.
func TestMediaBatch_NestedYAMLApplies(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"media:\n"+
		"  batch:\n"+
		"    progress: true\n"+
		"    manifest: run.jsonl\n")

	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MediaBatchProgress {
		t.Fatal("nested media.batch.progress was ignored")
	}
	if cfg.MediaBatchManifest != "run.jsonl" {
		t.Fatalf("media.batch.manifest = %q, want run.jsonl", cfg.MediaBatchManifest)
	}
}

// TestMediaBatch_TwoPhaseRejected pins that enabling the not-yet-implemented
// two-phase pass split is rejected up front rather than silently behaving as
// single-pass.
func TestMediaBatch_TwoPhaseRejected(t *testing.T) {
	cfg := config.Default()
	cfg.MediaBatchTwoPhase = true
	err := cfg.Validate()
	if err == nil {
		t.Fatal("media.batch.two_phase=true must fail validation (not yet implemented)")
	}
	if !strings.Contains(err.Error(), "two_phase") {
		t.Fatalf("error should name two_phase, got: %v", err)
	}
}

// TestMediaBatch_RoundTrip pins the non-secret knobs survive a snapshot
// round-trip via the flat keys.
func TestMediaBatch_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, ""+
		"root_dir: ./repo\n"+
		"media_batch_progress: true\n"+
		"media_batch_manifest: /tmp/run.jsonl\n")

	t.Setenv("DIR2MCP_DISABLE_KEYCHAIN", "1")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.MediaBatchProgress || cfg.MediaBatchManifest != "/tmp/run.jsonl" {
		t.Fatalf("flat keys not loaded: progress=%v manifest=%q", cfg.MediaBatchProgress, cfg.MediaBatchManifest)
	}
}
