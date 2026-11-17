package store

import (
	"context"
	"errors"
	"os"
	"strings"
)

// noSpeechIdentitySettingPrefix keys the record of the STT derivation identity
// that found no speech in a document (SPEC §8.6.13). A no-speech track has no
// transcript representation whose meta_json could carry its identity, so the
// record is one settings row per rel_path. Ingest writes it when a decode finds
// no speech and deletes it whenever the document's transcript is derived again;
// MarkDocumentDeleted deletes it with the document.
const noSpeechIdentitySettingPrefix = "no_speech_identity:"

// NoSpeechIdentitySettingKey is the settings key of relPath's no-speech
// record. The path is normalized the way the documents table stores it, so
// ingest and MarkDocumentDeleted name the same row.
func NoSpeechIdentitySettingKey(relPath string) string {
	if normalized, err := normalizeRelPath(relPath); err == nil {
		relPath = normalized
	}
	return noSpeechIdentitySettingPrefix + relPath
}

// NoSpeechTranscriptAbsent reports whether every selected audio track of the
// document at relPath was a no-speech result, so the document has no
// transcript and none is pending (SPEC §8.6.13). Two facts decide it:
//
//   - the no-speech record of the path exists. Ingest writes it when a track
//     of the last transcript derivation found no speech, and deletes it when
//     the transcript is derived again;
//   - the document has no active transcript representation (the source
//     transcript of a track, a translation, or a sidecar transcript).
//
// The record is one row for the path, not one for each track, so the record
// alone cannot tell "every track holds no speech" from "one track holds no
// speech and another has a transcript". The second fact tells them apart: a
// document that keeps a transcript of any track is not this case.
func (s *SQLiteStore) NoSpeechTranscriptAbsent(ctx context.Context, relPath string) (bool, error) {
	recorded, err := s.GetSetting(ctx, NoSpeechIdentitySettingKey(relPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if strings.TrimSpace(recorded) == "" {
		return false, nil
	}
	types, err := s.RepresentationTypesByPath(ctx, relPath)
	if err != nil {
		return false, err
	}
	for _, t := range types {
		if isTranscriptRepType(t) {
			return false, nil
		}
	}
	return true, nil
}

// isTranscriptRepType reports whether repType is a transcript representation:
// "transcript", a translation ("transcript-<lang>") or another audio track
// ("transcript@t<N>", with or without a language suffix).
func isTranscriptRepType(repType string) bool {
	repType = strings.ToLower(strings.TrimSpace(repType))
	return repType == "transcript" || strings.HasPrefix(repType, "transcript-") || strings.HasPrefix(repType, "transcript@")
}
