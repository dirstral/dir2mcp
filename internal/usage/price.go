package usage

import "strings"

// ModelPrice is the per-1K-token USD price for a model. Either field may be
// zero (e.g. embedding models have no output price). Prices are approximate
// and operator-overridable; they exist for relative budgeting, not billing.
type ModelPrice struct {
	// InputPer1K is USD per 1,000 prompt/input tokens.
	InputPer1K float64
	// OutputPer1K is USD per 1,000 completion/output tokens.
	OutputPer1K float64
}

// PriceTable maps a model name to its price. Lookup is case-insensitive and
// trims surrounding whitespace. An unknown model yields ok=false so callers can
// OMIT cost rather than fabricate a number (issue #327 requirement).
type PriceTable struct {
	prices map[string]ModelPrice
}

// DefaultPriceTable returns a built-in table with sensible approximate USD
// prices (per 1K tokens) for common dir2mcp providers, current as of mid-2026.
// These are starting points; operators override via config (NewPriceTable /
// Merge). Unknown models are intentionally absent so their cost is omitted.
func DefaultPriceTable() *PriceTable {
	return &PriceTable{prices: map[string]ModelPrice{
		// Mistral (chat)
		"mistral-small-2506": {InputPer1K: 0.0001, OutputPer1K: 0.0003},
		"mistral-small":      {InputPer1K: 0.0001, OutputPer1K: 0.0003},
		"mistral-large":      {InputPer1K: 0.002, OutputPer1K: 0.006},
		"mistral-medium":     {InputPer1K: 0.0004, OutputPer1K: 0.002},
		// Mistral (embed)
		"mistral-embed":   {InputPer1K: 0.0001},
		"codestral-embed": {InputPer1K: 0.00015},
		// OpenAI (chat)
		"gpt-4o":      {InputPer1K: 0.0025, OutputPer1K: 0.01},
		"gpt-4o-mini": {InputPer1K: 0.00015, OutputPer1K: 0.0006},
		// OpenAI (embed)
		"text-embedding-3-small": {InputPer1K: 0.00002},
		"text-embedding-3-large": {InputPer1K: 0.00013},
		// Gemini (chat)
		"gemini-2.5-flash": {InputPer1K: 0.0003, OutputPer1K: 0.0025},
		"gemini-2.5-pro":   {InputPer1K: 0.00125, OutputPer1K: 0.01},
		// Gemini (embed)
		"gemini-embedding-001": {InputPer1K: 0.00015},
		// Cohere (rerank: priced per search, not tokens; left out so cost is
		// omitted rather than mis-stated)
		"command-r":      {InputPer1K: 0.00015, OutputPer1K: 0.0006},
		"command-r-plus": {InputPer1K: 0.0025, OutputPer1K: 0.01},
	}}
}

// NewPriceTable builds a table from the default table merged with operator
// overrides. Override entries replace defaults by (case-insensitive) model
// name; new entries are added. A nil/empty overrides map yields the defaults.
func NewPriceTable(overrides map[string]ModelPrice) *PriceTable {
	t := DefaultPriceTable()
	t.Merge(overrides)
	return t
}

// Merge applies operator overrides onto the table in place. Keys are
// normalized (trimmed, lower-cased). Existing entries are replaced.
func (t *PriceTable) Merge(overrides map[string]ModelPrice) {
	if t == nil || len(overrides) == 0 {
		return
	}
	if t.prices == nil {
		t.prices = make(map[string]ModelPrice, len(overrides))
	}
	for name, p := range overrides {
		key := normalizeModel(name)
		if key == "" {
			continue
		}
		t.prices[key] = p
	}
}

// Lookup returns the price for model and whether it is known. Unknown models
// return ok=false; callers MUST omit cost in that case.
func (t *PriceTable) Lookup(model string) (ModelPrice, bool) {
	if t == nil {
		return ModelPrice{}, false
	}
	p, ok := t.prices[normalizeModel(model)]
	return p, ok
}

// Cost returns the USD cost for the given token usage under model's price, and
// whether a price was known. When the model is unknown, ok=false and cost is 0
// (callers omit it). A known model with zero usage yields cost 0, ok=true.
func (t *PriceTable) Cost(model string, u Usage) (float64, bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	cost := (float64(u.PromptTokens)/1000.0)*p.InputPer1K +
		(float64(u.CompletionTokens)/1000.0)*p.OutputPer1K
	return cost, true
}

func normalizeModel(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
