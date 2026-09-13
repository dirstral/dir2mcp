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
}

// CoverageRange is one decoded stretch of a recording, in absolute milliseconds
// from the start of the whole media, end-exclusive.
type CoverageRange struct {
	StartMS int `json:"start_ms"`
	EndMS   int `json:"end_ms"`
}

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
