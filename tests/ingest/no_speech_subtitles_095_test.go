package tests

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC §8.6.13 (0.95.0): subtitle files that write-back (§8.6.14) wrote for a
// track in an earlier run remain when the track becomes a no-speech result.
// The implementation MUST NOT delete them, and their emitted_artifacts rows
// remain, so the files stay owned and are never read as authored sidecars.

// TestNoSpeech_WrittenSubtitlesAndTheirRowsRemain: run 1 indexes a transcript
// and writes its subtitles. In run 2 another STT model finds no speech in the
// same bytes, so the transcript is retired and the document is a no_speech
// skip. Each subtitle file is still on disk with the same bytes and each
// emitted_artifacts row is unchanged. Run 3 (nothing changed) does not decode
// again, keeps the files and the rows, and does not read an owned file as an
// authored sidecar.
//
// Mutants killed: deleting the owned files with the retired transcript,
// dropping the ownership rows of a skipped document, and binding an owned
// file as a sidecar once its transcript is gone.
func TestNoSpeech_WrittenSubtitlesAndTheirRowsRemain(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"if_missing", "refresh"} {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newEmitHarness(t, func(cfg *config.Config) { cfg.MediaSubtitlesEmitPolicy = policy })
			mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))

			svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
			if err := svc.Run(ctx); err != nil {
				t.Fatalf("run 1: %v", err)
			}
			rowsBefore := emitRows(t, h.store)
			if _, ok := rowsBefore["audio/one.de.vtt"]; !ok || len(rowsBefore) < 2 {
				t.Fatalf("run 1 wrote rows %v, want the source VTT and more; the fixture is wrong", rowsBefore)
			}
			bytesBefore := map[string]string{}
			for rel := range rowsBefore {
				bytesBefore[rel] = readFile(t, filepath.Join(h.root, filepath.FromSlash(rel)))
			}

			// Run 2: another model decodes the same bytes and finds no speech.
			svc2, tr2 := h.service(t, "", "whisper-large-v4")
			if err := svc2.Run(ctx); err != nil {
				t.Fatalf("run 2: %v", err)
			}
			if tr2.calls == 0 {
				t.Fatalf("run 2 did not decode: the identity change must decode the recording again")
			}
			assertNoSpeechKeepsSubtitles(t, h, rowsBefore, bytesBefore, "after the no-speech run")

			// Run 3: nothing changed. The recording is not decoded again, and the
			// owned files are not bound as authored sidecars.
			svc3, tr3 := h.service(t, "", "whisper-large-v4")
			if err := svc3.Run(ctx); err != nil {
				t.Fatalf("run 3: %v", err)
			}
			if tr3.calls != 0 {
				t.Errorf("run 3 decoded %d time(s), want 0 (content and STT identity are unchanged)", tr3.calls)
			}
			assertNoSpeechKeepsSubtitles(t, h, rowsBefore, bytesBefore, "after the unchanged run")
		})
	}
}

// assertNoSpeechKeepsSubtitles checks the state of audio/one.mp3 after a
// no-speech outcome: a no_speech skip with no transcript, every written
// subtitle file on disk with its bytes, and every ownership row as it was.
func assertNoSpeechKeepsSubtitles(t *testing.T, h *emitHarness, rowsBefore map[string]store.EmittedArtifact, bytesBefore map[string]string, when string) {
	t.Helper()
	doc := mustGetDoc(t, h.store, "audio/one.mp3")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Fatalf("%s: status=%q skip_reason=%q error=%q, want skipped/no_speech", when, doc.Status, doc.SkipReason, doc.ErrorMessage)
	}
	if meta, err := h.store.RepresentationMetaByType(context.Background(), "audio/one.mp3", ingest.RepTypeTranscript); err == nil && meta != "" {
		t.Errorf("%s: a transcript is live (meta %s): the earlier one is not retired, or an owned subtitle was read as a sidecar", when, meta)
	}
	for rel, want := range bytesBefore {
		path := filepath.Join(h.root, filepath.FromSlash(rel))
		if !fileExists(path) {
			t.Errorf("%s: %s was deleted; written subtitles must remain", when, rel)
			continue
		}
		if got := readFile(t, path); got != want {
			t.Errorf("%s: %s changed", when, rel)
		}
	}
	if rowsAfter := emitRows(t, h.store); !reflect.DeepEqual(rowsAfter, rowsBefore) {
		t.Errorf("%s: emitted_artifacts rows changed:\n got %+v\nwant %+v", when, rowsAfter, rowsBefore)
	}
}
