package ingest

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Silence is an outcome, not a failure (SPEC §8.6.6, spec 0.98.0, #1182).
//
// When the STT provider answers successfully and the item has no speech (an
// empty transcript for the whole item, or an audio/video item with no audio
// track) and the item therefore has no representation, it is recorded
// status=skipped with skip_reason=no_speech instead of status=error. The empty
// transcript is cached like any transcript (keyed by the media bytes and the STT
// identity, §8.6.7), so a later run does not call STT for it again, and the item
// is re-derived only when its content or the STT identity changes. A provider
// failure (no answer, a timeout, a non-success response) stays an error and is
// retried. A windowed decode with refused windows stays under §8.6.13/§8.2.2.

// mediaVerdict is the transcript path's provisional verdict on a media document
// that produced no representation of its own. It is provisional because
// recognition (#622) is an independent representation source that runs after
// the transcript path; only when recognition comes up empty too is the verdict
// recorded on the document (settleUnrepresentedMedia).
type mediaVerdict int

const (
	// mediaVerdictNone: the transcript path produced a representation, the
	// document has media chunks, or the path already settled the document.
	mediaVerdictNone mediaVerdict = iota
	// mediaVerdictNoRepresentation: a video with no sidecar, no derived
	// transcript (STT off or unconfigured) and no media chunks (#398/#495). It is
	// a durable status=error.
	mediaVerdictNoRepresentation
	// mediaVerdictNoSpeech: STT answered and heard no speech, or the media has no
	// audio track, and the document has no media chunks. It is a durable
	// status=skipped with skip_reason=no_speech.
	mediaVerdictNoSpeech
)

// noSpeechVerdict is the verdict for a document whose transcript path found no
// speech: a document that already produced media chunks (embed_multimodal
// augment) stays searchable through them, so it is not a skip.
func noSpeechVerdict(mediaProduced bool) mediaVerdict {
	if mediaProduced {
		return mediaVerdictNone
	}
	return mediaVerdictNoSpeech
}

// heardNoSpeech reports whether an empty transcript is the provider's own
// answer for the whole item: either a single-request decode (no coverage
// record) or a windowed decode that covered the whole recording and refused no
// window. A decode that missed or refused windows is governed by §8.6.13 and
// §8.2.2 instead: its empty text says nothing about the audio it did not hear.
func heardNoSpeech(coverage *TranscriptCoverage) bool {
	return coverage.Complete() && (coverage == nil || len(coverage.Refused) == 0)
}

// settleNoSpeechTrack finishes a track whose transcript is empty because the
// provider heard no speech. Any transcript an earlier run derived for the track
// (with another STT identity) is retired, exactly as a §8.6.13 refusal does, so
// a document recorded as having no speech does not keep stale chunks of a
// transcript the current model does not produce.
func (s *Service) settleNoSpeechTrack(ctx context.Context, doc model.Document, tc trackContext) (bool, bool, string, error) {
	s.getLogger().Printf("stt: no speech in %s%s: the provider answered with an empty transcript or the media has no audio track (SPEC §8.6.6)",
		doc.RelPath, trackSuffixForLog(tc))
	if err := s.retireTrackTranscripts(ctx, doc, tc); err != nil {
		return false, false, "", fmt.Errorf("no-speech outcome for %s: %w", doc.RelPath, err)
	}
	return false, false, model.SkipReasonNoSpeech, nil
}

// settleUnrepresentedMedia records the final verdict on a media document that
// produced no representation and that recognition did not rescue. It reports
// whether the document was settled here (so the caller must not also credit it
// as indexed, #426).
func (s *Service) settleUnrepresentedMedia(ctx context.Context, doc model.Document, secretPatterns []*regexp.Regexp, verdict mediaVerdict) bool {
	switch verdict {
	case mediaVerdictNoRepresentation:
		// No sidecar, no derived transcript, no multimodal keyframe chunks AND no
		// recognition: the video is known but unsearchable, so record it as a
		// durable status="error" diagnostic (#398/#495) while letting the run
		// continue.
		s.getLogger().Printf("no representation produced for video %s: %v", doc.RelPath, errNoVideoRepresentation)
		s.addErrors(1)
		s.persistNonFatalDocError(ctx, doc, errNoVideoRepresentation, secretPatterns)
		return true
	case mediaVerdictNoSpeech:
		s.addSkipped(1)
		s.persistNoSpeechSkip(ctx, doc)
		return true
	default:
		return false
	}
}

// persistNoSpeechSkip records the durable no_speech skip. The row's
// error_message carries a short detail naming the STT identity the item was
// judged with, so the incremental gates can tell when that identity changed
// (noSpeechSettled) without a store schema change.
func (s *Service) persistNoSpeechSkip(ctx context.Context, doc model.Document) {
	doc.ErrorMessage = s.noSpeechDetail()
	s.persistNonFatalDocSkip(ctx, doc, model.SkipReasonNoSpeech)
}

// noSpeechIdentityPrefix opens the detail persistNoSpeechSkip writes. The token
// comes first so the store's 512-byte cap on error_message can never cut it.
const noSpeechIdentityPrefix = "no speech (stt_identity="

// noSpeechDetail renders the detail recorded on a no_speech row: a stable token
// of the active STT identity plus the provider and model in plain words.
func (s *Service) noSpeechDetail() string {
	return fmt.Sprintf("%s%s): the STT provider answered with no speech, or the media has no audio track (SPEC §8.6.6); judged with %s/%s",
		noSpeechIdentityPrefix, s.noSpeechIdentityToken(), strings.TrimSpace(s.sttProvider), strings.TrimSpace(s.sttModel))
}

// noSpeechIdentityToken is a short, stable token of the active transcript
// derivation identity (§8.6.7). A hash, not the identity itself, because under
// window scope the identity carries the whole route table and is unbounded.
func (s *Service) noSpeechIdentityToken() string {
	return computeContentHash([]byte(s.activeTranscriptIdentity()))[:16]
}

// recordedNoSpeechToken reads the identity token back from a no_speech row's
// detail. ok is false when the detail does not carry one.
func recordedNoSpeechToken(detail string) (string, bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(detail), noSpeechIdentityPrefix)
	if !found {
		return "", false
	}
	token, _, found := strings.Cut(rest, ")")
	return token, found && token != ""
}

// isNoSpeechSkip reports whether a stored row is a no_speech skip.
func isNoSpeechSkip(doc model.Document) bool {
	return doc.Status == string(model.DocumentStatusSkipped) && doc.SkipReason == model.SkipReasonNoSpeech
}

// noSpeechSettled reports whether a stored no_speech skip still stands for the
// active STT identity. With no transcriber configured nothing can re-derive the
// item, so the recorded outcome stands, exactly as transcriptStale self-skips
// when STT is off. A row whose detail carries no token is not settled: it is
// re-derived once (the transcript cache usually answers without calling STT).
func (s *Service) noSpeechSettled(existing model.Document) bool {
	if s.transcriber == nil {
		return true
	}
	recorded, ok := recordedNoSpeechToken(existing.ErrorMessage)
	return ok && recorded == s.noSpeechIdentityToken()
}

// noSpeechStale reports whether a stored no_speech skip must be re-derived on
// unchanged bytes because the STT identity it was judged with is no longer the
// active one (§8.6.7). It is the no_speech counterpart of the
// derivation-identity gate, which only reads the representations of an ok
// document and so cannot see an item that has none.
func (s *Service) noSpeechStale(existing model.Document) bool {
	if !isNoSpeechSkip(existing) || s.noSpeechSettled(existing) {
		return false
	}
	s.getLogger().Printf("STT identity changed for %s since it was recorded as having no speech; transcribing it again (SPEC §8.6.6/§8.6.7)", existing.RelPath)
	return true
}

// carryNoSpeechSkip keeps an unchanged no_speech document skipped on the full
// read path. The freshly built row is status=ok; without the carry an unchanged
// silent item would be written back as an ok document with no representation.
// It applies only when the content is the one that was judged (the stored
// content_hash done marker matches), the STT identity is unchanged and the run
// is not a forced reindex; otherwise the row stays ok and resolveNeedsProcessing
// re-derives it.
func (s *Service) carryNoSpeechSkip(doc *model.Document, existing model.Document, forceReindex bool) {
	if forceReindex || doc.Status != string(model.DocumentStatusOK) || !isNoSpeechSkip(existing) {
		return
	}
	if existing.ContentHash == "" || existing.ContentHash != doc.ContentHash || !s.noSpeechSettled(existing) {
		return
	}
	doc.Status = existing.Status
	doc.SkipReason = existing.SkipReason
	doc.ErrorMessage = existing.ErrorMessage
}
