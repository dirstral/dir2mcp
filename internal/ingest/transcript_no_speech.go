package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/statefs"
	"github.com/dirstral/dir2mcp/internal/store"
)

// This file implements the SPEC 0.93.0 no-speech outcome. A speech-to-text
// provider that answers a recording of music, ambient sound or silence with a
// well-formed response holding empty text and no segments has decoded the audio
// successfully and found nothing said (a no-speech result, SPEC §8.2). The
// adapters return that as an empty result with no error. Here the pipeline
// gives it its own outcome:
//
//   - a decode window with no speech counts as decoded and adds to coverage, so
//     it is never a shortfall under media.stt.min_coverage, and the §8.6.6
//     low-density check leaves it out of the duration;
//   - a failed window next to windows with no speech is still a failure: the
//     failed window can hold speech, so the document is an error and retried;
//   - a recording whose single decode, or every decode window, found no speech
//     produces no transcript. When the document has no other searchable
//     representation it is recorded as status="skipped" with
//     skip_reason="no_speech"; otherwise it stays status="ok". It is not an
//     error, and it is not decoded again while both the content and the STT
//     derivation identity are unchanged (§8.6.13).

// errNoSpeech reports that the decode of a recording succeeded and found no
// speech anywhere in it. It travels on the error channel of the transcript read
// so the five-value return of readOrComputeTranscriptOpts stays as it is, and it
// is never wrapped in ErrTranscriptProviderFailure: it is not a failure.
var errNoSpeech = errors.New("the speech-to-text decode found no speech in the recording")

// noSpeechMarkerSuffix names the cache marker written beside an empty transcript
// whose decode found no speech. An empty cached text WITHOUT the marker keeps its
// earlier meaning (a video with no audio track, or text cached by an older
// binary), so a cache hit never turns those into a no_speech skip.
const noSpeechMarkerSuffix = ".nospeech"

// decodedNoSpeech reports whether a decode that returned without error found no
// speech in the WHOLE recording: no text, and either a single-request decode (no
// coverage record) or a windowed decode in which every window decoded and none
// was refused. A windowed decode with a failed or refused window is not this
// outcome: the failure or the refusal is the story, and it keeps its own path.
func decodedNoSpeech(text string, coverage *TranscriptCoverage) bool {
	if strings.TrimSpace(stripSegmentMarkers(text)) != "" {
		return false
	}
	if coverage == nil {
		return true
	}
	return coverage.WindowsAttempted > 0 &&
		coverage.WindowsDecoded == coverage.WindowsAttempted &&
		len(coverage.Refused) == 0
}

// splitNoSpeech separates the no-speech outcome from a real error: noSpeech is
// true, and err nil, when err is errNoSpeech; any other error is returned as
// it is.
func splitNoSpeech(err error) (bool, error) {
	if errors.Is(err, errNoSpeech) {
		return true, nil
	}
	return false, err
}

// windowsWithText returns the windows whose decode holds text, in order.
func windowsWithText(windows []TranscriptWindow) []TranscriptWindow {
	out := make([]TranscriptWindow, 0, len(windows))
	for _, w := range windows {
		if strings.TrimSpace(stripSegmentMarkers(w.Res.Text)) != "" {
			out = append(out, w)
		}
	}
	return out
}

// fallbackNoSpeech is the verdict on a no-speech decode that the default
// profile produced because the selected route failed (§8.2.4,
// on_route_error=default). The default heard nothing, but the route that
// failed can hold the speech the default missed, and a route-fallback result
// is provisional. It is not settled as no_speech: it is an error that is
// retried on the next run, exactly as a failed window beside no-speech
// windows is (§8.6.13). It returns nil for a decode that used no fallback.
func fallbackNoSpeech(relPath string, coverage *TranscriptCoverage, route *itemRoute) error {
	from := ""
	if route != nil {
		from = route.FallbackFrom
	}
	if from == "" && coverage != nil {
		for _, l := range coverage.Languages {
			if l.FallbackFrom != "" {
				from = l.FallbackFrom
				break
			}
		}
	}
	if from == "" {
		return nil
	}
	return fmt.Errorf("transcription %s: route %q failed and the default profile found no speech; the failed route can hold speech, so the recording is retried (SPEC §8.2.4, §8.6.13)",
		relPath, from)
}

// decodedWithoutSpeech reports whether a window's decode succeeded (err is nil)
// and every piece of it is a no-speech result.
func decodedWithoutSpeech(results []pieceResult, err error) bool {
	return err == nil && noSpeechResults(results)
}

// noSpeechResults reports whether every decoded piece of a window came back
// with no text: a decode that succeeded and heard nothing said.
func noSpeechResults(results []pieceResult) bool {
	return len(results) > 0 && !resultsHaveContent(results)
}

// recordNoSpeechWindow settles a window-scoped decode window whose pieces all
// came back with no speech (SPEC 0.93.0, §8.2.2). There is nothing to identify,
// route or screen: the decoder's language report on music is noise, and the
// identifier would be asked about audio with nothing said. The window is
// recorded as decoded, so it counts toward coverage, with the unknown language
// and on the route that decoded it. The continuity state does not advance, so
// the next window with speech inherits the language of the last window that
// had some, not the silence.
func (s *Service) recordNoSpeechWindow(relPath string, plan windowSchedule, core CoverageRange, results []pieceResult, cur routedSTT, fallbackFrom string, st *windowLanguageState) windowDecode {
	s.getLogger().Printf("windowed %s: window [%d,%d]ms of %s holds no speech; it counts as decoded (SPEC 0.93.0)",
		plan.label, core.StartMS, core.EndMS, relPath)
	var out windowDecode
	out.decoded, out.covered, out.languages = recordWindow(results, core, "", "", nil, cur, true, fallbackFrom)
	out.noSpeech = &core
	st.recordDecoded(out.covered, out.languages)
	return out
}

// settleNoSpeech is the track verdict for a recording whose decode found no
// speech. An earlier run's transcript of this track (from before the audio
// changed, or before the provider learned to report no speech) is retired
// first, exactly as the §8.6.13 refusal does: a document recorded as skipped
// must not keep answering from chunks of a transcript it no longer has. A
// retirement that fails is an error, not a skip.
func (s *Service) settleNoSpeech(ctx context.Context, doc model.Document, tc trackContext) (bool, bool, string, error) {
	s.getLogger().Printf("stt: %s%s holds no speech: the decode succeeded and returned no text, so no transcript is produced (SPEC §8.6.13)",
		doc.RelPath, trackSuffixForLog(tc))
	if err := s.retireTrackTranscripts(ctx, doc, tc); err != nil {
		return false, false, "", err
	}
	s.recordNoSpeechIdentity(ctx, doc.RelPath)
	return false, false, model.SkipReasonNoSpeech, nil
}

// publishNoSpeechMarker makes the cache entry's no-speech marker match this
// decode and reports whether the text may be published beside it. A marker that
// cannot be written withholds the text cache, so the next run decodes again
// rather than reading an empty text whose meaning it cannot tell.
func (s *Service) publishNoSpeechMarker(path string, noSpeech bool) bool {
	if !noSpeech {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.getLogger().Printf("clear stale transcript no-speech marker (%s): %v; not caching this transcript", path, err)
			return false
		}
		return true
	}
	if err := statefs.WriteFile(path, []byte(model.SkipReasonNoSpeech)); err != nil {
		s.getLogger().Printf("write transcript no-speech marker (%s): %v; not caching this transcript", path, err)
		return false
	}
	return true
}

// cachedNoSpeech reports whether a cache hit is a no-speech decode: empty text
// with its marker beside it.
func cachedNoSpeech(text, markerPath string) bool {
	if strings.TrimSpace(text) != "" {
		return false
	}
	_, err := os.Stat(markerPath)
	return err == nil
}

// failedBesideNoSpeech is the §8.6.13 verdict on a windowed decode that
// produced no text because some windows failed and every other window is a
// no-speech result. It is an error, not a no-speech recording: a failed window
// can hold speech, so the document is recorded as an error and retried. It
// returns nil for every other decode, including one with refused windows,
// which the §8.2.2 terminal-status rules judge.
func failedBesideNoSpeech(relPath, text string, coverage *TranscriptCoverage) error {
	if coverage == nil || len(coverage.Refused) > 0 || strings.TrimSpace(stripSegmentMarkers(text)) != "" {
		return nil
	}
	if coverage.WindowsDecoded == 0 || coverage.WindowsDecoded >= coverage.WindowsAttempted {
		return nil
	}
	return fmt.Errorf("windowed transcription %s: %d of %d windows failed and every other window holds no speech; a failed window can hold speech, so the recording is retried (SPEC §8.6.13)",
		relPath, coverage.WindowsAttempted-coverage.WindowsDecoded, coverage.WindowsAttempted)
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
// transcript with: the recording minus its no-speech windows (SPEC 0.93.0). A
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

// settlePendingNoSpeech settles a no-speech recording once recognition has had
// its chance (SPEC §8.6.13). settled is false when the transcript path did not
// find a whole-recording no-speech result for this document. When recognition
// produced a representation the document keeps it and stays status="ok", so it
// is credited as indexed (suppress false). Otherwise it is recorded as
// status="skipped" with skip_reason="no_speech" and counted as a skip
// (suppress true, so the caller does not also credit it as indexed).
func (s *Service) settlePendingNoSpeech(ctx context.Context, doc model.Document, recognized bool) (suppress, settled bool) {
	if !s.pendingNoSpeechSkip {
		return false, false
	}
	s.pendingNoSpeechSkip = false
	// The document keeps another searchable representation when recognition
	// produced one now, or when one from an earlier run is still active (the
	// transcripts of this derivation were retired before this point).
	if recognized || s.hasIndexedRepresentations(ctx, doc.RelPath) {
		s.getLogger().Printf("stt: %s holds no speech but keeps another searchable representation; it stays indexed (SPEC §8.6.13)", doc.RelPath)
		return false, true
	}
	s.getLogger().Printf("stt: %s holds no speech and has no other searchable representation; recorded as skipped (skip_reason=%s, SPEC §8.6.13)",
		doc.RelPath, model.SkipReasonNoSpeech)
	s.addSkipped(1)
	s.persistNonFatalDocSkip(ctx, doc, model.SkipReasonNoSpeech)
	s.noSpeechSkippedThisDoc = true
	return true, true
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
// that found no speech in relPath. A failed write is logged: the cost is that an
// identity change later does not re-decode this recording until its content
// changes or it is reindexed with --force.
func (s *Service) recordNoSpeechIdentity(ctx context.Context, relPath string) {
	st, ok := s.store.(settingsStore)
	if !ok {
		return
	}
	if err := st.SetSetting(ctx, store.NoSpeechIdentitySettingKey(relPath), s.activeTranscriptIdentity()); err != nil {
		s.getLogger().Printf("stt: record the no-speech derivation identity of %s: %v", relPath, err)
	}
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

// noSpeechIdentityStale reports whether the no-speech outcome recorded for a
// media document was found by another STT derivation identity than the active
// one (SPEC §8.6.13, §8.6.7). Another model can find speech that this one did
// not, so the document is processed again. It is false when STT is off, for
// a document that is not audio or video, and when no record exists.
func (s *Service) noSpeechIdentityStale(ctx context.Context, existing model.Document) bool {
	if s.transcriber == nil || existing.Deleted || !isSidecarMediaType(existing.DocType) {
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
	s.getLogger().Printf("STT model changed for %s (no speech found with %q, configured %q); decoding it again (SPEC §8.6.13)",
		existing.RelPath, recorded, active)
	return true
}
