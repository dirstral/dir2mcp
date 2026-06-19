package tests

import (
	"math"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestCostPrices_DefaultEmpty verifies no overrides are present by default.
func TestCostPrices_DefaultEmpty(t *testing.T) {
	cfg := config.Default()
	if len(cfg.CostPriceOverrides) != 0 {
		t.Fatalf("cost price overrides must default to empty, got %v", cfg.CostPriceOverrides)
	}
}

// TestCostPrices_YAMLRoundTrips pins the cost.prices block parsing for #327.
func TestCostPrices_YAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := tmp + "/.dir2mcp.yaml"
	writeFile(t, path, strings.Join([]string{
		"cost:",
		"  prices:",
		"    my-chat-model:",
		"      input_per_1k: 0.0005",
		"      output_per_1k: 0.0015",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	p, ok := cfg.CostPriceOverrides["my-chat-model"]
	if !ok {
		t.Fatalf("expected cost override for my-chat-model, got %v", cfg.CostPriceOverrides)
	}
	if math.Abs(p.InputPer1K-0.0005) > 1e-12 || math.Abs(p.OutputPer1K-0.0015) > 1e-12 {
		t.Fatalf("price mismatch: %+v", p)
	}
}
