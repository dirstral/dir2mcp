package model

// This file holds the §8.6.13 transcript coverage record. It lives in model, not
// in ingest, because two packages below ingest need to read it: the store, which
// aggregates it for the §7.7 report (#972), and anything else that reads a
// transcript representation's meta_json. ingest keeps type aliases, so its own
// references and its constructor are unchanged.

// TranscriptCoverage records WHICH PART of a recording a windowed decode actually
// produced text for (SPEC §8.6.13, issue #961).
//
// A recording too long or too large for one STT request is decoded in several
// overlapping windows and merged into one transcript. Windows fail
// independently: a provider timeout, a cancelled context, a cut ffmpeg refuses.
// Before this type the merged transcript carried no trace of that, so a
// 73-minute recording that decoded one window of eight was indexed exactly like
// a complete one: status ok, chunks searchable, and nothing anywhere saying
// that 88% of the audio was never transcribed. An editor then reads "not found"
// for minute eleven onward and cannot tell it from "not said".
//
// It is recorded on the transcript representation's meta_json ONLY for a decode
// of two or more windows. A single-request decode records nothing, and per
// §5.2 absence means "no assertion", never "complete".
type TranscriptCoverage struct {
	// WindowsAttempted is how many windows the decode scheduled (>= 2 whenever
	// this value is recorded at all).
	WindowsAttempted int `json:"windows_attempted"`
	// WindowsDecoded is how many of them yielded transcript content.
	WindowsDecoded int `json:"windows_decoded"`
	// DecodedMS is the summed length of Ranges: how much of the recording the
	// transcript actually covers.
	DecodedMS int `json:"decoded_ms"`
	// DurationMS is the recording's length. 0 when the duration probe failed, in
	// which case Fraction falls back to the window counts.
	DurationMS int `json:"duration_ms"`
	// Ranges are the decoded stretches in ABSOLUTE recording time, coalesced,
	// non-overlapping and ascending (§8.6.13), so a consumer reads the gaps
	// directly instead of reconstructing them from window arithmetic.
	Ranges []CoverageRange `json:"ranges,omitempty"`
	// Languages is the per-window language record of SPEC §8.2.2, recorded ONLY
	// under media.stt.language_scope: window and REQUIRED there: which stretch of
	// the recording resolved to which language, which STT profile decoded it, and
	// whether that profile declares the language. Entries are in absolute
	// recording time, coalesced over adjacent windows whose (language,
	// language_source, route, covered) are identical, non-overlapping and
	// ascending. Absent under the default item scope, so an existing corpus's
	// coverage object is byte-for-byte unchanged.
	Languages []CoverageLanguage `json:"languages,omitempty"`
	// Refused lists the windows that were NOT decoded by decision (§8.2.2): an
	// uncovered language under on_uncovered_language=skip, or a per-window
	// quality-gate failure. A refused range is never inside Ranges. Absent when
	// nothing was refused. A window that failed for a provider or transport
	// reason is a failed window, counted in WindowsAttempted, not a refused one.
	Refused []RefusedRange `json:"refused,omitempty"`
	// Identity is the §8.6.7 "provider/model" that produced this transcript. It
	// is NOT part of the persisted coverage object; a reader fills it from the
	// sibling fields of the same meta_json so a §7.7 report can name what
	// decoded the recording without carrying the whole meta around.
	Identity string `json:"-"`
	// RefusedQualityReason is the §8.6.6 reason of the FIRST window refused as
	// quality_gate (§8.2.2), for the log line and the TRANSCRIBE_FAILED message
	// when every window was refused that way. Transient: the persisted record
	// carries only coverage.refused[].reason, as the spec defines it.
	RefusedQualityReason string `json:"-"`
}

// CoverageRange is one decoded stretch of a recording, in absolute milliseconds
// from the start of the whole media, end-exclusive.
type CoverageRange struct {
	StartMS int `json:"start_ms"`
	EndMS   int `json:"end_ms"`
}

// CoverageLanguage is one entry of TranscriptCoverage.Languages (SPEC §8.2.2):
// a stretch of the recording, the language it resolved to, how that language
// was obtained, the STT provider profile that decoded it, and whether that
// profile declares the language. A window with NO resolvable language omits
// Language, LanguageSource and LanguageConfidence (no BCP-47 tag is invented)
// and records Covered=true, because the §8.2.1 floor does not apply when no
// language is resolved.
type CoverageLanguage struct {
	StartMS int `json:"start_ms"`
	EndMS   int `json:"end_ms"`
	// Language is the BCP-47 primary subtag, or empty for an unknown window.
	Language string `json:"language,omitempty"`
	// LanguageSource is configured, detected or inherited. "inherited" appears
	// ONLY here: a window below the confidence floor takes the preceding
	// window's language, and the representation-level language_source (§5.2)
	// never carries it.
	LanguageSource string `json:"language_source,omitempty"`
	// LanguageConfidence is the minimum detector confidence over the coalesced
	// windows when LanguageSource is detected; absent otherwise.
	LanguageConfidence *float64 `json:"language_confidence,omitempty"`
	// Route names the STT provider profile that decoded the range.
	Route string `json:"route"`
	// Covered is false when the route's declared stt_languages omit Language
	// (the §8.2.1 floor tripped under warn); true otherwise.
	Covered bool `json:"covered"`
}

// RefusedRange is one stretch of the recording that was not decoded by
// decision (SPEC §8.2.2).
type RefusedRange struct {
	StartMS int `json:"start_ms"`
	EndMS   int `json:"end_ms"`
	// Reason is RefusedLanguageUncovered or RefusedQualityGate.
	Reason string `json:"reason"`
}

// Reasons a window is refused (SPEC §8.2.2 coverage.refused[].reason).
const (
	// RefusedLanguageUncovered: the window's resolved language is outside the
	// decoding profile's declared coverage and on_uncovered_language is skip.
	RefusedLanguageUncovered = "language_uncovered"
	// RefusedQualityGate: the window decoded but failed the §8.6.6 checks run
	// per window.
	RefusedQualityGate = "quality_gate"
)

// CoverageLanguageSourceInherited is the language_source a coverage entry
// records when a window below the confidence floor took the preceding window's
// language (§8.2.2). It is scoped to coverage entries by design.
const CoverageLanguageSourceInherited = "inherited"

// Fraction is how much of the recording decoded, in [0,1]. It prefers the
// measured time (DecodedMS/DurationMS), because that is the honest quantity an
// operator cares about: eight equal windows of which one decoded is 12% of the
// audio, but a final short window skews the count. When the duration probe
// failed (DurationMS == 0) there is no time to measure against, so it degrades
// to the window counts rather than reporting a false 0.
//
// A nil coverage, or one that attempted nothing, reports 1: no windowing
// happened, so there is no partial-decode claim to make.
func (c *TranscriptCoverage) Fraction() float64 {
	if c == nil || c.WindowsAttempted <= 0 {
		return 1
	}
	if c.DurationMS > 0 {
		f := float64(c.DecodedMS) / float64(c.DurationMS)
		if f > 1 {
			// Overlapping windows are coalesced before they are summed, so this can
			// only come from a duration probe that under-reports the media. Clamp
			// rather than report more than whole.
			return 1
		}
		return f
	}
	return float64(c.WindowsDecoded) / float64(c.WindowsAttempted)
}

// Complete reports whether the transcript covers the whole recording. It is the
// positive statement §8.6.13 requires a fully decoded multi-window transcript to
// make, so that absence of the whole object keeps its §5.2 "no assertion" meaning.
//
// With a known duration it asks the measured question ("does the decoded time
// reach the end of the recording?") rather than the bookkeeping one ("did every
// window I scheduled come back?"). The two normally agree, because the scheduled
// windows tile the recording. When they disagree the measured answer is the one
// that matters: a caller uses this to decide whether to ANNOUNCE a gap, and a gap
// that the window counts cannot see is exactly the gap worth announcing.
func (c *TranscriptCoverage) Complete() bool {
	if c == nil {
		return true
	}
	if c.DurationMS > 0 {
		return c.DecodedMS >= c.DurationMS
	}
	return c.WindowsDecoded >= c.WindowsAttempted
}

// TranscriptCoverageSummary is the corpus-level answer to "how much speech is
// this corpus missing?" (SPEC §7.7, issue #972). It sums only the transcripts
// whose coverage record does not state completeness.
type TranscriptCoverageSummary struct {
	// Transcripts is how many live transcripts record incomplete coverage.
	Transcripts int64 `json:"transcripts"`
	// DecodedMS and DurationMS are summed over those transcripts that KNOW their
	// recording's length, so the pair is a like-for-like ratio. Both are
	// coverage.* values; a transcript meta_json also carries a top-level
	// duration_ms, which is the media's length and not a decode result.
	DecodedMS  int64 `json:"decoded_ms"`
	DurationMS int64 `json:"duration_ms"`
	// UnknownDuration is how many of Transcripts had no usable duration and are
	// therefore absent from DecodedMS/DurationMS. Reported rather than summed as
	// zero: an unknown length counted as no shortfall is the silence §7.7
	// forbids.
	UnknownDuration int64 `json:"unknown_duration"`
	// Providers are the distinct "provider/model" identities recorded on those
	// transcripts (§8.6.7), sorted. This is what a §7.7 remediation may name: the
	// ENDPOINT that served a window is not recorded, and one provider may be
	// routed to several, so a report that named one would be guessing.
	Providers []string `json:"providers,omitempty"`
	// NoAssertion is how many live DECODED transcripts carry no coverage record
	// at all: a single-request decode records none (§8.6.13), and neither does
	// any transcript indexed before the record existed. §5.2 makes that "no
	// assertion", so it is reported as its own number rather than folded into
	// the clean count, where it would be indistinguishable from a corpus that
	// really is whole (#977).
	NoAssertion int64 `json:"no_assertion,omitempty"`
}

// Partial reports whether anything was found. A summary with no partial
// transcript is still reported, positively, because an omitted line and a clean
// corpus read identically (§7.7).
func (s TranscriptCoverageSummary) Partial() bool { return s.Transcripts > 0 }

// MissingMS is the audio those transcripts never decoded, over the ones whose
// duration is known. Never negative: coalesced ranges cannot exceed the
// recording, and a duration probe that under-reports is clamped here rather than
// reported as negative missing time.
func (s TranscriptCoverageSummary) MissingMS() int64 {
	if s.DurationMS <= s.DecodedMS {
		return 0
	}
	return s.DurationMS - s.DecodedMS
}
