package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestRetrievalMinScore_DefaultDisabled pins the local-first default: the
// relevance floor is 0 (disabled) unless explicitly configured.
func TestRetrievalMinScore_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalMinScore != 0 {
		t.Fatalf("retrieval.min_score must default to 0 (disabled), got %v", cfg.RetrievalMinScore)
	}
}

// TestRetrievalMinScore_NestedYAMLRoundTrips pins the nested-mapping form
// (retrieval: \n  min_score: 0.4) onto Config.RetrievalMinScore.
func TestRetrievalMinScore_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  min_score: 0.42",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RetrievalMinScore != 0.42 {
		t.Fatalf("nested retrieval.min_score = %v, want 0.42", cfg.RetrievalMinScore)
	}
}

// TestRetrievalMinScore_FlatAliasRoundTrips pins the flat snake_case alias
// (retrieval_min_score -> retrieval.min_score).
func TestRetrievalMinScore_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_min_score: 0.25\n")

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.RetrievalMinScore != 0.25 {
		t.Fatalf("flat retrieval_min_score = %v, want 0.25", cfg.RetrievalMinScore)
	}
}

// TestRetrievalMinScore_SnapshotRoundTrips pins the persisted-snapshot
// round-trip: the floor survives SaveEffectiveSnapshot -> YAML -> load.
func TestRetrievalMinScore_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.RetrievalMinScore = 0.33

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "retrieval_min_score: 0.33") {
		t.Fatalf("snapshot must persist retrieval_min_score: 0.33:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if loaded.RetrievalMinScore != 0.33 {
		t.Fatalf("loaded snapshot must carry RetrievalMinScore=0.33, got %v", loaded.RetrievalMinScore)
	}
}

// TestRetrievalMinScore_RejectsNonFinite asserts NaN/Inf strings (which
// strconv.ParseFloat accepts) are rejected at config-parse time rather than
// silently corrupting the floor comparison.
func TestRetrievalMinScore_RejectsNonFinite(t *testing.T) {
	for _, bad := range []string{"NaN", "Inf", "+Inf", "-Inf", "Infinity"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, "retrieval_min_score: "+bad+"\n")

		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("LoadFile with retrieval_min_score=%q = nil error, want rejection", bad)
		}
	}
}

// TestRetrievalMinScore_RejectsNegative pins validation: a negative floor is
// CONFIG_INVALID (it would never drop anything, so it is a misconfiguration).
func TestRetrievalMinScore_RejectsNegative(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_min_score: -0.1\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatalf("LoadFile with negative retrieval_min_score = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "retrieval.min_score") {
		t.Fatalf("error must mention retrieval.min_score, got: %v", err)
	}
}

// TestRetrievalMinScore_ValidateRejectsNegativeProgrammatic pins that the
// Validate() guard rejects a negative floor injected programmatically (not via
// the file parser), so the comparison can never run with a bad floor.
func TestRetrievalMinScore_ValidateRejectsNegativeProgrammatic(t *testing.T) {
	cfg := config.Default()
	cfg.RetrievalMinScore = -1
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate with negative RetrievalMinScore = nil error, want rejection")
	}
}
