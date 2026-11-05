package ingest

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// This file implements the §8.6.13 reconcile pass of spec 0.88.0 (dir2mcp
// #1143). The low-confidence rule does not join the derivation identity, so
// an upgrade does not decode stored transcripts again. A stored transcript
// without redecode_rule is checked here instead, when an operator arms the
// pass (`dir2mcp reindex --redecode-language-windows`).
//
// A stored transcript keeps no per-window decoder report, so the candidates
// come from the stored text alone: the §8.8 detector runs on the segments of
// each window core, against the item language. A window whose text the
// detector assigns to another language, at or above the §8.8 floor, is only
// a suspect: the text cannot tell a window the decoder wrote in the wrong
// language from a passage that is in another language. So the pass first
// probes the window (spec 0.89.0): it decodes one centred slice of at most
// media.stt.language_probe_sec with no language hint. Only a window whose
// probe reports the item language at or above media.stt.redecode_below is
// decoded again, with the item language as the request language.

// SetReconcileLanguageWindows arms or disarms the §8.6.13 reconcile pass for
// this Service. Armed, a cache hit on an item-scoped windowed transcript with
// no redecode_rule is checked before it is served.
func (s *Service) SetReconcileLanguageWindows(on bool) {
	s.reconcileLanguageWindows = on
}

// STTLanguagePin returns the source-language pin a Service built from cfg
// decodes under: media.language or the resolved STT profile's stt_language,
// as the active profile carries it. Empty means no pin. A pinned corpus never
// needs the §8.6.13 rule, so the reconcile flag refuses it.
func STTLanguagePin(cfg config.Config) string {
	return sttExpectedLanguage(cfg)
}

// reconcileWindow is one scheduled window of a stored transcript: its extent
// (the audio it was decoded from) and its core (the stretch the merge kept).
type reconcileWindow struct {
	extent CoverageRange
	core   CoverageRange
	lang   string
	conf   float64
}

// reconcileStoredRedecode runs the reconcile pass on one cache hit and
// returns the hit to serve. Every exit that cannot finish the check returns
// the hit unchanged, so the transcript is served as recorded and checked
// again on the next armed run.
func (s *Service) reconcileStoredRedecode(ctx context.Context, doc model.Document, content []byte, paths transcriptCachePaths, hit cachedTranscript) cachedTranscript {
	if !s.reconcileApplies(hit) {
		return hit
	}
	itemLang, _, ok := s.detectLanguage(hit.text)
	if !ok {
		return hit
	}
	rp, ok := cachedWindowPlan(hit.coverage)
	if !ok || len(rp.starts) < 2 {
		return hit
	}
	segs := windowSegments(model.TranscriptResult{Text: hit.text, Words: hit.words}, 0, 0)
	cands := s.reconcileCandidates(rp, segs, itemLang)
	if len(cands) == 0 {
		return s.publishReconciled(doc, paths, hit, hit.text, hit.words, nil, true)
	}
	return s.redecodeStoredWindows(ctx, doc, content, paths, hit, segs, cands, itemLang)
}

// reconcileApplies reports whether a cache hit is a transcript the pass
// checks: armed, the rule on, an item-scoped windowed decode (a recorded
// schedule, no per-window languages), no identifier route, and no
// redecode_rule yet.
func (s *Service) reconcileApplies(hit cachedTranscript) bool {
	if !s.reconcileLanguageWindows || !s.redecodeRuleOn() || hit.route != nil {
		return false
	}
	c := hit.coverage
	return c != nil && c.RedecodeRule == "" && c.WindowMS > 0 && c.WindowsDecoded >= 2 && len(c.Languages) == 0
}

// reconcileCandidates lists the scheduled windows whose stored text the §8.8
// detector assigns to a language other than itemLang, at or above its floor.
func (s *Service) reconcileCandidates(rp windowRetryPlan, segs []mergedSegment, itemLang string) []reconcileWindow {
	var out []reconcileWindow
	for i, start := range rp.starts {
		w := reconcileWindow{extent: CoverageRange{StartMS: start, EndMS: minInt(start+rp.plan.windowMS, rp.plan.totalMS)}}
		w.core = CoverageRange{StartMS: start, EndMS: rp.plan.totalMS}
		if i+1 < len(rp.starts) {
			w.core.EndMS = rp.starts[i+1]
		}
		tag, conf, ok := s.detectLanguage(coreText(segs, w.core))
		if !ok || tag == itemLang {
			continue
		}
		w.lang, w.conf = tag, conf
		out = append(out, w)
	}
	return out
}

// coreText joins the bodies of the segments that start inside core.
func coreText(segs []mergedSegment, core CoverageRange) string {
	var b strings.Builder
	for _, sg := range segs {
		if sg.startMS >= core.StartMS && sg.startMS < core.EndMS {
			b.WriteString(sg.body)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// redecodeStoredWindows stages the audio, probes each suspect window, decodes
// again with the item language each window the probe confirms, splices the
// kept decodes into the stored segments, and publishes the result with its
// record.
func (s *Service) redecodeStoredWindows(ctx context.Context, doc model.Document, content []byte, paths transcriptCachePaths, hit cachedTranscript, segs []mergedSegment, cands []reconcileWindow, itemLang string) cachedTranscript {
	hinted, ok := model.TranscriberForLanguage(s.transcriber, itemLang)
	if !ok {
		s.getLogger().Printf("reconcile %s: %d window(s) are in another language than %q, but the STT route cannot take a language hint; served as recorded (SPEC §8.6.13)", doc.RelPath, len(cands), itemLang)
		return hit
	}
	relPath, audio, ok, err := s.transcriptInput(ctx, doc, content)
	if err != nil || !ok {
		s.getLogger().Printf("reconcile %s: no audio to decode again (%v); served as recorded (SPEC §8.6.13)", doc.RelPath, err)
		return hit
	}
	tmpPath, cleanup, err := stageMediaTemp(audio, filepath.Ext(relPath))
	if err != nil {
		s.getLogger().Printf("reconcile %s: stage failed (%v); served as recorded (SPEC §8.6.13)", doc.RelPath, err)
		return hit
	}
	defer cleanup()
	run := reconcileRun{segs: segs}
	for _, w := range cands {
		s.reconcileOneWindow(ctx, doc, relPath, tmpPath, hinted, w, itemLang, &run)
	}
	if ctx.Err() != nil {
		return hit
	}
	text, words := hit.text, hit.words
	if run.anyKept {
		text, words = renderSegments(run.segs)
	}
	// A probe that failed leaves its window unchecked, so the transcript gets
	// no redecode_rule and the next armed run checks that window again.
	return s.publishReconciled(doc, paths, hit, text, words, run.record, !run.probeFailed)
}

// reconcileRun is the state of the reconcile pass over one transcript: the
// segments as spliced so far, the record of the windows decoded again, whether
// any second decode was kept, and whether any probe failed.
type reconcileRun struct {
	segs        []mergedSegment
	record      []model.RedecodedWindow
	anyKept     bool
	probeFailed bool
}

// reconcileOneWindow probes one suspect window and, when the probe confirms
// the item language, decodes it again with that language and splices a kept
// decode in. A stored transcript keeps no per-window decoder report, so every
// window this pass decodes again was probed, and its record names the probe
// result in first_language and first_confidence (SPEC §8.6.13, 0.89.0). The
// live rule of transcript_redecode.go records the decoder report instead.
func (s *Service) reconcileOneWindow(ctx context.Context, doc model.Document, relPath, tmpPath string, hinted model.Transcriber, w reconcileWindow, itemLang string, run *reconcileRun) {
	lang, conf, err := s.probeWindow(ctx, relPath, tmpPath, w.extent)
	if err != nil {
		run.probeFailed = true
		s.getLogger().Printf("reconcile %s: window [%d,%d]ms reads as %q; the language probe failed (%v); left as recorded (SPEC §8.6.13)",
			doc.RelPath, w.extent.StartMS, w.extent.EndMS, w.lang, err)
		return
	}
	below := s.cfg.MediaSTTRedecodeBelow
	if lang != itemLang || conf < below {
		s.getLogger().Printf("reconcile %s: window [%d,%d]ms reads as %q; the language probe reports %q at confidence %.2f, not %q at media.stt.redecode_below=%v or above; left as recorded (SPEC §8.6.13)",
			doc.RelPath, w.extent.StartMS, w.extent.EndMS, w.lang, lang, conf, itemLang, below)
		return
	}
	plan := windowSchedule{capBytes: sttPayloadCapBytes(s.transcriber), label: "transcription"}
	second, kept := s.redecodeWindow(ctx, relPath, tmpPath, hinted, plan, windowReport{span: w.extent})
	if kept {
		run.segs, run.anyKept = spliceCore(run.segs, second, w.core), true
	}
	s.getLogger().Printf("reconcile %s: window [%d,%d]ms reads as %q; the language probe reports %q at confidence %.2f; decoded again as %q, replaced=%v (SPEC §8.6.13)",
		doc.RelPath, w.extent.StartMS, w.extent.EndMS, w.lang, lang, conf, itemLang, kept)
	run.record = append(run.record, model.RedecodedWindow{
		StartMS: w.extent.StartMS, EndMS: w.extent.EndMS,
		FirstLanguage: lang, FirstConfidence: conf,
		Language: itemLang, Replaced: kept,
	})
}

// probeWindow decodes, with no language hint, the probe slice centred in the
// window extent (probeSpan, the slice the §8.2.4 identifier also cuts) and
// returns the language and the confidence the decoder reported for it. A
// confidence of 0 means none was reported.
func (s *Service) probeWindow(ctx context.Context, relPath, tmpPath string, extent CoverageRange) (string, float64, error) {
	span := s.probeSpan(extent)
	data, err := s.extractMediaSegment(ctx, tmpPath, span.StartMS, span.EndMS)
	if err != nil {
		return "", 0, err
	}
	if len(data) == 0 {
		return "", 0, errors.New("the probe slice is empty")
	}
	res, err := s.transcribeResult(ctx, model.TranscriberForAudioDuration(s.transcriber, span.EndMS-span.StartMS), relPath, data)
	if err != nil {
		return "", 0, err
	}
	return provider.PrimarySubtag(res.Language), res.LanguageConfidence, nil
}

// spliceCore replaces the stored segments that start inside core with the
// segments of the second decode that start inside core. The second decode's
// pieces are de-duplicated and grouped among themselves, as the merge treats
// one window (see retrySegments).
func spliceCore(segs []mergedSegment, second []TranscriptWindow, core CoverageRange) []mergedSegment {
	in := func(start int) bool { return start >= core.StartMS && start < core.EndMS }
	out := make([]mergedSegment, 0, len(segs))
	for _, sg := range segs {
		if !in(sg.startMS) {
			out = append(out, sg)
		}
	}
	var fresh []mergedSegment
	for k, w := range second {
		for _, sg := range windowSegments(w.Res, w.StartMS, k+1) {
			if sg.startMS < core.StartMS {
				sg.startMS = core.StartMS
			}
			if in(sg.startMS) {
				fresh = append(fresh, sg)
			}
		}
	}
	sort.SliceStable(fresh, func(a, b int) bool { return fresh[a].startMS < fresh[b].startMS })
	out = append(out, groupMistimedSegments(dedupMergedSegments(fresh))...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].startMS < out[b].startMS })
	return out
}

// publishReconciled stamps the record on a copy of the hit and writes the
// cache entry: the whole entry when the text changed, the coverage sidecar
// only otherwise. mark records redecode_rule, so the next armed run skips the
// transcript; without it and with nothing to record, the hit is served as it
// is. A write that fails still serves the checked transcript; the next armed
// run checks it again.
func (s *Service) publishReconciled(doc model.Document, paths transcriptCachePaths, hit cachedTranscript, text string, words []model.TimedWord, record []model.RedecodedWindow, mark bool) cachedTranscript {
	if !mark && len(record) == 0 {
		return hit
	}
	out := hit
	out.coverage = copyCoverage(hit.coverage)
	if mark {
		out.coverage.RedecodeRule = model.RedecodeRuleLowConfidence
	}
	out.coverage.RedecodedWindows = mergeRedecodedWindows(hit.coverage.RedecodedWindows, record)
	if text == hit.text {
		s.publishCachedCoverage(paths.coverage, out.coverage)
		return out
	}
	out.text, out.words = normalizeTranscriptNewlines(text), words
	if err := s.publishTranscriptCache(paths, out); err != nil {
		s.getLogger().Printf("reconcile %s: %v; the next armed run checks it again", doc.RelPath, err)
	}
	return out
}

// mergeRedecodedWindows keeps the entries an earlier run recorded for windows
// this run did not record again, so a run after a failed probe does not lose
// the record of a window that was already replaced. The result is in
// ascending start_ms order (SPEC §8.6.13).
func mergeRedecodedWindows(prior, current []model.RedecodedWindow) []model.RedecodedWindow {
	seen := make(map[[2]int]bool, len(current))
	for _, w := range current {
		seen[[2]int{w.StartMS, w.EndMS}] = true
	}
	var out []model.RedecodedWindow
	for _, w := range prior {
		if !seen[[2]int{w.StartMS, w.EndMS}] {
			out = append(out, w)
		}
	}
	out = append(out, current...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].StartMS < out[b].StartMS })
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
