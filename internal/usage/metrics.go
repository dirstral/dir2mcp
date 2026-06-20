package usage

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// stageMetric is the per-stage breakdown surfaced in the query_metrics event.
// Cost/EnergyWh/CO2eG are pointers so an unknown-model (no price / no energy
// factor) stage OMITS them rather than reporting a misleading 0.0.
type stageMetric struct {
	LatencyMS        int64    `json:"latency_ms"`
	Model            string   `json:"model,omitempty"`
	PromptTokens     int64    `json:"prompt_tokens,omitempty"`
	CompletionTokens int64    `json:"completion_tokens,omitempty"`
	TotalTokens      int64    `json:"total_tokens,omitempty"`
	TokensReported   bool     `json:"tokens_reported"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	// EnergyWh / CO2eG are OPT-IN approximate estimates (issue #328), present
	// only when carbon estimation is enabled and the stage's model has a known
	// energy factor. CO2eG additionally requires a positive grid factor.
	EnergyWh *float64 `json:"energy_wh,omitempty"`
	CO2eG    *float64 `json:"co2e_g,omitempty"`
}

// QueryMetrics accumulates per-query latency and (where available) token usage
// across stages, then renders a single structured event payload. It is not
// safe for concurrent stage registration; the retrieval service drives stages
// sequentially.
type QueryMetrics struct {
	op     string // "ask" or "search"
	prices *PriceTable
	carbon *CarbonModel
	stages map[Stage]*stageMetric
	order  []Stage
	total  time.Duration
}

// NewQueryMetrics starts metrics collection for an operation ("ask"/"search")
// using prices for cost mapping. A nil prices table still records tokens and
// latency; cost is simply always omitted. Carbon estimation is off; use
// NewQueryMetricsWithCarbon to enable the opt-in energy/CO2e estimate (#328).
func NewQueryMetrics(op string, prices *PriceTable) *QueryMetrics {
	return NewQueryMetricsWithCarbon(op, prices, nil)
}

// NewQueryMetricsWithCarbon is NewQueryMetrics plus an optional CarbonModel for
// the opt-in energy/CO2e estimate (issue #328). A nil or disabled carbon model
// omits all energy/CO2e fields, leaving cost/latency unchanged.
func NewQueryMetricsWithCarbon(op string, prices *PriceTable, carbon *CarbonModel) *QueryMetrics {
	return &QueryMetrics{
		op:     op,
		prices: prices,
		carbon: carbon,
		stages: make(map[Stage]*stageMetric),
	}
}

// RecordStage registers a stage's wall-clock latency, model name, and the
// usage observed for it (typically pulled from a Sink). Calling it more than
// once for the same stage accumulates latency and tokens.
func (m *QueryMetrics) RecordStage(stage Stage, model string, latency time.Duration, u Usage, reported bool) {
	if m == nil {
		return
	}
	sm, ok := m.stages[stage]
	if !ok {
		sm = &stageMetric{}
		m.stages[stage] = sm
		m.order = append(m.order, stage)
	}
	sm.LatencyMS += latency.Milliseconds()
	if model != "" {
		sm.Model = model
	}
	if reported {
		sm.PromptTokens += u.PromptTokens
		sm.CompletionTokens += u.CompletionTokens
		if u.TotalTokens > 0 {
			sm.TotalTokens += u.TotalTokens
		} else {
			sm.TotalTokens += u.PromptTokens + u.CompletionTokens
		}
		sm.TokensReported = true
	}
}

// SetTotalLatency records the overall wall-clock time for the query, which may
// exceed the sum of stages (it includes index search, fusion, dedup, etc.).
func (m *QueryMetrics) SetTotalLatency(d time.Duration) {
	if m != nil {
		m.total = d
	}
}

// finalized holds the aggregate totals computed once per render so Event and
// LogLine stay consistent without recomputing.
type finalized struct {
	totalCost   float64
	costKnown   bool
	totalTokens int64
	totalWh     float64
	energyKnown bool
	totalCO2eG  float64
	co2eKnown   bool
	carbonOn    bool
}

// finalize computes each stage's cost (when its model is priced) and, when
// carbon estimation is enabled, its energy/CO2e estimate (when its model has a
// known factor), populating the per-stage pointers and returning the aggregate
// totals. Unknown models omit the respective field rather than fabricating 0.
func (m *QueryMetrics) finalize() finalized {
	f := finalized{carbonOn: m.carbon.Enabled()}
	for _, stage := range m.order {
		sm := m.stages[stage]
		if sm.TokensReported {
			f.totalTokens += sm.TotalTokens
		}
		if sm.TokensReported && sm.Model != "" {
			if cost, ok := m.prices.Cost(sm.Model, Usage{
				PromptTokens:     sm.PromptTokens,
				CompletionTokens: sm.CompletionTokens,
			}); ok {
				rounded := roundUSD(cost)
				sm.CostUSD = &rounded
				f.totalCost += cost
				f.costKnown = true
			}
			if f.carbonOn {
				m.finalizeStageCarbon(sm, &f)
			}
		}
	}
	return f
}

// finalizeStageCarbon estimates and records a single stage's energy/CO2e,
// updating the running totals. Caller guarantees carbon is enabled and the
// stage reported tokens for a named model.
func (m *QueryMetrics) finalizeStageCarbon(sm *stageMetric, f *finalized) {
	wh, ok := m.carbon.EstimateWh(sm.Model, sm.TotalTokens)
	if !ok {
		return
	}
	roundedWh := roundEnergy(wh)
	sm.EnergyWh = &roundedWh
	f.totalWh += wh
	f.energyKnown = true
	if co2e, ok := m.carbon.EstimateCO2eGrams(wh); ok {
		roundedCO2e := roundEnergy(co2e)
		sm.CO2eG = &roundedCO2e
		f.totalCO2eG += co2e
		f.co2eKnown = true
	}
}

// Event renders the structured payload for the query_metrics NDJSON event.
// It contains ONLY counts, costs, and latency — never prompts, documents, or
// keys. cost_usd (per-stage and total) is omitted entirely when no priced
// model contributed, so unknown models never fabricate a cost.
func (m *QueryMetrics) Event() map[string]interface{} {
	f := m.finalize()

	stages := make(map[string]interface{}, len(m.order))
	for _, stage := range m.order {
		stages[string(stage)] = m.stages[stage]
	}

	payload := map[string]interface{}{
		"op":           m.op,
		"latency_ms":   m.total.Milliseconds(),
		"total_tokens": f.totalTokens,
		"stages":       stages,
	}
	if f.costKnown {
		payload["cost_usd"] = roundUSD(f.totalCost)
	}
	// Energy/CO2e are OPT-IN, approximate estimates (issue #328). They are
	// surfaced only when enabled and at least one stage had a known factor, and
	// are explicitly flagged with energy_estimate=true so consumers never mistake
	// them for measurements.
	if f.energyKnown {
		payload["energy_wh"] = roundEnergy(f.totalWh)
		payload["energy_estimate"] = true
	}
	if f.co2eKnown {
		payload["co2e_g"] = roundEnergy(f.totalCO2eG)
	}
	return payload
}

// LogLine renders a concise, human-readable one-liner for stderr logs. It
// mirrors Event but never includes raw payloads.
func (m *QueryMetrics) LogLine() string {
	f := m.finalize()
	var b strings.Builder
	fmt.Fprintf(&b, "query_metrics op=%s latency_ms=%d tokens=%d", m.op, m.total.Milliseconds(), f.totalTokens)
	if f.costKnown {
		fmt.Fprintf(&b, " cost_usd=%.6f", roundUSD(f.totalCost))
	} else {
		b.WriteString(" cost_usd=unknown")
	}
	if f.carbonOn {
		if f.energyKnown {
			fmt.Fprintf(&b, " energy_wh~%.4f", roundEnergy(f.totalWh))
		} else {
			b.WriteString(" energy_wh=unknown")
		}
		if f.co2eKnown {
			fmt.Fprintf(&b, " co2e_g~%.4f", roundEnergy(f.totalCO2eG))
		}
	}
	for _, stage := range m.order {
		sm := m.stages[stage]
		fmt.Fprintf(&b, " %s[%dms", stage, sm.LatencyMS)
		if sm.TokensReported {
			fmt.Fprintf(&b, ",tok=%d", sm.TotalTokens)
		}
		b.WriteString("]")
	}
	return b.String()
}

// roundUSD rounds to 6 decimal places (micro-dollar) for stable, readable
// output without floating-point noise.
func roundUSD(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}

// roundEnergy rounds Wh / gCO2e estimates to 4 decimals for stable, readable
// output. The estimate's inherent uncertainty far exceeds this precision; the
// rounding only removes floating-point noise.
func roundEnergy(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}
