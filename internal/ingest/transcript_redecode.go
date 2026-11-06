package ingest

import (
	"context"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// This file implements the §8.6.13 low-confidence re-decode (spec 0.88.0,
// dir2mcp #1143). Under media.stt.language_scope: item, with no pin and no
// identifier hint, each window is decoded with no language hint. The decoder
// then picks the language from the start of the window, so a window that opens
// with a jingle in another language can come out whole in that language, at a
// low confidence. After the item language L is resolved (§8.8), a window that
// the decoder reported in another language below media.stt.redecode_below,
// while another window reported L at or above it, is decoded once more with L
// as the request language.
//
// The rule and the key do not join the derivation identity (§8.6.7), so an
// upgrade does not decode every stored transcript again.

// windowReport is the decoder's language report for one decoded window under
// item scope: the scheduled window span, the first language a piece reported
// (a BCP-47 primary subtag, empty when none), the lowest confidence a piece
// reported (0 when none), and where the window's pieces sit in the decoded
// windows slice.
type windowReport struct {
	span  CoverageRange
	lang  string
	conf  float64
	first int
	count int
}

// decoderReport reads the language and the confidence the decoder reported
// for a window from its decoded pieces: the first piece that names a language
// names the window, and the lowest reported confidence is the window's. A
// confidence of 0 means no confidence was reported.
func decoderReport(results []pieceResult) (string, float64) {
	lang, conf := "", 0.0
	for _, r := range results {
		tag := provider.PrimarySubtag(r.res.Language)
		if tag == "" {
			continue
		}
		if lang == "" {
			lang = tag
		}
		if c := r.res.LanguageConfidence; c > 0 && (conf == 0 || c < conf) {
			conf = c
		}
	}
	return lang, conf
}

// redecodeOutcome is what the rule did to one transcript: the windows it
// decoded again (in ascending start order) and the rule marker. A zero value
// means the rule did not run.
type redecodeOutcome struct {
	windows []model.RedecodedWindow
	rule    string
}

// stamp records the outcome on the coverage record, which carries it to the
// transcript cache and to the meta_json.
func (o redecodeOutcome) stamp(cov *TranscriptCoverage) {
	if cov == nil || o.rule == "" {
		return
	}
	cov.RedecodeRule = o.rule
	cov.RedecodedWindows = o.windows
}

// redecodeRuleOn reports whether the §8.6.13 low-confidence rule applies to an
// unhinted decode: item scope, no operator pin, and a threshold above 0.
func (s *Service) redecodeRuleOn() bool {
	return !s.windowScoped() && provider.PrimarySubtag(s.transcriptLanguage) == "" && s.cfg.MediaSTTRedecodeBelow > 0
}

// redecodeLowConfidenceWindows applies the §8.6.13 rule to the windows of one
// item-scoped decode. It returns the windows to merge (with the replaced
// windows swapped in) and the outcome to record. The rule needs two or more
// decoded windows and a resolved item language; without them it does not run
// and records nothing.
func (s *Service) redecodeLowConfidenceWindows(ctx context.Context, relPath, tmpPath string, stt model.Transcriber, plan windowSchedule, windows []TranscriptWindow, reports []windowReport) ([]TranscriptWindow, redecodeOutcome) {
	if !s.redecodeRuleOn() || len(reports) < 2 {
		return windows, redecodeOutcome{}
	}
	text, _ := MergeTranscriptWindows(windows, plan.stepMS)
	itemLang, _, ok := s.detectLanguage(text)
	if !ok {
		return windows, redecodeOutcome{}
	}
	below := s.cfg.MediaSTTRedecodeBelow
	qualifying := qualifyingWindows(reports, itemLang, below)
	if len(qualifying) == 0 {
		return windows, redecodeOutcome{rule: model.RedecodeRuleLowConfidence}
	}
	hinted, ok := model.TranscriberForLanguage(stt, itemLang)
	if !ok {
		s.getLogger().Printf("windowed transcription %s: %d window(s) qualify for a second decode in %q, but the STT route cannot take a language hint; the first decode stands (SPEC §8.6.13)",
			relPath, len(qualifying), itemLang)
		return windows, redecodeOutcome{}
	}
	replaced := make(map[int][]TranscriptWindow, len(qualifying))
	out := redecodeOutcome{rule: model.RedecodeRuleLowConfidence}
	for _, i := range qualifying {
		r := reports[i]
		second, kept := s.redecodeWindow(ctx, relPath, tmpPath, hinted, plan, r)
		if kept {
			replaced[i] = second
		}
		s.getLogger().Printf("windowed transcription %s: window [%d,%d]ms was decoded as %q at confidence %.2f below media.stt.redecode_below=%v; decoded again as %q, replaced=%v (SPEC §8.6.13)",
			relPath, r.span.StartMS, r.span.EndMS, r.lang, r.conf, below, itemLang, kept)
		out.windows = append(out.windows, model.RedecodedWindow{
			StartMS: r.span.StartMS, EndMS: r.span.EndMS,
			FirstLanguage: r.lang, FirstConfidence: r.conf,
			Language: itemLang, Replaced: kept,
		})
	}
	return spliceWindows(windows, reports, replaced), out
}

// qualifyingWindows returns the indexes of the reports that qualify for a
// second decode (§8.6.13): the decoder reported a language other than
// itemLang at a confidence below the threshold, and at least one other window
// reported itemLang at or above it. A window with no reported language or no
// reported confidence does not qualify. The order is the schedule order, so
// the record is ascending by start.
func qualifyingWindows(reports []windowReport, itemLang string, below float64) []int {
	anchored := false
	for _, r := range reports {
		if r.lang == itemLang && r.conf >= below {
			anchored = true
			break
		}
	}
	if !anchored {
		return nil
	}
	var out []int
	for i, r := range reports {
		if r.lang != "" && r.lang != itemLang && r.conf > 0 && r.conf < below {
			out = append(out, i)
		}
	}
	return out
}

// redecodeWindow decodes the window span once more on the hinted transcriber.
// kept is true only when every piece decoded and at least one has transcript
// content: a second decode that failed, in whole or in part, or that came back
// empty, leaves the first decode in place.
func (s *Service) redecodeWindow(ctx context.Context, relPath, tmpPath string, hinted model.Transcriber, plan windowSchedule, r windowReport) ([]TranscriptWindow, bool) {
	pieces, err := s.extractWithinCap(ctx, tmpPath, r.span.StartMS, r.span.EndMS, plan.capBytes, maxWindowSplits)
	if err != nil {
		s.getLogger().Printf("windowed transcription %s: cannot cut window [%d,%d]ms for the second decode: %v; the first decode stands", relPath, r.span.StartMS, r.span.EndMS, err)
		return nil, false
	}
	results, derr := s.decodePieces(ctx, relPath, hinted, pieces, plan)
	if derr != nil || !resultsHaveContent(results) {
		return nil, false
	}
	decoded, _ := resultWindows(results)
	return decoded, true
}

// resultsHaveContent reports whether any decoded piece holds transcript text
// once the segment markers are removed.
func resultsHaveContent(results []pieceResult) bool {
	for _, r := range results {
		if strings.TrimSpace(stripSegmentMarkers(r.res.Text)) != "" {
			return true
		}
	}
	return false
}

// spliceWindows rebuilds the merge input with each replaced window's pieces in
// place of its first decode. The reports cover every decoded window in order,
// so the pieces of a window that was not replaced are copied as they are. When
// the reports do not account for every piece the input is returned unchanged,
// because a splice that dropped a piece would drop text.
func spliceWindows(windows []TranscriptWindow, reports []windowReport, replaced map[int][]TranscriptWindow) []TranscriptWindow {
	if len(replaced) == 0 || !reportsTileWindows(reports, len(windows)) {
		return windows
	}
	out := make([]TranscriptWindow, 0, len(windows))
	for i, r := range reports {
		if second, ok := replaced[i]; ok {
			out = append(out, second...)
			continue
		}
		out = append(out, windows[r.first:r.first+r.count]...)
	}
	return out
}

// reportsTileWindows reports whether the reports name every one of n pieces
// exactly once, in order.
func reportsTileWindows(reports []windowReport, n int) bool {
	next := 0
	for _, r := range reports {
		if r.first != next || r.count < 0 {
			return false
		}
		next += r.count
	}
	return next == n
}

// applyRedecodeMeta records the §8.6.13 re-decode outcome on an item-scoped
// transcript's meta: redecoded_windows when windows were decoded again, and
// redecode_rule whenever the rule ran, so a consumer and a later reconcile
// pass can tell "checked" from "not checked".
func applyRedecodeMeta(meta *transcriptMeta, coverage *TranscriptCoverage) {
	if coverage == nil || coverage.RedecodeRule == "" {
		return
	}
	meta.RedecodeRule = coverage.RedecodeRule
	meta.RedecodedWindows = coverage.RedecodedWindows
}
