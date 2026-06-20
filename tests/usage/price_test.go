package tests

import (
	"math"
	"testing"

	"github.com/dirstral/dir2mcp/internal/usage"
)

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestPriceTable_KnownModelCost(t *testing.T) {
	pt := usage.NewPriceTable(map[string]usage.ModelPrice{
		"test-chat": {InputPer1K: 0.001, OutputPer1K: 0.002},
	})
	cost, ok := pt.Cost("test-chat", usage.Usage{PromptTokens: 1000, CompletionTokens: 500})
	if !ok {
		t.Fatal("expected known model to yield a cost")
	}
	// 1000/1000*0.001 + 500/1000*0.002 = 0.001 + 0.001 = 0.002
	if !approxEqual(cost, 0.002) {
		t.Fatalf("cost=%v, want 0.002", cost)
	}
}

func TestPriceTable_UnknownModelOmitsCost(t *testing.T) {
	pt := usage.DefaultPriceTable()
	cost, ok := pt.Cost("totally-made-up-model-xyz", usage.Usage{PromptTokens: 9999, CompletionTokens: 9999})
	if ok {
		t.Fatalf("unknown model must omit cost, got ok=true cost=%v", cost)
	}
	if cost != 0 {
		t.Fatalf("unknown-model cost must be 0 (omitted), got %v", cost)
	}
}

func TestPriceTable_CaseInsensitiveLookup(t *testing.T) {
	pt := usage.NewPriceTable(map[string]usage.ModelPrice{
		"  Mixed-Case-Model ": {InputPer1K: 0.01},
	})
	if _, ok := pt.Lookup("mixed-case-model"); !ok {
		t.Fatal("lookup should be case-insensitive and whitespace-trimmed")
	}
}

func TestPriceTable_OverridesReplaceDefaults(t *testing.T) {
	// mistral-small-2506 exists in the default table; override it.
	pt := usage.NewPriceTable(map[string]usage.ModelPrice{
		"mistral-small-2506": {InputPer1K: 1.0, OutputPer1K: 1.0},
	})
	cost, ok := pt.Cost("mistral-small-2506", usage.Usage{PromptTokens: 1000})
	if !ok {
		t.Fatal("expected overridden model to be priced")
	}
	if !approxEqual(cost, 1.0) {
		t.Fatalf("override not applied: cost=%v, want 1.0", cost)
	}
}

func TestPriceTable_DefaultsCoverCommonModels(t *testing.T) {
	pt := usage.DefaultPriceTable()
	for _, m := range []string{"mistral-small-2506", "mistral-embed", "gpt-4o-mini", "gemini-2.5-flash"} {
		if _, ok := pt.Lookup(m); !ok {
			t.Errorf("default table missing common model %q", m)
		}
	}
}

func TestPriceTable_NilTableOmitsCost(t *testing.T) {
	var pt *usage.PriceTable
	if _, ok := pt.Cost("mistral-small-2506", usage.Usage{PromptTokens: 1000}); ok {
		t.Fatal("nil price table must omit cost")
	}
}
