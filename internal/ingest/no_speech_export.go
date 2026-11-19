package ingest

// NoSpeechFallbackDueFirstForTest exposes the choice a multi-track no_speech
// row makes between two §8.2.4 fallback stamps (a, the stamp already kept, and
// b, the next track's) under the given default profile, for tests in the
// tests/ tree. It returns the stamp kept.
func NoSpeechFallbackDueFirstForTest(producer, aAt, aBy, bAt, bBy string) (string, string) {
	kept := dueFirstFallback(noSpeechFallback{ok: true, at: aAt, by: aBy}, noSpeechFallback{ok: true, at: bAt, by: bBy}, producer)
	return kept.at, kept.by
}
