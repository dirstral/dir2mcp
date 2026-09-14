package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// transcriptCoverage is the SPEC §7.7 honest-coverage verdict for SPEECH: how
// much of the corpus was never HEARD, as opposed to never read (issue #972).
//
// It is computed once and rendered by the same two surfaces the extraction
// verdict uses, the `dir2mcp up` banner and a doctor check, so the two can never
// disagree about what the record holds.
//
// The gap it closes is created by a DEFAULT. #961 made a windowed decode record
// which part of a recording it produced text for, per representation, and
// `media.stt.on_partial_transcript` defaults to `warn`, which persists the
// partial transcript and leaves its document `status=ok`. So the shortfall
// reaches no other report: `skip_reasons` sees only the `skip` path, the
// extraction verdict is scoped to format classes, and `status` says `ok`. A
// corpus can be 100% indexed, report no skips and no errors, and still be
// missing hours of speech.
type transcriptCoverage struct {
	model.TranscriptCoverageSummary
	// Remedy names what to do. Empty when nothing is partial.
	Remedy string
}

// partialTranscriptCounter is the store capability the verdict reads. A store
// that does not implement it (a test fake, a non-SQLite backend) yields no
// section rather than an error.
type partialTranscriptCounter interface {
	PartialTranscriptCoverage(ctx context.Context) (model.TranscriptCoverageSummary, error)
}

// computeTranscriptCoverage derives the §7.7 speech verdict from the durable
// record and attaches the remediation.
func computeTranscriptCoverage(ctx context.Context, counter partialTranscriptCounter, cfg config.Config) (transcriptCoverage, error) {
	summary, err := counter.PartialTranscriptCoverage(ctx)
	if err != nil {
		return transcriptCoverage{}, err
	}
	cov := transcriptCoverage{TranscriptCoverageSummary: summary}
	if !summary.Partial() {
		return cov, nil
	}
	cov.Remedy = transcriptCoverageRemedy(cfg, summary)
	return cov, nil
}

// transcriptCoverageRemedy names an action that actually re-decodes.
//
// "Run a reindex" is NOT one, and §7.7 requires the difference to be stated.
// §8.6.13 keys the transcript cache on the media bytes folded with the STT
// derivation identity, and requires a cache hit to restore the recorded coverage
// along with the text. An operator who repairs a down endpoint changes neither
// key component, so an ordinary reindex returns the same partial transcript,
// reports the same shortfall, and never calls the provider.
//
// `--redecode-partial-transcripts` is the action that does work (#974): it
// ignores the cached transcript of exactly the recordings named here.
func transcriptCoverageRemedy(_ config.Config, summary model.TranscriptCoverageSummary) string {
	var b strings.Builder
	if len(summary.Providers) > 0 {
		// Named, not guessed: the record holds provider and model, never the
		// endpoint that served a given window.
		fmt.Fprintf(&b, "Decoded by %s. ", strings.Join(summary.Providers, ", "))
	}
	b.WriteString("Check that provider's endpoint, then run `dir2mcp reindex --redecode-partial-transcripts`. ")
	b.WriteString("A plain reindex re-decodes nothing here: the transcript cache is keyed on the media bytes ")
	b.WriteString("and the provider/model, and a repaired endpoint changes neither, so the same partial transcript comes back.")
	return b.String()
}

// Summary renders the verdict as one sentence for a check detail. It is written
// for the clean case too: §7.7 requires a corpus with no partial transcript to
// be reported POSITIVELY, because an omitted line and a clean corpus read
// identically to the operator deciding whether to trust a search result.
func (c transcriptCoverage) Summary() string {
	if !c.Partial() {
		return "no transcript records an incomplete decode" + c.noAssertionClause()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d transcript(s) record an incomplete decode", c.Transcripts)
	if c.DurationMS > 0 {
		fmt.Fprintf(&b, "; %s of %s of audio decoded (%s never heard)",
			humanDuration(c.DecodedMS), humanDuration(c.DurationMS), humanDuration(c.MissingMS()))
	}
	if c.UnknownDuration > 0 {
		// Reported, never summed as zero: an unknown length counted as no
		// shortfall is the silence §7.7 forbids.
		fmt.Fprintf(&b, "; %d of them have no known duration and are not in that total", c.UnknownDuration)
	}
	b.WriteString(c.noAssertionClause())
	return b.String()
}

// noAssertionClause names the transcripts that say NOTHING about their coverage,
// and is empty when there are none (#977).
//
// Without it, a corpus holding no coverage records at all gets the same sentence
// as a corpus that is genuinely whole: "no transcript records an incomplete
// decode". Every corpus indexed before §8.6.13 existed is in that state, and on
// the RFE validation corpus it meant a clean-looking verdict over 34 transcripts
// of which 19 recordings were large enough to be windowed today.
//
// It does not say how many of those sit on media that WOULD be windowed now.
// That number is derivable from `documents.size_bytes` and it is the actionable
// half, but it is a guess about the CURRENT config inside a report whose whole
// value is that it states only what the record holds. The operator who wants
// certainty has `--redecode-partial-transcripts`.
func (c transcriptCoverage) noAssertionClause() string {
	if c.NoAssertion <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"; %d transcript(s) assert nothing about coverage (a single-request decode "+
			"records none, and neither does one indexed before the record existed)",
		c.NoAssertion)
}

// humanDuration renders milliseconds as "1h 3m", "3m 20s" or "12s". Whole units
// only: this is a coverage report, and a second of precision on three hours of
// audio would suggest a measurement the windows cannot support.
func humanDuration(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	seconds := ms / 1000
	if ms > 0 && seconds == 0 {
		// A shortfall under a second is still a shortfall, and "0s never heard"
		// is the exact silence this report exists to remove: it reads as nothing
		// missing. Windows are minutes long, so this is the rounding edge rather
		// than a case an operator meets often, which is why it has to be right
		// rather than argued about.
		return "<1s"
	}
	switch {
	case seconds >= 3600:
		return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
	case seconds >= 60:
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

// startupTranscriptCoverage computes the verdict for the `up` banner, on the
// same terms as startupExtractionCoverage: silent where no banner is printed
// (--json, --quiet, a daemon child) and on a store that cannot answer, and loud
// on a read failure, because a coverage report that fails quietly is the silence
// §7.7 forbids.
func (a *App) startupTranscriptCoverage(ctx context.Context, st interface{}, cfg config.Config, opts upOptions, stderr io.Writer) transcriptCoverage {
	if opts.jsonOutput || opts.quiet || a.isDaemonChild() {
		return transcriptCoverage{}
	}
	counter, ok := st.(partialTranscriptCounter)
	if !ok {
		return transcriptCoverage{}
	}
	cov, err := computeTranscriptCoverage(ctx, counter, cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: transcript coverage unavailable: %v\n", err)
		return transcriptCoverage{}
	}
	return cov
}

// printTranscriptCoverageSection emits the banner's speech-coverage lines.
//
// Silent when nothing is partial, which is where this differs from the doctor
// check on purpose. The banner prints on every start and a line saying "no
// transcript records an incomplete decode" on every start is noise that trains
// an operator to skip the section. `doctor` is the surface that is ASKED the
// question, so that is where §7.7's positive statement belongs.
func printTranscriptCoverageSection(out io.Writer, s styles, cov transcriptCoverage) {
	// Deliberately keyed on Partial, not on the summary being non-empty: the
	// #977 no-assertion count is not a defect, and a section that appeared on
	// every start of every pre-§8.6.13 corpus would be noise an operator learns
	// to skip. doctor is the surface that is ASKED, and it carries the clause.
	if !cov.Partial() {
		return
	}
	writef(out, "  %s\n", s.sectionHeader("Speech coverage"))
	writeln(out, s.kv("Partial", cov.Summary()))
	writeln(out, s.kv("Fix", cov.Remedy))
	writeln(out)
}

// StartupTranscriptCoverageForTest exposes the banner's probe to the external
// `tests/cli` package, which cannot call an unexported method. It returns how
// many partial transcripts the verdict found; 0 means the probe was skipped or
// found nothing, and whether the store was queried at all is observable on the
// caller's counter.
func (a *App) StartupTranscriptCoverageForTest(ctx context.Context, st interface{}, cfg config.Config, jsonOutput, quiet bool, stderr io.Writer) int64 {
	return a.startupTranscriptCoverage(ctx, st, cfg,
		upOptions{globalOptions: globalOptions{jsonOutput: jsonOutput, quiet: quiet}}, stderr).Transcripts
}

// startupCoverage is the pair of §7.7 verdicts the `up` banner prints: what the
// corpus could not READ, and what it never HEARD. They travel together because
// they are computed at the same moment, from the same store, for the same
// banner, and a second parameter threaded through the same four signatures
// would only invite the two to drift apart.
type startupCoverage struct {
	Extraction extractionCoverage
	Transcript transcriptCoverage
}
