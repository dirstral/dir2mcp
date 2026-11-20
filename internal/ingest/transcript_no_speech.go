package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// This file holds the spec 0.99.0 details of the no-speech outcome. Spec
// 0.98.0 (#1182) made a recording with no speech a status=skipped
// skip_reason=no_speech outcome, cached the empty transcript and counted a
// window with no speech as decoded (no_speech.go). Spec 0.99.0 adds:
//
//   - a no-speech window adds no language and is not gated, and the §8.6.6
//     low-density check leaves it out of the duration;
//   - a failed window next to windows with no speech is still a failure: the
//     failed window can hold speech, so the document is an error and retried.
//     A failed window takes precedence over the language_uncovered skip when
//     no window produced transcript text;
//   - a no-speech track in a document that stays ok (another representation
//     keeps it searchable) is decoded again after an STT identity change, and
//     open_file answers MEDIA_NO_TEXT for its absent transcript. The record of
//     the identity that found no speech is one settings row per path.

// hasTranscriptText reports whether a decode's text holds something that the
// merge keeps. It reads the text as the merge does (windowSegments): a line
// that is only a real timestamp marker ("[00:12]") holds no text, and every
// other line that is not blank does. A bracketed line that is not a timestamp
// ("[hello]", "[music]") is text.
func hasTranscriptText(text string) bool {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		_, body, ok := parseTranscriptTimestamp(line)
		if !ok || strings.TrimSpace(body) != "" {
			return true
		}
	}
	return false
}

// windowsWithText returns the windows whose decode holds text, in order.
func windowsWithText(windows []TranscriptWindow) []TranscriptWindow {
	out := make([]TranscriptWindow, 0, len(windows))
	for _, w := range windows {
		if hasTranscriptText(w.Res.Text) {
			out = append(out, w)
		}
	}
	return out
}

// decodedWithoutSpeech reports whether a window's decode succeeded (err is nil),
// every piece of the window has a result, and every result is a no-speech
// result. A window with a piece that has no result is not a no-speech window,
// also when a later step returned no error: the missing piece can hold speech.
func decodedWithoutSpeech(pieces []windowPiece, results []pieceResult, err error) bool {
	return err == nil && len(results) >= len(pieces) && noSpeechResults(results)
}

// noSpeechResults reports whether every decoded piece of a window came back
// with no text: a decode that succeeded and heard nothing said.
func noSpeechResults(results []pieceResult) bool {
	return len(results) > 0 && !resultsHaveContent(results)
}

// recordNoSpeechWindow settles a window-scoped decode window whose pieces all
// came back with no speech (SPEC §8.6.13, spec 0.99.0; §8.2.2). There is nothing to identify,
// route or screen: the decoder's language report on music is noise, and the
// identifier would be asked about audio with nothing said. The window is
// recorded as decoded, so it counts toward coverage, on the route that decoded
// it, with the pinned language when the recording has a pin and with no
// language otherwise. The continuity state does not advance, so the next
// window with no confident language inherits the language of the last window
// that resolved one, not the silence, and stays on that window's route.
//
// A window whose route failed and whose fallback decode found no speech
// (fallbackFrom is set) keeps its fallback_from entry and follows the window
// retry (§8.6.13). A recording whose every window holds no speech is cached
// with that fallback record, and the §8.6.7 retry conditions decide when the
// route is tried again (§8.6.6).
func (s *Service) recordNoSpeechWindow(relPath string, plan windowSchedule, core CoverageRange, results []pieceResult, cur routedSTT, fallbackFrom string, st *windowLanguageState) windowDecode {
	s.getLogger().Printf("windowed %s: window [%d,%d]ms of %s holds no speech; it counts as decoded (SPEC §8.6.13)",
		plan.label, core.StartMS, core.EndMS, relPath)
	// The no-speech decode adds no language (§8.6.13). A pin is another
	// signal: it resolves the language of every window (§8.2.2), so the
	// window records the pinned language. With no pin the window has no
	// language. The identifier is not asked about a window with nothing said,
	// so it gives no report for it.
	lang, source := "", ""
	if st.pinned {
		lang, source = st.lang, langSourceConfigured
	}
	var out windowDecode
	out.decoded, out.covered, out.languages = recordWindow(results, core, lang, source, nil, cur, true, fallbackFrom)
	out.noSpeech = &core
	st.recordDecoded(out.covered, out.languages)
	return out
}

// failedBesideNoSpeech is the §8.6.13 verdict on a windowed decode that
// produced no text and in which at least one window failed. It is an error,
// not a no-speech recording and not a declared skip: a failed window can hold
// speech (in a covered language too), so the document is recorded as an error
// and retried. A failed window takes precedence over the §8.2.2 rules, so
// this holds also when other windows are language_uncovered refusals or
// no-speech results (spec 0.99.0). It returns nil for every other decode. A
// decode with refused windows and no failed window goes to the §8.2.2
// terminal-status rules (allWindowsRefusedReason): a quality-gate refusal next
// to no-speech windows is TRANSCRIBE_FAILED there, and a language refusal is
// the language skip.
func failedBesideNoSpeech(relPath, text string, coverage *TranscriptCoverage) error {
	if coverage == nil || hasTranscriptText(text) {
		return nil
	}
	failed := coverage.WindowsFailed
	if failed == 0 && len(coverage.Refused) == 0 {
		// A coverage record without the count (item scope): every window that
		// did not decode failed.
		failed = coverage.WindowsAttempted - coverage.WindowsDecoded
	}
	if failed <= 0 {
		return nil
	}
	if len(coverage.Refused) > 0 {
		return fmt.Errorf("windowed transcription %s: %d of %d windows failed and no window decoded to text (the other windows were refused or hold no speech); a failed window can hold speech, so the recording is retried (SPEC §8.6.13)",
			relPath, failed, coverage.WindowsAttempted)
	}
	return fmt.Errorf("windowed transcription %s: %d of %d windows failed and every other window holds no speech; a failed window can hold speech, so the recording is retried (SPEC §8.6.13)",
		relPath, failed, coverage.WindowsAttempted)
}

// withNoSpeech records on a coverage record how much of the recording the
// no-speech windows span, coalesced and clamped to the recording.
func withNoSpeech(cov *TranscriptCoverage, spans []CoverageRange, totalMS int) *TranscriptCoverage {
	if cov == nil || len(spans) == 0 {
		return cov
	}
	total := 0
	for _, r := range coalesceCoverageRanges(spans) {
		if totalMS > 0 && r.EndMS > totalMS {
			r.EndMS = totalMS
		}
		if r.EndMS > r.StartMS {
			total += r.EndMS - r.StartMS
		}
	}
	cov.NoSpeechMS = total
	return cov
}

// speechDuration is the duration the §8.6.6 low-density check compares the
// transcript with: the recording minus its no-speech windows (spec 0.99.0). A
// recording with one window of speech and long windows of music is not
// degenerate output. A result at or below zero disables the duration floor, as
// an unknown duration does.
func speechDuration(duration time.Duration, coverage *TranscriptCoverage) time.Duration {
	if coverage == nil || coverage.NoSpeechMS <= 0 {
		return duration
	}
	left := duration - time.Duration(coverage.NoSpeechMS)*time.Millisecond
	if left < 0 {
		return 0
	}
	return left
}

// settingsStore is the optional store capability behind the no-speech identity
// record (store.NoSpeechIdentitySettingKey, SPEC §8.6.13). A no-speech track has
// no transcript representation whose meta_json could carry its identity, so the
// record is one settings row per rel_path. Its lifecycle keeps it from going
// stale: it is deleted whenever the document's transcript is derived again
// (content change, forced reindex, retry, identity change), written only when
// that derivation finds no speech, and deleted with the document by
// MarkDocumentDeleted (which a delete, a rename and a move all reach). A store
// without the capability records nothing, and a no-speech outcome is then
// decoded again only when its content changes.
type settingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	DeleteSetting(ctx context.Context, key string) error
}

// recordNoSpeechIdentity stores the active STT derivation identity as the one
// that found no speech in relPath. A failed write is returned: without the
// record an identity change later would not decode this recording again
// (SPEC §8.6.13), so the caller must not settle the no-speech outcome. A store
// without the capability records nothing and returns nil.
func (s *Service) recordNoSpeechIdentity(ctx context.Context, relPath string) error {
	st, ok := s.store.(settingsStore)
	if !ok {
		return nil
	}
	if err := st.SetSetting(ctx, store.NoSpeechIdentitySettingKey(relPath), s.activeTranscriptIdentity()); err != nil {
		return fmt.Errorf("record the no-speech derivation identity of %s: %w", relPath, err)
	}
	return nil
}

// clearNoSpeechIdentity deletes the no-speech record of relPath. It runs at the
// start of every transcript derivation of the document, so a record only ever
// describes the derivation that wrote it: a later decode with speech, a
// sidecar that now wins, or a provider failure leaves no record behind, and a
// decode that finds no speech again writes a fresh one.
func (s *Service) clearNoSpeechIdentity(ctx context.Context, relPath string) {
	st, ok := s.store.(settingsStore)
	if !ok {
		return
	}
	if err := st.DeleteSetting(ctx, store.NoSpeechIdentitySettingKey(relPath)); err != nil {
		s.getLogger().Printf("stt: clear the no-speech derivation identity of %s: %v", relPath, err)
	}
}

// noSpeechTrackStale reports whether a document that stays ok has a no-speech
// track that another STT derivation identity than the active one found (SPEC
// §8.6.13, spec 0.99.0; §8.6.7). Another model can find speech that this one
// did not, so the document is processed again. A document skipped as
// no_speech is not this case: its row carries the identity token, and
// noSpeechStale decides it. It is false when STT is off, for a document that
// is not ok or not audio or video, and when no record exists.
func (s *Service) noSpeechTrackStale(ctx context.Context, existing model.Document) bool {
	if s.transcriber == nil || existing.Deleted || existing.Status != string(model.DocumentStatusOK) || !isSidecarMediaType(existing.DocType) {
		return false
	}
	st, ok := s.store.(settingsStore)
	if !ok {
		return false
	}
	recorded, err := st.GetSetting(ctx, store.NoSpeechIdentitySettingKey(existing.RelPath))
	if err != nil || recorded == "" {
		return false
	}
	active := s.activeTranscriptIdentity()
	if recorded == active {
		return false
	}
	s.getLogger().Printf("STT model changed for %s (a track held no speech under %q, configured %q); decoding it again (SPEC §8.6.13)",
		existing.RelPath, recorded, active)
	return true
}

// ReadOrComputeTranscriptOutcome is ReadOrComputeTranscript for a caller that
// must tell a no-speech result from other empty text (the MCP transcribe
// tools, SPEC §8.6.13). noSpeech is true when the decode, new or cached,
// found no speech in the whole recording (heardNoSpeech); the text is then
// empty and err is nil. An empty decode with refused windows is not this case.
func (s *Service) ReadOrComputeTranscriptOutcome(ctx context.Context, doc model.Document, content []byte, language string) (text string, noSpeech bool, err error) {
	text, _, coverage, _, err := s.readOrComputeTranscriptWithWords(ctx, doc, content, language, doc.RelPath)
	if err != nil {
		return "", false, err
	}
	if !hasTranscriptText(text) && heardNoSpeech(coverage) {
		return "", true, nil
	}
	return text, false, nil
}

// TranscriptCached reports whether the transcript cache holds an entry for
// the content and language under the active STT derivation identity, the key
// that readOrComputeTranscriptWithWords reads. A cached no-speech result is an
// entry too (an empty text). A caller uses it to say whether a
// read sent a provider request.
func (s *Service) TranscriptCached(content []byte, language string) bool {
	path := filepath.Join(s.cfg.StateDir, "cache", "transcribe", s.transcriptCacheKey(content)+TranscriptLangSuffix(language)+".txt")
	_, err := os.Stat(path)
	return err == nil
}

// SettleNoSpeechOutsideScan leaves a document whose first audio track was a
// no-speech result in the state that a scan writes (SPEC §8.6.13). It serves
// the MCP transcribe tool, which decodes outside a scan and decodes only the
// first audio track:
//
//   - an earlier transcript of the track is retired with its chunks, its
//     translations and the summaries derived from it;
//   - the no-speech record of the path holds the active STT derivation
//     identity;
//   - a document that keeps another searchable representation stays as it is,
//     and a document with none is recorded as status="skipped" with
//     skip_reason="no_speech" and the identity token of persistNoSpeechSkip.
//     The row keeps its content_hash, so the next scan does not decode the
//     recording again.
//
// no_speech needs every selected track to hold no speech (§8.6.12). When
// media.stt.tracks selects more than the first track, the first track alone
// does not decide it, so the stored state is left to the next scan, which
// decodes every selected track.
//
// A retirement or a record write that fails is returned, as in a scan. The
// document is then recorded as status="error", so the next scan processes it
// again: a document whose transcript was retired must not be skipped as
// unchanged.
func (s *Service) SettleNoSpeechOutsideScan(ctx context.Context, doc model.Document) error {
	if sel, err := config.ParseSTTTracks(s.cfg.MediaSTTTracks); err != nil || sel.Mode != config.STTTracksFirst {
		s.getLogger().Printf("stt: the first audio track of %s holds no speech; media.stt.tracks selects more tracks, so the next scan settles the document (SPEC §8.6.12)", doc.RelPath)
		return nil
	}
	if err := s.settleNoSpeechOutsideScan(ctx, doc); err != nil {
		s.persistNonFatalDocError(ctx, doc, err, nil)
		return err
	}
	return nil
}

// settleNoSpeechOutsideScan is the body of SettleNoSpeechOutsideScan.
func (s *Service) settleNoSpeechOutsideScan(ctx context.Context, doc model.Document) error {
	if err := s.retireTrackTranscripts(ctx, doc, trackContext{audioIndex: 0}); err != nil {
		return err
	}
	if err := s.recordNoSpeechIdentity(ctx, doc.RelPath); err != nil {
		return err
	}
	if s.hasIndexedRepresentations(ctx, doc.RelPath) {
		return nil
	}
	return s.persistNoSpeechSkip(ctx, doc)
}

// SettleSpeechOutsideScan is the counterpart of SettleNoSpeechOutsideScan for
// a decode outside a scan that holds speech. The no-speech record of an
// earlier decode no longer describes the recording, so it is deleted, as a
// scan does at the start of each derivation. A row that an earlier decode
// recorded as status="skipped" with skip_reason="no_speech" becomes
// status="ok": the document has a transcript now.
func (s *Service) SettleSpeechOutsideScan(ctx context.Context, doc model.Document) {
	s.clearNoSpeechIdentity(ctx, doc.RelPath)
	if doc.Status != string(model.DocumentStatusSkipped) || doc.SkipReason != model.SkipReasonNoSpeech {
		return
	}
	doc.Status = string(model.DocumentStatusOK)
	doc.SkipReason = ""
	doc.ErrorMessage = ""
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		s.getLogger().Printf("stt: record %s as indexed after a decode with speech: %v", doc.RelPath, err)
	}
}
