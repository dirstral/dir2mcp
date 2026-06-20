package usage

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// stageMetric is the per-stage breakdown surfaced in the query_metrics event.
// Cost is a pointer so an unknown-model (no price) stage OMITS cost rather than
// reporting a misleading 0.0.
type stageMetric struct {
	LatencyMS        int64    `json:"latency_ms"`
	Model            string   `json:"model,omitempty"`
	PromptTokens     int64    `json:"prompt_tokens,omitempty"`
	CompletionTokens int64    `json:"completion_tokens,omitempty"`
	TotalTokens      int64    `json:"total_tokens,omitempty"`
	TokensReported   bool     `json:"tokens_reported"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
}

// QueryMetrics accumulates per-query latency and (where available) token usage
// across stages, then renders a single structured event payload. It is not
// safe for concurrent stage registration; the retrieval service drives stages
// sequentially.
type QueryMetrics struct {
	op     string // "ask" or "search"
	prices *PriceTable
	stages map[Stage]*stageMetric
	order  []Stage
	total  time.Duration
}

// NewQueryMetrics starts metrics collection for an operation ("ask"/"search")
// using prices for cost mapping. A nil prices table still records tokens and
// latency; cost is simply always omitted.
func NewQueryMetrics(op string, prices *PriceTable) *QueryMetrics {
	return &QueryMetrics{
		op:     op,
		prices: prices,
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

// finalize computes each stage's cost (when its model is priced) and returns
// the aggregate totals: known cost, whether any stage had a known cost, and
// total tokens across stages.
func (m *QueryMetrics) finalize() (totalCost float64, costKnown bool, totalTokens int64) {
	for _, stage := range m.order {
		sm := m.stages[stage]
		if sm.TokensReported {
			totalTokens += sm.TotalTokens
		}
		if sm.TokensReported && sm.Model != "" {
			cost, ok := m.prices.Cost(sm.Model, Usage{
				PromptTokens:     sm.PromptTokens,
				CompletionTokens: sm.CompletionTokens,
			})
			if ok {
				rounded := roundUSD(cost)
				sm.CostUSD = &rounded
				totalCost += cost
				costKnown = true
			}
		}
	}
	return totalCost, costKnown, totalTokens
}

// Event renders the structured payload for the query_metrics NDJSON event.
// It contains ONLY counts, costs, and latency — never prompts, documents, or
// keys. cost_usd (per-stage and total) is omitted entirely when no priced
// model contributed, so unknown models never fabricate a cost.
func (m *QueryMetrics) Event() map[string]interface{} {
	totalCost, costKnown, totalTokens := m.finalize()

	stages := make(map[string]interface{}, len(m.order))
	for _, stage := range m.order {
		stages[string(stage)] = m.stages[stage]
	}

	payload := map[string]interface{}{
		"op":           m.op,
		"latency_ms":   m.total.Milliseconds(),
		"total_tokens": totalTokens,
		"stages":       stages,
	}
	if costKnown {
		payload["cost_usd"] = roundUSD(totalCost)
	}
	return payload
}

// LogLine renders a concise, human-readable one-liner for stderr logs. It
// mirrors Event but never includes raw payloads.
func (m *QueryMetrics) LogLine() string {
	totalCost, costKnown, totalTokens := m.finalize()
	var b strings.Builder
	fmt.Fprintf(&b, "query_metrics op=%s latency_ms=%d tokens=%d", m.op, m.total.Milliseconds(), totalTokens)
	if costKnown {
		fmt.Fprintf(&b, " cost_usd=%.6f", roundUSD(totalCost))
	} else {
		b.WriteString(" cost_usd=unknown")
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
