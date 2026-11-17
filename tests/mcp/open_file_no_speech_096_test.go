package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC 0.96.0 §8.6.13, §15.4: dir2mcp_open_file on a document that is
// status=skipped with skip_reason=no_speech returns the non-retryable
// MEDIA_NO_TEXT. It never returns the retryable OCR_NOT_READY and never empty
// content. The document row decides, not the file extension.

// seedNoSpeechTranscriptCache writes the cache entry that ingest leaves for a
// decode that found no speech: an empty transcript text and its marker. With
// no STT identity the cache key is the sha256 of the source bytes.
func seedNoSpeechTranscriptCache(t *testing.T, env *openFile1100Env, body string) {
	t.Helper()
	dir := filepath.Join(env.state, "cache", "transcribe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript cache: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	base := filepath.Join(dir, hex.EncodeToString(sum[:]))
	if err := os.WriteFile(base+".txt", nil, 0o644); err != nil {
		t.Fatalf("write empty transcript: %v", err)
	}
	if err := os.WriteFile(base+".nospeech", []byte(model.SkipReasonNoSpeech), 0o644); err != nil {
		t.Fatalf("write no-speech marker: %v", err)
	}
}

// openFileSpans096 are the span kinds of dir2mcp_open_file.
func openFileSpans096(relPath string) map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"no span":   {"rel_path": relPath},
		"line span": {"rel_path": relPath, "start_line": 1, "end_line": 5},
		"page span": {"rel_path": relPath, "page": 1},
		"time span": {"rel_path": relPath, "start_ms": 0, "end_ms": 5000},
	}
}

// assertNoSpeechMediaNoText checks the answer the rule requires.
func assertNoSpeechMediaNoText(t *testing.T, res openFile1100Result) {
	t.Helper()
	if !res.isError {
		t.Fatalf("want an error, got success with content %q", res.content)
	}
	if res.code != "MEDIA_NO_TEXT" || res.retryable {
		t.Fatalf("want the non-retryable MEDIA_NO_TEXT, got code=%q retryable=%v (body=%s)", res.code, res.retryable, res.raw)
	}
	if !strings.Contains(res.raw, "holds no speech") {
		t.Errorf("the message must say that the recording holds no speech: %s", res.raw)
	}
	if strings.Contains(res.raw, "replace mode") {
		t.Errorf("the message must not name the multimodal replace mode: %s", res.raw)
	}
}

// TestOpenFile_NoSpeechSkip_IsMediaNoText covers audio and video, each span
// kind, and the transcript cache entry present and absent. The .mkv row shows
// that the document row decides: this build does not serve that extension as
// media, and the answer is the same.
//
// Mutants killed: serving the empty cached transcript as empty content,
// answering the retryable OCR_NOT_READY on a cache miss, and deciding by the
// file extension.
func TestOpenFile_NoSpeechSkip_IsMediaNoText(t *testing.T) {
	const body = "fake-media-bytes with no speech"
	for _, doc := range []struct{ relPath, docType string }{
		{"music/ambient.mp3", "audio"},
		{"video/clip.mp4", "video"},
		{"video/tape.mkv", "video"},
	} {
		for _, cached := range []bool{true, false} {
			doc, cached := doc, cached
			name := doc.relPath + "/cache absent"
			if cached {
				name = doc.relPath + "/cache present"
			}
			t.Run(name, func(t *testing.T) {
				env := newOpenFile1100Env(t)
				env.writeCorpusFile(t, doc.relPath, body)
				env.seedDocument(t, model.Document{
					RelPath: doc.relPath, DocType: doc.docType, SourceType: "file",
					SizeBytes: int64(len(body)), Status: "skipped", SkipReason: model.SkipReasonNoSpeech,
				})
				if cached {
					seedNoSpeechTranscriptCache(t, env, body)
				}
				for span, args := range openFileSpans096(doc.relPath) {
					t.Run(span, func(t *testing.T) {
						assertNoSpeechMediaNoText(t, callOpenFile1100(t, env.url, args))
					})
				}
			})
		}
	}
}

// TestOpenFile_NoSpeechRuleCoversOnlyNoSpeechDocuments pins the limits of the
// rule. An ok audio document with no no-speech record is not covered: with an
// empty cached transcript it answers as before (the cached text, here empty),
// and with no cache entry it answers the retryable OCR_NOT_READY, because its
// transcript can still arrive. A document skipped for another reason keeps
// DOC_TYPE_UNSUPPORTED.
func TestOpenFile_NoSpeechRuleCoversOnlyNoSpeechDocuments(t *testing.T) {
	const body = "fake-audio-bytes"

	t.Run("ok document with no no-speech record, cached empty transcript", func(t *testing.T) {
		env := newOpenFile1100Env(t)
		env.writeCorpusFile(t, "talk.mp3", body)
		env.seedDocument(t, model.Document{RelPath: "talk.mp3", DocType: "audio", SourceType: "file", SizeBytes: int64(len(body)), Status: "ok"})
		seedNoSpeechTranscriptCache(t, env, body)
		res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "talk.mp3"})
		if res.isError || res.content != "" {
			t.Fatalf("an ok document with no no-speech record is unchanged: want success with the cached (empty) text, got error=%v code=%q content=%q", res.isError, res.code, res.content)
		}
	})

	t.Run("ok document with no no-speech record, no cache entry", func(t *testing.T) {
		env := newOpenFile1100Env(t)
		env.writeCorpusFile(t, "talk.mp3", body)
		env.seedDocument(t, model.Document{RelPath: "talk.mp3", DocType: "audio", SourceType: "file", SizeBytes: int64(len(body)), Status: "ok"})
		res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "talk.mp3"})
		if res.code != "OCR_NOT_READY" || !res.retryable {
			t.Fatalf("an ok document is unchanged: want the retryable OCR_NOT_READY, got code=%q retryable=%v", res.code, res.retryable)
		}
	})

	t.Run("skipped for another reason", func(t *testing.T) {
		env := newOpenFile1100Env(t)
		env.writeCorpusFile(t, "talk.mp3", body)
		env.seedDocument(t, model.Document{
			RelPath: "talk.mp3", DocType: "audio", SourceType: "file", SizeBytes: int64(len(body)),
			Status: "skipped", SkipReason: model.SkipReasonLanguageUncovered,
		})
		res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "talk.mp3"})
		if res.code != "DOC_TYPE_UNSUPPORTED" || res.retryable {
			t.Fatalf("another skip reason is unchanged: want DOC_TYPE_UNSUPPORTED, got code=%q retryable=%v", res.code, res.retryable)
		}
	})
}

// seedNoSpeechRecord writes the per-path record that ingest stores when a
// track of the last transcript derivation found no speech.
func seedNoSpeechRecord(t *testing.T, env *openFile1100Env, relPath string) {
	t.Helper()
	if err := env.store.SetSetting(context.Background(), store.NoSpeechIdentitySettingKey(relPath), "stt:whisper/large-v3"); err != nil {
		t.Fatalf("seed no-speech record for %s: %v", relPath, err)
	}
}

// seedRepresentation096 stores one active representation of repType for
// relPath.
func seedRepresentation096(t *testing.T, env *openFile1100Env, relPath, repType string) {
	t.Helper()
	ctx := context.Background()
	doc, err := env.store.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("get %s: %v", relPath, err)
	}
	if _, err := env.store.UpsertRepresentation(ctx, model.Representation{DocID: doc.DocID, RepType: repType, RepHash: "h-" + repType}); err != nil {
		t.Fatalf("seed %s representation for %s: %v", repType, relPath, err)
	}
}

// okNoSpeechEnv096 is a document that stays status=ok through its recognition
// representation after its only selected track was a no-speech result: the
// no-speech record exists and the document has no transcript. One recognition
// annotation answers the time span [0, 5000] ms.
func okNoSpeechEnv096(t *testing.T, relPath, docType, body string) *openFile1100Env {
	t.Helper()
	env := newOpenFile1100Env(t)
	env.writeCorpusFile(t, relPath, body)
	env.seedDocument(t, model.Document{RelPath: relPath, DocType: docType, SourceType: "file", SizeBytes: int64(len(body)), Status: "ok"})
	seedRepresentation096(t, env, relPath, "recognition")
	seedNoSpeechRecord(t, env, relPath)
	env.svc.SetChunkMetadata(1, model.SearchHit{
		RelPath: relPath,
		Snippet: "home run by Sam Rivera",
		Span:    model.Span{Kind: "time", StartMS: 0, EndMS: 5000},
	})
	return env
}

// TestOpenFile_OKDocumentWithNoSpeechTranscript pins SPEC 0.96.0 §8.6.13,
// §15.4 for a document that stays ok through another representation after
// every selected track was a no-speech result. Its transcript is absent, not
// pending. A call that would return the transcript (the default call or a
// line span on audio, or a time span that no other representation answers)
// returns the non-retryable MEDIA_NO_TEXT, with the transcript cache entry
// present and absent. A time span that recognition answers is answered from
// it. A video keeps its answer for the default call, which never returns a
// transcript.
//
// Mutants killed: serving the empty cached transcript, answering the
// retryable OCR_NOT_READY on a cache miss, and refusing a time span that
// recognition can answer.
func TestOpenFile_OKDocumentWithNoSpeechTranscript(t *testing.T) {
	const body = "fake-media-bytes with no speech"
	for _, cached := range []bool{true, false} {
		cached := cached
		name := "audio/cache absent"
		if cached {
			name = "audio/cache present"
		}
		t.Run(name, func(t *testing.T) {
			env := okNoSpeechEnv096(t, "match.mp3", "audio", body)
			if cached {
				seedNoSpeechTranscriptCache(t, env, body)
			}
			for span, args := range map[string]map[string]interface{}{
				"default call":         {"rel_path": "match.mp3"},
				"line span":            {"rel_path": "match.mp3", "start_line": 1, "end_line": 5},
				"time span, no answer": {"rel_path": "match.mp3", "start_ms": 60000, "end_ms": 65000},
			} {
				t.Run(span, func(t *testing.T) {
					assertNoSpeechMediaNoText(t, callOpenFile1100(t, env.url, args))
				})
			}
			res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "match.mp3", "start_ms": 0, "end_ms": 5000})
			if res.isError || res.content != "home run by Sam Rivera" {
				t.Fatalf("a time span that recognition answers: want its text, got error=%v code=%q content=%q", res.isError, res.code, res.content)
			}
		})
	}

	t.Run("video", func(t *testing.T) {
		env := okNoSpeechEnv096(t, "match.mp4", "video", body)
		assertNoSpeechMediaNoText(t, callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "match.mp4", "start_ms": 60000, "end_ms": 65000}))
		res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "match.mp4", "start_ms": 0, "end_ms": 5000})
		if res.isError || res.content != "home run by Sam Rivera" {
			t.Fatalf("a time span that recognition answers: want its text, got error=%v code=%q content=%q", res.isError, res.code, res.content)
		}
		// The default call on a video never returns a transcript, so its answer
		// is unchanged.
		if res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "match.mp4"}); res.code == "MEDIA_NO_TEXT" || res.code == "OCR_NOT_READY" {
			t.Errorf("the default call on a video is unchanged, got code=%q", res.code)
		}
	})
}

// TestOpenFile_MultiTrackWithOneTranscriptKeepsTheTranscript: the no-speech
// record is one row for the path, not one for each track. A document in which
// one selected track has a transcript and another track was a no-speech
// result holds the record and a transcript. It is not the "every selected
// track" case, so the default call returns the transcript as before.
//
// Mutant killed: deciding by the record alone.
func TestOpenFile_MultiTrackWithOneTranscriptKeepsTheTranscript(t *testing.T) {
	const body = "fake-audio-bytes of two tracks"
	env := newOpenFile1100Env(t)
	env.writeCorpusFile(t, "dub.mp3", body)
	env.seedDocument(t, model.Document{RelPath: "dub.mp3", DocType: "audio", SourceType: "file", SizeBytes: int64(len(body)), Status: "ok"})
	seedRepresentation096(t, env, "dub.mp3", "transcript")
	seedNoSpeechRecord(t, env, "dub.mp3")
	dir := filepath.Join(env.state, "cache", "transcribe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir transcript cache: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".txt"), []byte("[00:00] the first track speaks"), 0o644); err != nil {
		t.Fatalf("write transcript cache: %v", err)
	}

	res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "dub.mp3"})
	if res.isError || !strings.Contains(res.content, "the first track speaks") {
		t.Fatalf("want the transcript of the track that has one, got error=%v code=%q content=%q", res.isError, res.code, res.content)
	}
}
