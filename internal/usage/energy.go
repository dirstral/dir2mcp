package usage

// Energy / carbon footprint estimation (issue #328).
//
// This is an OPT-IN, deliberately APPROXIMATE estimate. It multiplies the
// already-captured token counts by a configurable per-model energy factor
// (Wh per 1,000 tokens) to produce watt-hours, and optionally multiplies that
// by a grid carbon-intensity factor (gCO2e per Wh) to produce grams of CO2e.
//
// It is NOT a measurement. Real energy use depends on hardware, batching,
// utilization, datacenter PUE, and grid mix — none of which dir2mcp can
// observe. The numbers exist as a relative sustainability signal, clearly
// labelled an estimate, with every factor operator-overridable. When a model
// has no known factor the estimate is OMITTED (never fabricated as 0), exactly
// like the cost path.
//
// Privacy: like the rest of this package, only counts and derived estimates
// are recorded — never prompts, documents, queries, or keys.

// DefaultGridIntensityGramsPerWh is the default grid carbon-intensity factor in
// grams of CO2e per watt-hour, used when carbon estimation is enabled but no
// grid factor is configured. ~0.4 kgCO2e/kWh = 0.4 gCO2e/Wh is a rough global
// average electricity intensity (mid-2020s). Operators should override it with
// a value for their region / energy contract for a meaningful estimate.
const DefaultGridIntensityGramsPerWh = 0.4

// EnergyFactor is the approximate electricity drawn by a model per 1,000
// tokens, in watt-hours. A single factor is applied to total tokens
// (prompt+completion); the prompt/completion split is not modeled because the
// per-token energy difference is dwarfed by the estimate's overall uncertainty.
type EnergyFactor struct {
	// WhPer1K is watt-hours consumed per 1,000 tokens processed. Approximate
	// and operator-overridable.
	WhPer1K float64
}

// EnergyTable maps a model name to its approximate energy factor. Lookup is
// case-insensitive and whitespace-trimmed (it reuses normalizeModel). An
// unknown model yields ok=false so callers OMIT the estimate rather than
// fabricate a number — mirroring PriceTable (issue #328).
type EnergyTable struct {
	factors map[string]EnergyFactor
}

// DefaultEnergyTable returns a built-in table with rough Wh-per-1K-token
// factors for common dir2mcp models. These are order-of-magnitude starting
// points drawn from public LLM-inference energy estimates (mid-2020s); they are
// NOT vendor-published figures. Small embedding models draw far less than large
// chat models. Operators override via config (carbon.energy / NewEnergyTable).
// Unknown models are intentionally absent so their estimate is omitted.
func DefaultEnergyTable() *EnergyTable {
	return &EnergyTable{factors: map[string]EnergyFactor{
		// Mistral (chat)
		"mistral-small-2506": {WhPer1K: 0.30},
		"mistral-small":      {WhPer1K: 0.30},
		"mistral-medium":     {WhPer1K: 0.60},
		"mistral-large":      {WhPer1K: 1.20},
		// Mistral (embed)
		"mistral-embed":   {WhPer1K: 0.02},
		"codestral-embed": {WhPer1K: 0.02},
		// OpenAI (chat)
		"gpt-4o":      {WhPer1K: 1.00},
		"gpt-4o-mini": {WhPer1K: 0.20},
		// OpenAI (embed)
		"text-embedding-3-small": {WhPer1K: 0.01},
		"text-embedding-3-large": {WhPer1K: 0.02},
		// Gemini (chat)
		"gemini-2.5-flash": {WhPer1K: 0.20},
		"gemini-2.5-pro":   {WhPer1K: 1.00},
		// Gemini (embed)
		"gemini-embedding-001": {WhPer1K: 0.02},
		// Anthropic (chat)
		"claude-sonnet-4-6": {WhPer1K: 0.60},
		"claude-haiku-4-5":  {WhPer1K: 0.20},
		// Cohere (chat)
		"command-r":      {WhPer1K: 0.30},
		"command-r-plus": {WhPer1K: 1.00},
	}}
}

// NewEnergyTable builds a table from the default table merged with operator
// overrides. Override entries replace defaults by (case-insensitive) model
// name; new entries are added. A nil/empty overrides map yields the defaults.
func NewEnergyTable(overrides map[string]EnergyFactor) *EnergyTable {
	t := DefaultEnergyTable()
	t.Merge(overrides)
	return t
}

// Merge applies operator overrides onto the table in place. Keys are
// normalized (trimmed, lower-cased). Existing entries are replaced.
func (t *EnergyTable) Merge(overrides map[string]EnergyFactor) {
	if t == nil || len(overrides) == 0 {
		return
	}
	if t.factors == nil {
		t.factors = make(map[string]EnergyFactor, len(overrides))
	}
	for name, f := range overrides {
		key := normalizeModel(name)
		if key == "" {
			continue
		}
		t.factors[key] = f
	}
}

// Lookup returns the energy factor for model and whether it is known. Unknown
// models return ok=false; callers MUST omit the estimate in that case.
func (t *EnergyTable) Lookup(model string) (EnergyFactor, bool) {
	if t == nil {
		return EnergyFactor{}, false
	}
	f, ok := t.factors[normalizeModel(model)]
	return f, ok
}

// EnergyWh returns the estimated watt-hours for totalTokens under model's
// factor, and whether a factor was known. Unknown model ⇒ ok=false, wh=0
// (callers omit it). A known model with zero tokens yields wh=0, ok=true.
func (t *EnergyTable) EnergyWh(model string, totalTokens int64) (float64, bool) {
	f, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	return (float64(totalTokens) / 1000.0) * f.WhPer1K, true
}

// CarbonModel pairs an energy table with a grid carbon-intensity factor and the
// opt-in toggle. It is the single object the retrieval service consults to
// estimate energy/CO2e for a query. A nil/disabled model omits all estimates.
type CarbonModel struct {
	enabled bool
	energy  *EnergyTable
	// gridGramsPerWh is grams of CO2e per watt-hour. When <= 0 the CO2e
	// estimate is omitted but the Wh estimate is still surfaced.
	gridGramsPerWh float64
}

// NewCarbonModel builds a CarbonModel. When enabled is false the returned model
// reports Enabled()==false and produces no estimates. energyOverrides are
// merged onto the default energy table. gridGramsPerWh <= 0 disables the CO2e
// estimate (Wh is still produced). Pass a negative grid value to mean "unset";
// callers wanting the built-in default should pass DefaultGridIntensityGramsPerWh.
func NewCarbonModel(enabled bool, energyOverrides map[string]EnergyFactor, gridGramsPerWh float64) *CarbonModel {
	return &CarbonModel{
		enabled:        enabled,
		energy:         NewEnergyTable(energyOverrides),
		gridGramsPerWh: gridGramsPerWh,
	}
}

// Enabled reports whether carbon estimation is on. A nil model is disabled.
func (c *CarbonModel) Enabled() bool {
	return c != nil && c.enabled
}

// EstimateWh returns the estimated watt-hours for totalTokens under model, and
// whether an estimate is available (enabled AND model known). A disabled model
// always returns ok=false.
func (c *CarbonModel) EstimateWh(model string, totalTokens int64) (float64, bool) {
	if !c.Enabled() {
		return 0, false
	}
	return c.energy.EnergyWh(model, totalTokens)
}

// EstimateCO2eGrams converts a watt-hour estimate into grams of CO2e using the
// configured grid intensity, returning ok=false when carbon estimation is
// disabled or no positive grid factor is configured.
func (c *CarbonModel) EstimateCO2eGrams(wh float64) (float64, bool) {
	if !c.Enabled() || c.gridGramsPerWh <= 0 {
		return 0, false
	}
	return wh * c.gridGramsPerWh, true
}
