package ingest

import (
	"sort"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TranscriptCoverage and CoverageRange are the §8.6.13 coverage record. They
// live in internal/model so the store can aggregate them for the §7.7 report
// without importing ingest (#972); these aliases keep every existing reference
// in this package, and ingest's public surface, exactly as they were.
type TranscriptCoverage = model.TranscriptCoverage

// CoverageRange is one decoded stretch of a recording. See model.CoverageRange.
type CoverageRange = model.CoverageRange

// coalesceCoverageRanges sorts ranges by start and merges every overlapping or
// adjacent pair, returning non-overlapping ascending ranges (§8.6.13). Decode
// windows overlap by design (TranscriptWindowOverlapMS), so without this a fully
// decoded recording would report more decoded milliseconds than it is long.
// Empty and inverted ranges are dropped.
func coalesceCoverageRanges(ranges []CoverageRange) []CoverageRange {
	cleaned := make([]CoverageRange, 0, len(ranges))
	for _, r := range ranges {
		if r.EndMS > r.StartMS {
			cleaned = append(cleaned, r)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	sort.Slice(cleaned, func(a, b int) bool {
		if cleaned[a].StartMS != cleaned[b].StartMS {
			return cleaned[a].StartMS < cleaned[b].StartMS
		}
		return cleaned[a].EndMS < cleaned[b].EndMS
	})
	out := []CoverageRange{cleaned[0]}
	for _, r := range cleaned[1:] {
		last := &out[len(out)-1]
		// `>=` merges a range that merely TOUCHES the previous one: two windows
		// scheduled back to back cover one continuous stretch, and reporting it as
		// two ranges would read as a gap of zero milliseconds.
		if r.StartMS >= last.StartMS && r.StartMS <= last.EndMS {
			if r.EndMS > last.EndMS {
				last.EndMS = r.EndMS
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// newScopedTranscriptCoverage builds the coverage record of a decode run under
// media.stt.language_scope: window (SPEC §8.2.2). Unlike newTranscriptCoverage it
// records for ANY window count, including one: coverage.languages is required
// under window scope whatever the count, and a one-window decode whose window
// was refused must still say so. The per-piece language entries are coalesced on
// (language, language_source, route, covered) and the refused ranges on reason.
func newScopedTranscriptCoverage(stats windowStats, totalMS int) *TranscriptCoverage {
	base := newTranscriptCoverage(stats.attempted, stats.decoded, totalMS, stats.ranges)
	if base == nil {
		base = newTranscriptCoverage(2, stats.decoded, totalMS, stats.ranges)
		base.WindowsAttempted = stats.attempted
	}
	base.Languages = coalesceCoverageLanguages(stats.languages, totalMS)
	base.Refused = coalesceRefusedRanges(stats.refused, totalMS)
	return base
}

// newTranscriptCoverage builds the §8.6.13 coverage record from a windowed
// decode's raw per-piece ranges. It coalesces the ranges, clamps them to the
// recording, and sums what is left.
//
// It returns nil when fewer than two windows were attempted: §8.6.13 scopes the
// record to a MULTI-window decode, and recording it for a single request would
// turn "no assertion" into a claim about a decode that was never windowed.
func newTranscriptCoverage(attempted, decoded, totalMS int, ranges []CoverageRange) *TranscriptCoverage {
	if attempted < 2 {
		return nil
	}
	if totalMS > 0 {
		clamped := make([]CoverageRange, 0, len(ranges))
		for _, r := range ranges {
			if r.StartMS < 0 {
				r.StartMS = 0
			}
			if r.EndMS > totalMS {
				r.EndMS = totalMS
			}
			clamped = append(clamped, r)
		}
		ranges = clamped
	}
	merged := coalesceCoverageRanges(ranges)
	decodedMS := 0
	for _, r := range merged {
		decodedMS += r.EndMS - r.StartMS
	}
	return &TranscriptCoverage{
		WindowsAttempted: attempted,
		WindowsDecoded:   decoded,
		DecodedMS:        decodedMS,
		DurationMS:       totalMS,
		Ranges:           merged,
	}
}
