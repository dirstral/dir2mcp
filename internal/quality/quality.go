// Package quality provides reusable, content-only gates that detect
// degenerate or hallucinated STT/OCR/translation output before it is
// chunked and embedded.
//
// All detectors are pure functions over text plus a small [Context]; they
// have no dependencies on store, ingest, or embeddings. A detector that
// lacks the input it needs (for example a language detector without an
// expected language) reports nothing rather than guessing.
package quality

import "time"

// Modality identifies the kind of content being inspected. It is advisory:
// detectors do not currently branch on it, but it is carried for detail
// strings and future use.
type Modality string

// Supported content modalities.
const (
	ModalityTranscript Modality = "transcript"
	ModalityOCR        Modality = "ocr"
	ModalityText       Modality = "text"
)

// Context carries optional signals about the content under inspection.
// Every field is optional; a detector that lacks its required input
// reports nothing.
type Context struct {
	// Modality is the kind of content being inspected.
	Modality Modality
	// Duration is the source media duration, used for density checks.
	Duration time.Duration
	// ExpectedLanguage is a BCP-47-ish language tag (e.g. "en", "ru",
	// "pt-BR"); only the primary subtag is used.
	ExpectedLanguage string
	// SourceSegmentCount is the number of source segments (e.g. STT
	// segments) the output is expected to roughly reflect.
	SourceSegmentCount int
}

// Reason is a stable machine-readable identifier for why a [Finding] was
// raised.
type Reason string

// Known finding reasons.
const (
	ReasonRepetitionLoop   Reason = "repetition_loop"
	ReasonEmptyOutput      Reason = "empty_output"
	ReasonLanguageMismatch Reason = "language_mismatch"
	ReasonLowDensity       Reason = "low_text_density"
	ReasonGibberish        Reason = "gibberish_ratio"
)

// Finding describes a single quality problem detected in the content.
type Finding struct {
	// Reason is the stable machine-readable cause.
	Reason Reason
	// Detail is a human-readable explanation; it may include excerpts.
	Detail string
	// Score is a detector-specific magnitude (typically a fraction in
	// [0,1]) describing how strongly the gate tripped.
	Score float64
}

// Detector inspects text and optionally reports a single [Finding].
// Implementations must be pure and side-effect free.
type Detector interface {
	// Name returns a short stable identifier for the detector.
	Name() string
	// Inspect returns a non-nil Finding if the content trips the gate,
	// or nil otherwise.
	Inspect(text string, ctx Context) *Finding
}

// Verdict is the aggregate result of running a [Gate].
type Verdict struct {
	// Findings holds every non-nil finding, in detector (severity) order.
	Findings []Finding
}

// OK reports whether no findings were raised.
func (v Verdict) OK() bool { return len(v.Findings) == 0 }

// Primary returns the highest-severity finding, or nil if the verdict is
// clean. Findings are ordered by severity at evaluation time.
func (v Verdict) Primary() *Finding {
	if len(v.Findings) == 0 {
		return nil
	}
	return &v.Findings[0]
}

// Gate runs an ordered set of detectors over content.
type Gate struct {
	detectors []Detector
}

// New builds a Gate from cfg. Defaults are filled via [Config.WithDefaults],
// and detectors are appended in severity order (empty, repetition,
// gibberish, language, density). Each detector is included only if its
// corresponding Enabled flag is set.
func New(cfg Config) *Gate {
	cfg = cfg.WithDefaults()
	g := &Gate{}
	if cfg.Empty.Enabled {
		g.detectors = append(g.detectors, emptyDetector{minChars: cfg.Empty.MinChars})
	}
	if cfg.Repetition.Enabled {
		g.detectors = append(g.detectors, repetitionDetector{
			n:        cfg.Repetition.NGram,
			maxFrac:  cfg.Repetition.MaxRepeatFraction,
			minWords: cfg.Repetition.MinWords,
		})
	}
	if cfg.Gibberish.Enabled {
		g.detectors = append(g.detectors, gibberishDetector{
			maxNonPrintable: cfg.Gibberish.MaxNonPrintableFraction,
		})
	}
	if cfg.Language.Enabled {
		g.detectors = append(g.detectors, languageDetector{
			maxOffScript: cfg.Language.MaxOffScriptFraction,
		})
	}
	if cfg.Density.Enabled {
		g.detectors = append(g.detectors, densityDetector{
			minCharsPerMinute: cfg.Density.MinCharsPerMinute,
			minSegmentRatio:   cfg.Density.MinSegmentRatio,
		})
	}
	return g
}

// Evaluate runs every detector over text and collects all non-nil findings.
func (g *Gate) Evaluate(text string, ctx Context) Verdict {
	var v Verdict
	for _, d := range g.detectors {
		if f := d.Inspect(text, ctx); f != nil {
			v.Findings = append(v.Findings, *f)
		}
	}
	return v
}
