package ingest

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/statefs"
)

// This file implements the SPEC 0.93.0 no-speech outcome. A speech-to-text
// provider that answers a recording of music, ambient sound or silence with a
// well-formed response holding empty text and no segments has decoded the audio
// successfully and found nothing said. The adapters return that as an empty
// result with no error. Here the pipeline gives it its own outcome:
//
//   - a decode window with no speech counts as decoded and adds to coverage, so
//     it is never a shortfall under media.stt.min_coverage;
//   - a recording whose single decode, or every decode window, found no speech
//     is recorded as status="skipped" with skip_reason="no_speech". It is not an
//     error, so it is not retried while the file is unchanged.

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
	s.getLogger().Printf("stt: %s%s holds no speech: the decode succeeded and returned no text; recorded as skipped (skip_reason=%s, SPEC 0.93.0)",
		doc.RelPath, trackSuffixForLog(tc), model.SkipReasonNoSpeech)
	if err := s.retireTrackTranscripts(ctx, doc, tc); err != nil {
		return false, false, "", err
	}
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
