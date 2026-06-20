package tests

import (
	"math"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/usage"
)

// TestCarbon_DefaultDisabled verifies the opt-in contract: off by default (#328).
func TestCarbon_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.Carbon.Enabled {
		t.Fatal("carbon estimate must default to disabled")
	}
	if len(cfg.Carbon.EnergyOverrides) != 0 {
		t.Fatalf("energy overrides must default to empty, got %v", cfg.Carbon.EnergyOverrides)
	}
}

// TestCarbon_AbsentBlockDisabled: no carbon: block ⇒ disabled, no error.
func TestCarbon_AbsentBlockDisabled(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, "server:\n  port: 8080\n")
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Carbon.Enabled {
		t.Fatal("absent carbon block must leave estimate disabled")
	}
}

// TestCarbon_YAMLRoundTrips pins the carbon: block parsing for #328.
func TestCarbon_YAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, strings.Join([]string{
		"carbon:",
		"  enabled: true",
		"  grid_g_co2e_per_wh: 0.35",
		"  energy:",
		"    my-chat-model:",
		"      wh_per_1k: 0.75",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.Carbon.Enabled {
		t.Fatal("expected carbon enabled")
	}
	if math.Abs(cfg.Carbon.GridGramsCO2ePerWh-0.35) > 1e-12 {
		t.Fatalf("grid factor mismatch: %v", cfg.Carbon.GridGramsCO2ePerWh)
	}
	f, ok := cfg.Carbon.EnergyOverrides["my-chat-model"]
	if !ok {
		t.Fatalf("expected energy override for my-chat-model, got %v", cfg.Carbon.EnergyOverrides)
	}
	if math.Abs(f.WhPer1K-0.75) > 1e-12 {
		t.Fatalf("wh_per_1k mismatch: %+v", f)
	}
}

// TestCarbon_DefaultGridWhenOmitted: enabled with no grid key ⇒ built-in default
// so a minimal config still yields a CO2e estimate.
func TestCarbon_DefaultGridWhenOmitted(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, strings.Join([]string{
		"carbon:",
		"  enabled: true",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.Carbon.Enabled {
		t.Fatal("expected carbon enabled")
	}
	if math.Abs(cfg.Carbon.GridGramsCO2ePerWh-usage.DefaultGridIntensityGramsPerWh) > 1e-12 {
		t.Fatalf("grid factor should default to %v, got %v",
			usage.DefaultGridIntensityGramsPerWh, cfg.Carbon.GridGramsCO2ePerWh)
	}
}
