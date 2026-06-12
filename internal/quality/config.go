package quality

// Config bundles the configuration for every detector. Callers should
// generally start from [DefaultConfig] and disable detectors selectively,
// because [Config.WithDefaults] fills only unset thresholds and does not
// force Enabled flags on.
type Config struct {
	Empty      EmptyConfig
	Repetition RepetitionConfig
	Gibberish  GibberishConfig
	Language   LanguageConfig
	Density    DensityConfig
}

// EmptyConfig configures the empty-output detector.
type EmptyConfig struct {
	Enabled  bool
	MinChars int
}

// RepetitionConfig configures the repetition-loop detector.
type RepetitionConfig struct {
	Enabled           bool
	NGram             int
	MaxRepeatFraction float64
	MinWords          int
}

// GibberishConfig configures the gibberish (non-printable) detector.
type GibberishConfig struct {
	Enabled                 bool
	MaxNonPrintableFraction float64
}

// LanguageConfig configures the language/script-mismatch detector.
type LanguageConfig struct {
	Enabled              bool
	MaxOffScriptFraction float64
}

// DensityConfig configures the low-text-density detector.
type DensityConfig struct {
	Enabled           bool
	MinCharsPerMinute float64
	MinSegmentRatio   float64
}

// DefaultConfig returns the recommended configuration with every detector
// enabled and conservative thresholds.
func DefaultConfig() Config {
	return Config{
		Empty: EmptyConfig{
			Enabled:  true,
			MinChars: 1,
		},
		Repetition: RepetitionConfig{
			Enabled:           true,
			NGram:             3,
			MaxRepeatFraction: 0.30,
			MinWords:          30,
		},
		Gibberish: GibberishConfig{
			Enabled:                 true,
			MaxNonPrintableFraction: 0.20,
		},
		Language: LanguageConfig{
			Enabled:              true,
			MaxOffScriptFraction: 0.50,
		},
		Density: DensityConfig{
			Enabled:           true,
			MinCharsPerMinute: 30,
			MinSegmentRatio:   1.0 / 3.0,
		},
	}
}

// WithDefaults returns a copy of c with any unset or invalid (<= 0)
// threshold filled in from [DefaultConfig]. Enabled flags are preserved
// exactly as set; callers should start from DefaultConfig and disable
// detectors selectively, since WithDefaults does not force Enabled=true.
func (c Config) WithDefaults() Config {
	d := DefaultConfig()

	if c.Empty.MinChars <= 0 {
		c.Empty.MinChars = d.Empty.MinChars
	}

	if c.Repetition.NGram <= 0 {
		c.Repetition.NGram = d.Repetition.NGram
	}
	if c.Repetition.MaxRepeatFraction <= 0 {
		c.Repetition.MaxRepeatFraction = d.Repetition.MaxRepeatFraction
	}
	if c.Repetition.MinWords <= 0 {
		c.Repetition.MinWords = d.Repetition.MinWords
	}

	if c.Gibberish.MaxNonPrintableFraction <= 0 {
		c.Gibberish.MaxNonPrintableFraction = d.Gibberish.MaxNonPrintableFraction
	}

	if c.Language.MaxOffScriptFraction <= 0 {
		c.Language.MaxOffScriptFraction = d.Language.MaxOffScriptFraction
	}

	if c.Density.MinCharsPerMinute <= 0 {
		c.Density.MinCharsPerMinute = d.Density.MinCharsPerMinute
	}
	if c.Density.MinSegmentRatio <= 0 {
		c.Density.MinSegmentRatio = d.Density.MinSegmentRatio
	}

	return c
}
