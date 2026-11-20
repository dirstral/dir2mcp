package ingest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
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

// emptyFailedDecode reports whether a windowed decode left windows undecoded
// with no window refused: when such a decode yields no text, the windows that
// answered heard nothing and the others FAILED. That is a provider failure to
// retry, not silence (SPEC §8.6.6: a provider failure is still an error); a
// decode with refused windows stays under §8.6.13/§8.2.2.
func emptyFailedDecode(coverage *TranscriptCoverage) bool {
	return coverage != nil && !coverage.Complete() && len(coverage.Refused) == 0
}

// settleNoSpeechTrack finishes a track whose transcript is empty because the
// provider heard no speech. Any transcript an earlier run derived for the track
// (with another STT identity) is retired, exactly as a §8.6.13 refusal does, so
// a document recorded as having no speech does not keep stale chunks of a
// transcript the current model does not produce. When the empty transcript is a
// §8.2.4 route-fallback result, its stamp is kept for the no_speech row, so the
// item still reaches the fallback retry (noSpeechSettled).
func (s *Service) settleNoSpeechTrack(ctx context.Context, doc model.Document, tc trackContext, decoded cachedTranscript) (bool, bool, string, error) {
	s.getLogger().Printf("stt: no speech in %s%s: the provider answered with an empty transcript or the media has no audio track (SPEC §8.6.6)",
		doc.RelPath, trackSuffixForLog(tc))
	if err := s.retireTrackTranscripts(ctx, doc, tc); err != nil {
		return false, false, "", fmt.Errorf("no-speech outcome for %s: %w", doc.RelPath, err)
	}
	if at, by, ok := decoded.fallbackStamp(); ok {
		s.noSpeechFallbackThisDoc = dueFirstFallback(s.noSpeechFallbackThisDoc,
			noSpeechFallback{ok: true, at: at, by: by}, s.defaultRouteIdentity())
	}
	return false, false, model.SkipReasonNoSpeech, nil
}

// noSpeechFallback is the §8.2.4 stamp (route_fallback_at, route_fallback_by)
// of an empty transcript that a route-fallback decode produced. ok is false
// when the decode was not a fallback result.
type noSpeechFallback struct {
	ok     bool
	at, by string
}

// dueFirstFallback picks, of two fallback stamps of one document's silent
// tracks, the one whose §8.2.4 retry becomes due first, so the single stamp a
// no_speech row keeps never hides a track that is due. A stamp from another
// default profile is due at once (DecideRouteFallback), then an unparseable
// route_fallback_at, then the earliest one; on a tie the kept stamp stays.
func dueFirstFallback(kept, next noSpeechFallback, producer string) noSpeechFallback {
	if !kept.ok {
		return next
	}
	if !next.ok {
		return kept
	}
	keptStale := strings.TrimSpace(kept.by) != strings.TrimSpace(producer)
	nextStale := strings.TrimSpace(next.by) != strings.TrimSpace(producer)
	if keptStale != nextStale {
		if nextStale {
			return next
		}
		return kept
	}
	if fallbackDueAt(next.at).Before(fallbackDueAt(kept.at)) {
		return next
	}
	return kept
}

// fallbackDueAt orders route_fallback_at stamps for dueFirstFallback: an
// unparseable one is due at once, so it sorts first.
func fallbackDueAt(at string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(at))
	if err != nil {
		return time.Time{}
	}
	return t
}

// settleUnrepresentedMedia records the final verdict on a media document that
// produced no representation and that recognition did not rescue. It reports
// whether the document was settled here (so the caller must not also credit it
// as indexed, #426). A no_speech row the store refused is returned as an error,
// so the document is not finalized as done without its durable record.
func (s *Service) settleUnrepresentedMedia(ctx context.Context, doc model.Document, secretPatterns *secretmatch.Matcher, verdict mediaVerdict) (bool, error) {
	switch verdict {
	case mediaVerdictNoRepresentation:
		// No sidecar, no derived transcript, no multimodal keyframe chunks AND no
		// recognition: the video is known but unsearchable, so record it as a
		// durable status="error" diagnostic (#398/#495) while letting the run
		// continue.
		s.getLogger().Printf("no representation produced for video %s: %v", doc.RelPath, errNoVideoRepresentation)
		s.addErrors(1)
		s.persistNonFatalDocError(ctx, doc, errNoVideoRepresentation, secretPatterns)
		return true, nil
	case mediaVerdictNoSpeech:
		return true, s.persistNoSpeechSkip(ctx, doc)
	default:
		return false, nil
	}
}

// persistNoSpeechSkip records the durable no_speech skip. The row's
// error_message carries a short detail naming the STT identity the item was
// judged with, and the §8.2.4 stamp when the empty transcript is a
// route-fallback result, so the incremental gates can tell when the identity
// changed or the fallback retry is due (noSpeechSettled) without a store schema
// change.
//
// Unlike persistNonFatalDocSkip, a failed write is returned: the caller would
// otherwise stamp the done marker on the row's earlier ok state, and an
// unchanged file would then be skipped as indexed with nothing indexed. The skip
// is counted and its file_skip raised only once the store accepted the row, and
// never for an archive member, which the container accounts for (SPEC §3.2).
func (s *Service) persistNoSpeechSkip(ctx context.Context, doc model.Document) error {
	doc.Status = string(model.DocumentStatusSkipped)
	doc.SkipReason = model.SkipReasonNoSpeech
	doc.ErrorMessage = s.noSpeechDetail()
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("persist no_speech skip for %s: %w", doc.RelPath, err)
	}
	// Keep the live dedup map equal to the row this write produced (#691).
	s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
	if s.archiveMemberThisDoc {
		return nil
	}
	s.addSkipped(1)
	s.notifyDocumentSkip(doc)
	return nil
}

// noSpeechIdentityPrefix opens the detail persistNoSpeechSkip writes. The token
// and the fallback stamp come first so the store's 512-byte cap on
// error_message can never cut them; noSpeechRecordEnd closes them.
const (
	noSpeechIdentityPrefix = "no speech (stt_identity="
	noSpeechRecordEnd      = "): "
	noSpeechFallbackAt     = "; route_fallback_at="
	noSpeechFallbackBy     = "; route_fallback_by="
)

// noSpeechDetail renders the detail recorded on a no_speech row: a stable token
// of the active STT identity, the §8.2.4 stamp of a route-fallback result, and
// the provider and model in plain words.
func (s *Service) noSpeechDetail() string {
	record := s.noSpeechIdentityToken()
	if fb := s.noSpeechFallbackThisDoc; fb.ok {
		record += noSpeechFallbackAt + fb.at + noSpeechFallbackBy + fb.by
	}
	return fmt.Sprintf("%s%s%sthe STT provider answered with no speech, or the media has no audio track (SPEC §8.6.6); judged with %s/%s",
		noSpeechIdentityPrefix, record, noSpeechRecordEnd, strings.TrimSpace(s.sttProvider), strings.TrimSpace(s.sttModel))
}

// noSpeechIdentityToken is a short, stable token of the active transcript
// derivation identity (§8.6.7) and of the media.stt.tracks selection (§8.6.12)
// the verdict was judged over: a track the old selection did not decode may
// hold the speech. A hash, not the identity itself, because under window scope
// the identity carries the whole route table and is unbounded. The per-track
// transcript cache keys are unchanged, so a new selection decodes only the
// tracks it adds.
func (s *Service) noSpeechIdentityToken() string {
	return computeContentHash([]byte(s.activeTranscriptIdentity() + "\x00tracks=" + s.sttTrackSelectionKey()))[:16]
}

// sttTrackSelectionKey renders the media.stt.tracks selection in a canonical
// form, so an unset selection and an explicit "first" are the same key.
func (s *Service) sttTrackSelectionKey() string {
	sel, err := config.ParseSTTTracks(s.cfg.MediaSTTTracks)
	if err != nil {
		return strings.Join(s.cfg.MediaSTTTracks, ",")
	}
	return fmt.Sprintf("%d:%v", sel.Mode, sel.Indices)
}

// recordedNoSpeech reads the identity token and the fallback stamp back from a
// no_speech row's detail. ok is false when the detail carries no token.
func recordedNoSpeech(detail string) (token string, fb noSpeechFallback, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(detail), noSpeechIdentityPrefix)
	if !found {
		return "", noSpeechFallback{}, false
	}
	record, _, found := strings.Cut(rest, noSpeechRecordEnd)
	if !found {
		return "", noSpeechFallback{}, false
	}
	token, stamp, hasStamp := strings.Cut(record, noSpeechFallbackAt)
	if hasStamp {
		fb.ok = true
		fb.at, fb.by, _ = strings.Cut(stamp, noSpeechFallbackBy)
	}
	return token, fb, token != ""
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
//
// Silence a §8.2.4 route-fallback decode produced is provisional like any
// fallback result: it does not stand when the fallback retry is due
// (on_route_error: fail, route_retry_after elapsed, or route_fallback_by is not
// the current default profile). The item is then processed, and the cached
// fallback result goes through the retry in serveCachedTranscript.
func (s *Service) noSpeechSettled(existing model.Document) bool {
	if s.transcriber == nil {
		return true
	}
	recorded, fb, ok := recordedNoSpeech(existing.ErrorMessage)
	if !ok || recorded != s.noSpeechIdentityToken() {
		return false
	}
	if !fb.ok {
		return true
	}
	if v := s.routeFallbackVerdict(fb.at, fb.by); v.Due {
		s.getLogger().Printf("route fallback: %s was recorded as having no speech from a route-fallback decode (%s); decoding it again on its route this scan (SPEC §8.2.4)", existing.RelPath, v.Why)
		return false
	}
	return true
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
	s.getLogger().Printf("%s was recorded as having no speech under another STT identity, or from a route-fallback decode now due; transcribing it again (SPEC §8.6.6/§8.6.7/§8.2.4)", existing.RelPath)
	return true
}

// archiveHasStaleNoSpeechMember reports whether an archive container holds a
// no_speech member that must be re-derived (noSpeechStale). The members of an
// unchanged container are otherwise retained without being processed, so a
// member's STT identity change or due fallback retry would never be seen. With
// no transcriber configured nothing is stale, and the store is not read.
func (s *Service) archiveHasStaleNoSpeechMember(ctx context.Context, archiveRelPath string) bool {
	if s.transcriber == nil {
		return false
	}
	const pageSize = 500
	for offset := 0; ; offset += pageSize {
		docs, _, err := s.store.ListFiles(ctx, archiveRelPath+"/", "", pageSize, offset)
		if err != nil {
			s.getLogger().Printf("check archive members of %s for a stale no_speech outcome: %v", archiveRelPath, err)
			return false
		}
		for _, d := range docs {
			if s.noSpeechStale(d) {
				return true
			}
		}
		if len(docs) < pageSize {
			return false
		}
	}
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
