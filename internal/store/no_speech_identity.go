package store

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
