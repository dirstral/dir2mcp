package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestRetrievalMMR_Defaults pins the local-first defaults: MMR is OFF and the
// lambda trade-off defaults to a balanced 0.5.
func TestRetrievalMMR_Defaults(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalMMREnabled {
		t.Fatalf("retrieval.mmr.enabled must default to false")
	}
	if cfg.RetrievalMMRLambda != 0.5 {
		t.Fatalf("retrieval.mmr.lambda must default to 0.5, got %v", cfg.RetrievalMMRLambda)
	}
}

// TestRetrievalMMR_NestedYAMLRoundTrips pins the nested-mapping form onto
// Config.RetrievalMMREnabled / RetrievalMMRLambda.
func TestRetrievalMMR_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  mmr:",
		"    enabled: true",
		"    lambda: 0.7",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalMMREnabled {
		t.Fatalf("nested retrieval.mmr.enabled = false, want true")
	}
	if cfg.RetrievalMMRLambda != 0.7 {
		t.Fatalf("nested retrieval.mmr.lambda = %v, want 0.7", cfg.RetrievalMMRLambda)
	}
}

// TestRetrievalMMR_FlatAliasRoundTrips pins the flat snake_case aliases
// (retrieval_mmr_enabled / retrieval_mmr_lambda).
func TestRetrievalMMR_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval_mmr_enabled: true",
		"retrieval_mmr_lambda: 0.3",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalMMREnabled {
		t.Fatalf("flat retrieval_mmr_enabled = false, want true")
	}
	if cfg.RetrievalMMRLambda != 0.3 {
		t.Fatalf("flat retrieval_mmr_lambda = %v, want 0.3", cfg.RetrievalMMRLambda)
	}
}

// TestRetrievalMMR_SnapshotRoundTrips pins the persisted-snapshot round-trip:
// both knobs survive SaveEffectiveSnapshot -> YAML -> load.
func TestRetrievalMMR_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.RetrievalMMREnabled = true
	cfg.RetrievalMMRLambda = 0.8

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "retrieval_mmr_enabled: true") {
		t.Fatalf("snapshot must persist retrieval_mmr_enabled: true:\n%s", raw)
	}
	if !strings.Contains(string(raw), "retrieval_mmr_lambda: 0.8") {
		t.Fatalf("snapshot must persist retrieval_mmr_lambda: 0.8:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !loaded.RetrievalMMREnabled || loaded.RetrievalMMRLambda != 0.8 {
		t.Fatalf("loaded snapshot must carry enabled=true lambda=0.8, got enabled=%v lambda=%v",
			loaded.RetrievalMMREnabled, loaded.RetrievalMMRLambda)
	}
}

// TestRetrievalMMR_RejectsLambdaOutOfRange pins range validation: a lambda
// outside [0,1] is CONFIG_INVALID.
func TestRetrievalMMR_RejectsLambdaOutOfRange(t *testing.T) {
	for _, bad := range []string{"-0.1", "1.5", "2"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, "retrieval_mmr_lambda: "+bad+"\n")

		_, err := config.LoadFile(path)
		if err == nil {
			t.Errorf("LoadFile with retrieval_mmr_lambda=%q = nil error, want rejection", bad)
			continue
		}
		if !strings.Contains(err.Error(), "retrieval.mmr.lambda") {
			t.Errorf("error must mention retrieval.mmr.lambda, got: %v", err)
		}
	}
}

// TestRetrievalMMR_AcceptsLambdaBoundaries pins that the inclusive endpoints 0
// and 1 are valid (pure diversity / pure relevance).
func TestRetrievalMMR_AcceptsLambdaBoundaries(t *testing.T) {
	for _, good := range []string{"0", "1", "0.0", "1.0"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, "retrieval_mmr_lambda: "+good+"\n")

		if _, err := config.LoadFile(path); err != nil {
			t.Errorf("LoadFile with retrieval_mmr_lambda=%q = %v, want accepted", good, err)
		}
	}
}

// TestRetrievalMMR_RejectsNonFinite asserts NaN/Inf strings (which
// strconv.ParseFloat accepts) are rejected at config-parse time.
func TestRetrievalMMR_RejectsNonFinite(t *testing.T) {
	for _, bad := range []string{"NaN", "Inf", "+Inf", "-Inf", "Infinity"} {
		tmp := t.TempDir()
		path := filepath.Join(tmp, ".dir2mcp.yaml")
		writeFile(t, path, "retrieval_mmr_lambda: "+bad+"\n")

		if _, err := config.LoadFile(path); err == nil {
			t.Errorf("LoadFile with retrieval_mmr_lambda=%q = nil error, want rejection", bad)
		}
	}
}

// TestRetrievalMMR_ValidateRejectsOutOfRangeProgrammatic pins that the
// Validate() guard rejects an out-of-range lambda injected programmatically
// (not via the file parser).
func TestRetrievalMMR_ValidateRejectsOutOfRangeProgrammatic(t *testing.T) {
	cfg := config.Default()
	cfg.RetrievalMMRLambda = 1.7
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate with RetrievalMMRLambda=1.7 = nil error, want rejection")
	}
}
