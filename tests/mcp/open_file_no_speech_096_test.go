package tests

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
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

// TestOpenFile_NoSpeechRuleCoversOnlyTheNoSpeechSkip pins the limits of the
// rule. A document that stays ok with a no-speech track is not covered: an
// audio document with status ok and an empty cached transcript answers as
// before (the cached text, here empty), and with no cache entry it answers
// the retryable OCR_NOT_READY. A document skipped for another reason keeps
// DOC_TYPE_UNSUPPORTED.
func TestOpenFile_NoSpeechRuleCoversOnlyTheNoSpeechSkip(t *testing.T) {
	const body = "fake-audio-bytes"

	t.Run("ok document, cached empty transcript", func(t *testing.T) {
		env := newOpenFile1100Env(t)
		env.writeCorpusFile(t, "talk.mp3", body)
		env.seedDocument(t, model.Document{RelPath: "talk.mp3", DocType: "audio", SourceType: "file", SizeBytes: int64(len(body)), Status: "ok"})
		seedNoSpeechTranscriptCache(t, env, body)
		res := callOpenFile1100(t, env.url, map[string]interface{}{"rel_path": "talk.mp3"})
		if res.isError || res.content != "" {
			t.Fatalf("an ok document is unchanged: want success with the cached (empty) text, got error=%v code=%q content=%q", res.isError, res.code, res.content)
		}
	})

	t.Run("ok document, no cache entry", func(t *testing.T) {
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
