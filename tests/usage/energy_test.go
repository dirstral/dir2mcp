package tests

import (
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/usage"
)

// TestEnergyTable_KnownModelWh verifies the Wh estimate for a known model.
func TestEnergyTable_KnownModelWh(t *testing.T) {
	et := usage.NewEnergyTable(map[string]usage.EnergyFactor{
		"test-chat": {WhPer1K: 0.5},
	})
	wh, ok := et.EnergyWh("test-chat", 2000)
	if !ok {
		t.Fatal("expected known model to yield an energy estimate")
	}
	// 2000/1000 * 0.5 = 1.0 Wh
	if !approxEqual(wh, 1.0) {
		t.Fatalf("wh=%v, want 1.0", wh)
	}
}

// TestEnergyTable_UnknownModelOmits ensures unknown models never fabricate Wh.
func TestEnergyTable_UnknownModelOmits(t *testing.T) {
	et := usage.DefaultEnergyTable()
	wh, ok := et.EnergyWh("totally-made-up-model-xyz", 9999)
	if ok {
		t.Fatalf("unknown model must omit estimate, got ok=true wh=%v", wh)
	}
	if wh != 0 {
		t.Fatalf("unknown-model wh must be 0 (omitted), got %v", wh)
	}
}

// TestEnergyTable_OverridesReplaceDefaults checks operator overrides win.
func TestEnergyTable_OverridesReplaceDefaults(t *testing.T) {
	et := usage.NewEnergyTable(map[string]usage.EnergyFactor{
		"mistral-small-2506": {WhPer1K: 9.0},
	})
	wh, ok := et.EnergyWh("mistral-small-2506", 1000)
	if !ok {
		t.Fatal("expected overridden model to be known")
	}
	if !approxEqual(wh, 9.0) {
		t.Fatalf("override not applied: wh=%v, want 9.0", wh)
	}
}

// TestEnergyTable_NilOmits guards the nil-table path.
func TestEnergyTable_NilOmits(t *testing.T) {
	var et *usage.EnergyTable
	if _, ok := et.EnergyWh("mistral-small-2506", 1000); ok {
		t.Fatal("nil energy table must omit estimate")
	}
}

// TestCarbonModel_DisabledProducesNothing verifies the off-by-default contract.
func TestCarbonModel_DisabledProducesNothing(t *testing.T) {
	c := usage.NewCarbonModel(false, nil, usage.DefaultGridIntensityGramsPerWh)
	if c.Enabled() {
		t.Fatal("carbon model must be disabled")
	}
	if _, ok := c.EstimateWh("mistral-small-2506", 1000); ok {
		t.Fatal("disabled carbon model must not estimate Wh")
	}
}

// TestCarbonModel_NilDisabled guards the nil receiver.
func TestCarbonModel_NilDisabled(t *testing.T) {
	var c *usage.CarbonModel
	if c.Enabled() {
		t.Fatal("nil carbon model must be disabled")
	}
	if _, ok := c.EstimateWh("mistral-small-2506", 1000); ok {
		t.Fatal("nil carbon model must not estimate Wh")
	}
}

// TestCarbonModel_WhAndCO2e checks the full Wh -> gCO2e math for a known model.
func TestCarbonModel_WhAndCO2e(t *testing.T) {
	c := usage.NewCarbonModel(true, map[string]usage.EnergyFactor{
		"chat-x": {WhPer1K: 1.0},
	}, 0.4)
	wh, ok := c.EstimateWh("chat-x", 5000)
	if !ok {
		t.Fatal("expected Wh estimate for known model")
	}
	// 5000/1000 * 1.0 = 5.0 Wh
	if !approxEqual(wh, 5.0) {
		t.Fatalf("wh=%v, want 5.0", wh)
	}
	co2e, ok := c.EstimateCO2eGrams(wh)
	if !ok {
		t.Fatal("expected CO2e estimate with positive grid factor")
	}
	// 5.0 Wh * 0.4 gCO2e/Wh = 2.0 g
	if !approxEqual(co2e, 2.0) {
		t.Fatalf("co2e=%v, want 2.0", co2e)
	}
}

// TestCarbonModel_NonPositiveGridOmitsCO2e: Wh is surfaced, CO2e is omitted.
func TestCarbonModel_NonPositiveGridOmitsCO2e(t *testing.T) {
	c := usage.NewCarbonModel(true, map[string]usage.EnergyFactor{
		"chat-x": {WhPer1K: 1.0},
	}, 0)
	wh, ok := c.EstimateWh("chat-x", 1000)
	if !ok || !approxEqual(wh, 1.0) {
		t.Fatalf("expected Wh=1.0, got ok=%v wh=%v", ok, wh)
	}
	if _, ok := c.EstimateCO2eGrams(wh); ok {
		t.Fatal("non-positive grid factor must omit CO2e")
	}
}

// TestQueryMetrics_CarbonEnabledKnownModel exercises the emitter end-to-end:
// energy_wh + co2e_g present and flagged as an estimate.
func TestQueryMetrics_CarbonEnabledKnownModel(t *testing.T) {
	carbon := usage.NewCarbonModel(true, map[string]usage.EnergyFactor{
		"chat-x": {WhPer1K: 1.0},
	}, 0.4)
	qm := usage.NewQueryMetricsWithCarbon("ask", usage.DefaultPriceTable(), carbon)
	qm.RecordStage(usage.StageGenerate, "chat-x", 30*time.Millisecond,
		usage.Usage{PromptTokens: 1000, CompletionTokens: 1000}, true)
	qm.SetTotalLatency(40 * time.Millisecond)

	ev := qm.Event()
	wh, ok := ev["energy_wh"].(float64)
	if !ok {
		t.Fatalf("energy_wh missing: %v", ev["energy_wh"])
	}
	// 2000 tokens * 1.0 Wh/1K = 2.0 Wh
	if !approxEqual(wh, 2.0) {
		t.Fatalf("energy_wh=%v, want 2.0", wh)
	}
	if est, _ := ev["energy_estimate"].(bool); !est {
		t.Fatal("energy_estimate flag must be true when energy is surfaced")
	}
	co2e, ok := ev["co2e_g"].(float64)
	if !ok {
		t.Fatalf("co2e_g missing: %v", ev["co2e_g"])
	}
	// 2.0 Wh * 0.4 = 0.8 g
	if !approxEqual(co2e, 0.8) {
		t.Fatalf("co2e_g=%v, want 0.8", co2e)
	}
}

// TestQueryMetrics_CarbonDisabledOmitsFields: with carbon off, no energy fields
// appear (cost/latency unchanged).
func TestQueryMetrics_CarbonDisabledOmitsFields(t *testing.T) {
	qm := usage.NewQueryMetrics("ask", usage.DefaultPriceTable())
	qm.RecordStage(usage.StageGenerate, "mistral-small-2506", 10*time.Millisecond,
		usage.Usage{PromptTokens: 1000, CompletionTokens: 1000}, true)
	ev := qm.Event()
	if _, present := ev["energy_wh"]; present {
		t.Fatalf("energy_wh must be absent when carbon disabled, got %v", ev["energy_wh"])
	}
	if _, present := ev["co2e_g"]; present {
		t.Fatalf("co2e_g must be absent when carbon disabled, got %v", ev["co2e_g"])
	}
	if _, present := ev["energy_estimate"]; present {
		t.Fatal("energy_estimate flag must be absent when carbon disabled")
	}
}

// TestQueryMetrics_CarbonUnknownModelOmits: enabled but unknown model ⇒ no
// fabricated estimate.
func TestQueryMetrics_CarbonUnknownModelOmits(t *testing.T) {
	carbon := usage.NewCarbonModel(true, nil, 0.4)
	qm := usage.NewQueryMetricsWithCarbon("ask", usage.DefaultPriceTable(), carbon)
	qm.RecordStage(usage.StageGenerate, "unknown-model-zzz", 10*time.Millisecond,
		usage.Usage{PromptTokens: 1000, CompletionTokens: 1000}, true)
	ev := qm.Event()
	if _, present := ev["energy_wh"]; present {
		t.Fatalf("energy_wh must be omitted for unknown model, got %v", ev["energy_wh"])
	}
	if _, present := ev["co2e_g"]; present {
		t.Fatalf("co2e_g must be omitted for unknown model, got %v", ev["co2e_g"])
	}
	// tokens still present.
	if ev["total_tokens"].(int64) != 2000 {
		t.Fatalf("total_tokens=%v, want 2000", ev["total_tokens"])
	}
}
