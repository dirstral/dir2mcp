package tests

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// sttServiceWithDetection mirrors sttService (derivation_identity_test.go) but
// turns on §8.8 best-effort language auto-detection.
func sttServiceWithDetection(t *testing.T, root, stateDir string, st model.Store, sttModel, language string, tr *fakeTranscriber) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, config.Config{
		RootDir:                  root,
		StateDir:                 stateDir,
		STTProvider:              "off",
		LanguageDetectionEnabled: true,
	}, st)
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", sttModel)
	svc.SetTranscriptLanguage(language)
	return svc
}

// A clearly-English transcript (with a leading timestamp, as a real transcript
// carries) long enough for trigram detection.
const englishTranscript = "[00:00] The committee reviewed the annual report in detail, and several members raised concerns about the regional budget allocations before the proposal was finally approved by a clear majority."

// TestTranscriptLanguageDetected_WhenNoPin: with no operator pin, the STT
// transcript records a best-effort detected language (§8.8 `detected`).
func TestTranscriptLanguageDetected_WhenNoPin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	svc := sttServiceWithDetection(t, root, t.TempDir(), st, "whisper-large-v3", "", &fakeTranscriber{text: englishTranscript})

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(transcriptMetaFor(t, st, "talk.mp3")), &p); err != nil {
		t.Fatalf("meta not json: %v", err)
	}
	if p["language"] != "en" {
		t.Errorf("language = %v, want en", p["language"])
	}
	if p["language_source"] != "detected" {
		t.Errorf("language_source = %v, want detected", p["language_source"])
	}
}

// TestTranscriptPin_NotOverriddenByDetection: an operator pin (configured) wins
// over detection even when the audio is in a different language (§8.8 precedence).
func TestTranscriptPin_NotOverriddenByDetection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	// English audio, but the operator pinned French — the pin must win.
	svc := sttServiceWithDetection(t, root, t.TempDir(), st, "whisper-large-v3", "fr", &fakeTranscriber{text: englishTranscript})

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(transcriptMetaFor(t, st, "talk.mp3")), &p); err != nil {
		t.Fatalf("meta not json: %v", err)
	}
	if p["language"] != "fr" {
		t.Errorf("language = %v, want fr (pin must win over detection)", p["language"])
	}
	if p["language_source"] != "configured" {
		t.Errorf("language_source = %v, want configured", p["language_source"])
	}
}

// TestTranscriptDetectedLanguage_DoesNotForceReDerivation is the critical guard
// for the §8.8 decoupling: a detected language is NOT part of the derivation
// identity, so re-scanning with the same STT model must NOT re-transcribe. Without
// the decoupling, the recorded identity (language=en) would differ from the active
// identity (no pin), spuriously re-transcribing the whole corpus.
func TestTranscriptDetectedLanguage_DoesNotForceReDerivation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	tr1 := &fakeTranscriber{text: englishTranscript}
	svc1 := sttServiceWithDetection(t, root, stateDir, st, "whisper-large-v3", "", tr1)
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	if tr1.calls != 1 {
		t.Fatalf("scan1 STT calls = %d, want 1", tr1.calls)
	}
	if meta := transcriptMetaFor(t, st, "talk.mp3"); !strings.Contains(meta, `"language_source":"detected"`) {
		t.Fatalf("scan1 expected a detected language, got meta=%q", meta)
	}

	// Same model + same content: identity must match → no re-transcription.
	tr2 := &fakeTranscriber{text: englishTranscript}
	svc2 := sttServiceWithDetection(t, root, stateDir, st, "whisper-large-v3", "", tr2)
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if tr2.calls != 0 {
		t.Fatalf("scan2 STT calls = %d, want 0 (a detected language must NOT be part of the derivation identity)", tr2.calls)
	}
}
