package quality

// Config bundles the configuration for every detector. Callers should
// generally start from [DefaultConfig] and disable detectors selectively,
// because [Config.WithDefaults] fills only unset thresholds and does not
// force Enabled flags on.
//
// Every threshold is exposed as a per-detector knob so a caller can tune the
// gate for its corpus (e.g. a speech-dense corpus may enable the duration
// density floor, while a music/podcast corpus leaves it off). Wiring these
// knobs from the user-facing config file lives outside this package; the gate
// only accepts an already-resolved Config.
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

// RepetitionConfig configures the repetition-loop detector. Detection is
// script-agnostic: it measures how much of the (whitespace-normalized) rune
// stream is explained by a single short repeating period, so it catches
// space-delimited loops ("thank you thank you …") and space-free CJK loops
// ("感谢观看感谢观看 …") uniformly.
type RepetitionConfig struct {
	Enabled bool
	// MaxRepeatFraction is the largest share of the content that may be
	// explained by a single dominant repeating period before the gate trips.
	MaxRepeatFraction float64
	// MaxPeriod bounds the longest repeating unit (in runes) considered. It
	// keeps detection O(runes × MaxPeriod) on long inputs.
	MaxPeriod int
	// MinRunes is the minimum normalized rune length required before the
	// detector inspects the content; shorter inputs are skipped as too small
	// to judge. This replaces whitespace word counting, which was blind to
	// scripts without word boundaries.
	MinRunes int
}

// GibberishConfig configures the gibberish/coherence detector. Beyond the
// non-printable-rune check it applies two script-agnostic coherence signals:
// an "other character" (symbol/box-drawing) density ceiling and, for
// alphabetic scripts that distinguish vowels, a vowel floor that catches
// printable-ASCII consonant mash.
type GibberishConfig struct {
	Enabled bool
	// MaxNonPrintableFraction is the ceiling on replacement/control/private-use
	// runes (corrupted decoding).
	MaxNonPrintableFraction float64
	// MaxOtherFraction is the ceiling on the fraction of visible runes that are
	// neither letters, digits, nor punctuation (symbol soup). Natural prose in
	// any script sits well below this.
	MaxOtherFraction float64
	// MinVowelFraction is the floor on the vowel share among vowel-bearing
	// alphabetic letters (Latin/Cyrillic/Greek). Consonant mash falls below it;
	// it is set low so heavily accented real languages stay clear.
	MinVowelFraction float64
	// MinVowelSampleLetters is the minimum number of vowel-bearing letters
	// required before the vowel floor is applied, so short fragments are not
	// misjudged.
	MinVowelSampleLetters int
}

// LanguageConfig configures the language/script-mismatch detector.
type LanguageConfig struct {
	Enabled              bool
	MaxOffScriptFraction float64
}

// DensityConfig configures the low-text-density detector. The duration-based
// floor (MinCharsPerMinute) is disabled by default: a sparse transcript is a
// legitimate outcome for music or long silences, so density alone is not
// treated as a failure. The segment-ratio check remains the clearer signal of
// dropped content and stays on by default. Operators who know a corpus is
// speech-dense can set MinCharsPerMinute > 0 to re-enable the floor.
type DensityConfig struct {
	Enabled           bool
	MinCharsPerMinute float64
	MinSegmentRatio   float64
}

// Default threshold values, exported as named constants so the reasoning
// behind each is documented in one place.
const (
	defaultEmptyMinChars = 1

	defaultRepetitionMaxFraction = 0.50
	defaultRepetitionMaxPeriod   = 64
	defaultRepetitionMinRunes    = 24

	defaultGibberishMaxNonPrintable = 0.20
	defaultGibberishMaxOther        = 0.40
	defaultGibberishMinVowel        = 0.08
	defaultGibberishMinVowelSample  = 16
	defaultLanguageMaxOffScript     = 0.50
	defaultDensityMinSegmentRatio   = 1.0 / 3.0
	defaultDensityMinCharsPerMinute = 0.0 // 0 => duration floor disabled.
)

// DefaultConfig returns the recommended configuration with every detector
// enabled and conservative thresholds.
func DefaultConfig() Config {
	return Config{
		Empty: EmptyConfig{
			Enabled:  true,
			MinChars: defaultEmptyMinChars,
		},
		Repetition: RepetitionConfig{
			Enabled:           true,
			MaxRepeatFraction: defaultRepetitionMaxFraction,
			MaxPeriod:         defaultRepetitionMaxPeriod,
			MinRunes:          defaultRepetitionMinRunes,
		},
		Gibberish: GibberishConfig{
			Enabled:                 true,
			MaxNonPrintableFraction: defaultGibberishMaxNonPrintable,
			MaxOtherFraction:        defaultGibberishMaxOther,
			MinVowelFraction:        defaultGibberishMinVowel,
			MinVowelSampleLetters:   defaultGibberishMinVowelSample,
		},
		Language: LanguageConfig{
			Enabled:              true,
			MaxOffScriptFraction: defaultLanguageMaxOffScript,
		},
		Density: DensityConfig{
			Enabled:           true,
			MinCharsPerMinute: defaultDensityMinCharsPerMinute,
			MinSegmentRatio:   defaultDensityMinSegmentRatio,
		},
	}
}

// WithDefaults returns a copy of c with any unset or invalid (<= 0)
// threshold filled in from [DefaultConfig]. Enabled flags are preserved
// exactly as set; callers should start from DefaultConfig and disable
// detectors selectively, since WithDefaults does not force Enabled=true.
//
// MinCharsPerMinute is intentionally not filled: 0 is a meaningful value
// (duration floor disabled) rather than "unset".
func (c Config) WithDefaults() Config {
	d := DefaultConfig()

	if c.Empty.MinChars <= 0 {
		c.Empty.MinChars = d.Empty.MinChars
	}

	if c.Repetition.MaxRepeatFraction <= 0 {
		c.Repetition.MaxRepeatFraction = d.Repetition.MaxRepeatFraction
	}
	if c.Repetition.MaxPeriod <= 0 {
		c.Repetition.MaxPeriod = d.Repetition.MaxPeriod
	}
	if c.Repetition.MinRunes <= 0 {
		c.Repetition.MinRunes = d.Repetition.MinRunes
	}

	if c.Gibberish.MaxNonPrintableFraction <= 0 {
		c.Gibberish.MaxNonPrintableFraction = d.Gibberish.MaxNonPrintableFraction
	}
	if c.Gibberish.MaxOtherFraction <= 0 {
		c.Gibberish.MaxOtherFraction = d.Gibberish.MaxOtherFraction
	}
	if c.Gibberish.MinVowelFraction <= 0 {
		c.Gibberish.MinVowelFraction = d.Gibberish.MinVowelFraction
	}
	if c.Gibberish.MinVowelSampleLetters <= 0 {
		c.Gibberish.MinVowelSampleLetters = d.Gibberish.MinVowelSampleLetters
	}

	if c.Language.MaxOffScriptFraction <= 0 {
		c.Language.MaxOffScriptFraction = d.Language.MaxOffScriptFraction
	}

	if c.Density.MinSegmentRatio <= 0 {
		c.Density.MinSegmentRatio = d.Density.MinSegmentRatio
	}

	return c
}
