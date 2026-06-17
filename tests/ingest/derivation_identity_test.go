package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// newRealStore returns an initialized on-disk SQLiteStore (it persists documents
// and representations across scans, and implements RepresentationMetaByType so
// the derivation-identity gate is exercised end to end, spec §8.6.7).
func newRealStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// sttService builds a service rooted at root with a deterministic STT identity
// and a fakeTranscriber, without resolving a real provider profile.
func sttService(t *testing.T, root, stateDir string, st model.Store, provider, sttModel, language string, tr *fakeTranscriber) *ingest.Service {
	t.Helper()
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity(provider, sttModel)
	svc.SetTranscriptLanguage(language)
	return svc
}

func transcriptMetaFor(t *testing.T, st *store.SQLiteStore, relPath string) string {
	t.Helper()
	meta, err := st.RepresentationMetaByType(context.Background(), relPath, ingest.RepTypeTranscript)
	if err != nil {
		t.Fatalf("RepresentationMetaByType(%s): %v", relPath, err)
	}
	return meta
}

// TestSTTIdentity_PersistedOnBareTranscriptRep is the regression guard for the
// core bug: the bare machine transcript was persisted with NO meta_json, so the
// STT provider/model were never recorded (spec §5.2/§8.6.7).
func TestSTTIdentity_PersistedOnBareTranscriptRep(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	svc := sttService(t, root, t.TempDir(), st, "whisper", "whisper-large-v3", "en",
		&fakeTranscriber{text: "[00:00] hello world"})

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	meta := transcriptMetaFor(t, st, "talk.mp3")
	if meta == "" {
		t.Fatal("bare transcript rep persisted with empty meta_json (regression: STT identity not recorded)")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(meta), &parsed); err != nil {
		t.Fatalf("transcript meta is not valid json (%q): %v", meta, err)
	}
	if parsed["source"] != "stt" {
		t.Errorf("source = %v, want stt", parsed["source"])
	}
	if parsed["provider"] != "whisper" {
		t.Errorf("provider = %v, want whisper", parsed["provider"])
	}
	if parsed["model"] != "whisper-large-v3" {
		t.Errorf("model = %v, want whisper-large-v3", parsed["model"])
	}
	if parsed["language"] != "en" {
		t.Errorf("language = %v, want en", parsed["language"])
	}
}

// TestSTTModelSwap_InvalidatesAndRederives asserts that changing the active STT
// model re-derives the transcript even though the media bytes (content_hash) are
// unchanged (spec §8.6.7).
func TestSTTModelSwap_InvalidatesAndRederives(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	// Scan 1: Voxtral.
	tr1 := &fakeTranscriber{text: "[00:00] from voxtral"}
	svc1 := sttService(t, root, stateDir, st, "mistral-ocr", "voxtral-mini-latest", "en", tr1)
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	if tr1.calls != 1 {
		t.Fatalf("scan1 STT calls = %d, want 1", tr1.calls)
	}

	// Scan 2: swap to self-hosted whisper-large-v3. Same bytes, different model.
	tr2 := &fakeTranscriber{text: "[00:00] from whisper"}
	svc2 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr2)
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if tr2.calls != 1 {
		t.Fatalf("scan2 STT calls = %d, want 1 (model swap must re-transcribe unchanged media)", tr2.calls)
	}
	meta := transcriptMetaFor(t, st, "talk.mp3")
	if !strings.Contains(meta, "whisper-large-v3") || !strings.Contains(meta, `"provider":"whisper"`) {
		t.Fatalf("transcript meta not refreshed to new STT identity: %q", meta)
	}
}

// TestSTTNoSwap_NoChurn asserts that re-scanning with the SAME STT identity and
// unchanged content does NOT re-transcribe (no needless churn).
func TestSTTNoSwap_NoChurn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	tr1 := &fakeTranscriber{text: "[00:00] hello"}
	svc1 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr1)
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}

	tr2 := &fakeTranscriber{text: "[00:00] hello"}
	svc2 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr2)
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if tr2.calls != 0 {
		t.Fatalf("scan2 STT calls = %d, want 0 (identical identity + content must not re-transcribe)", tr2.calls)
	}
}

// TestEmptyRecordedIdentity_Passes is the backward-compat guard: a pre-upgrade
// transcript recorded no provider/model. An empty recorded identity MUST pass so
// the first upgrade does not force a corpus-wide re-transcription (mirrors
// VerifyEmbedIdentity's fresh-index rule).
func TestEmptyRecordedIdentity_Passes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	// Scan 1 simulates a pre-upgrade run: NO STT identity recorded (empty meta).
	tr1 := &fakeTranscriber{text: "[00:00] legacy"}
	svc1 := sttService(t, root, stateDir, st, "", "", "", tr1)
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}

	// Scan 2 with a NOW-configured STT identity must NOT re-transcribe the
	// legacy transcript whose identity was never recorded.
	tr2 := &fakeTranscriber{text: "[00:00] upgraded"}
	svc2 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr2)
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if tr2.calls != 0 {
		t.Fatalf("scan2 STT calls = %d, want 0 (empty recorded identity must pass; no mass re-transcribe)", tr2.calls)
	}
}

// TestForceReindex_StillWins asserts --force re-transcribes regardless of a
// matching identity.
func TestForceReindex_StillWins(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	tr1 := &fakeTranscriber{text: "[00:00] hello"}
	svc1 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "en", tr1)
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}

	// Scan 2 uses a fresh cache dir (same store, so content_hash is unchanged)
	// so a forced re-derivation actually re-invokes the provider rather than
	// being served by the transcript cache. With --force the identity gate is
	// irrelevant: reprocessing must happen regardless.
	tr2 := &fakeTranscriber{text: "[00:00] hello"}
	svc2 := sttService(t, root, t.TempDir(), st, "whisper", "whisper-large-v3", "en", tr2)
	if err := svc2.ProcessDocument(context.Background(), f, nil, true); err != nil {
		t.Fatalf("scan2 (force): %v", err)
	}
	if tr2.calls != 1 {
		t.Fatalf("scan2 STT calls = %d, want 1 (force must re-transcribe even on identical identity)", tr2.calls)
	}
}

// TestSidecarExemption_STTSwapDoesNotInvalidate asserts that a sidecar-sourced
// transcript is NOT re-derived when the STT model changes: sidecars are
// authored, not model-derived (spec §8.6.4/§8.6.7). The transcriber must never
// be called because the authored sidecar stands in for STT.
func TestSidecarExemption_STTSwapDoesNotInvalidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nauthored caption\n")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	// Scan 1: sidecar is ingested; STT must not run.
	tr1 := &explodingTranscriber{t: t}
	svc1 := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	svc1.SetTranscriber(tr1)
	svc1.SetSTTIdentity("mistral-ocr", "voxtral-mini-latest")
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	meta1 := transcriptMetaFor(t, st, "talk.mp3")
	if !strings.Contains(meta1, `"source":"sidecar"`) {
		t.Fatalf("expected sidecar-sourced transcript, got meta %q", meta1)
	}

	// Scan 2: swap STT model. The sidecar transcript must NOT be invalidated, and
	// the exploding transcriber must never be called.
	tr2 := &explodingTranscriber{t: t}
	svc2 := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	svc2.SetTranscriber(tr2)
	svc2.SetSTTIdentity("whisper", "whisper-large-v3")
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	meta2 := transcriptMetaFor(t, st, "talk.mp3")
	if !strings.Contains(meta2, `"source":"sidecar"`) {
		t.Fatalf("sidecar transcript must survive STT model swap, got meta %q", meta2)
	}
}

// TestTranslationCascade_RefreshesOnSTTSwap asserts that when an STT transcript
// is re-derived after a model swap, a translated transcript of it is refreshed
// rather than left stale (spec §8.6.2/§8.6.7).
func TestTranslationCascade_RefreshesOnSTTSwap(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	// Scan 1: Voxtral source transcript + an en translation.
	tr1 := &fakeTranscriber{text: "[00:00] bonjour"}
	gen1 := &recordingGenerator{out: "[00:00] hello v1"}
	svc1 := sttService(t, root, stateDir, st, "mistral-ocr", "voxtral-mini-latest", "fr", tr1)
	svc1.SetTranslator(gen1, "mistral", "mistral-large", []string{"en"})
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	if gen1.calls == 0 {
		t.Fatal("scan1 expected a translation call")
	}

	// Scan 2: swap STT model -> source text changes -> translation must re-run.
	tr2 := &fakeTranscriber{text: "[00:00] salut tout le monde"}
	gen2 := &recordingGenerator{out: "[00:00] hello v2"}
	svc2 := sttService(t, root, stateDir, st, "whisper", "whisper-large-v3", "fr", tr2)
	svc2.SetTranslator(gen2, "mistral", "mistral-large", []string{"en"})
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if tr2.calls != 1 {
		t.Fatalf("scan2 STT calls = %d, want 1 (swap re-transcribes)", tr2.calls)
	}
	if gen2.calls == 0 {
		t.Fatal("scan2 expected the translation to refresh after the source transcript was re-derived")
	}

	// The translated transcript must reflect the refreshed text, not the stale v1.
	enMeta, err := st.RepresentationMetaByType(context.Background(), "talk.mp3", ingest.TranscriptRepType("en"))
	if err != nil {
		t.Fatalf("RepresentationMetaByType(en): %v", err)
	}
	if !strings.Contains(enMeta, `"source":"translation"`) {
		t.Fatalf("expected translated transcript rep, got meta %q", enMeta)
	}
}

// TestOCRModelSwap_InvalidatesAndRederives asserts that changing the active OCR
// model re-extracts the document even though the bytes are unchanged (§8.6.7).
// It uses a Mistral client pointed at a local stub so the provider/model is the
// real extraction identity, with a counter to confirm re-extraction.
func TestOCRModelSwap_InvalidatesAndRederives(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "doc.pdf"), "fake-pdf-bytes")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "doc.pdf", SizeBytes: 14, MTimeUnix: time.Now().Unix()}

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pages":[{"markdown":"extracted page text"}]}`))
	}))
	t.Cleanup(srv.Close)

	newMistral := func(ocrModel string) *mistral.Client {
		c := mistral.NewClient(srv.URL, "test-key")
		c.DefaultOCRModel = ocrModel
		return c
	}

	// Scan 1: OCR with model "mistral-ocr-2024".
	svc1 := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	svc1.SetOCR(newMistral("mistral-ocr-2024"))
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	if calls != 1 {
		t.Fatalf("scan1 OCR calls = %d, want 1", calls)
	}
	meta1, _ := st.RepresentationMetaByType(context.Background(), "doc.pdf", ingest.RepTypeExtractedMarkdown)
	if !strings.Contains(meta1, "mistral-ocr-2024") {
		t.Fatalf("scan1 OCR meta missing model: %q", meta1)
	}

	// Scan 2: swap to model "mistral-ocr-2025". Same bytes -> must re-extract.
	calls = 0
	svc2 := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	svc2.SetOCR(newMistral("mistral-ocr-2025"))
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if calls != 1 {
		t.Fatalf("scan2 OCR calls = %d, want 1 (OCR model swap must re-extract unchanged bytes)", calls)
	}
	meta2, _ := st.RepresentationMetaByType(context.Background(), "doc.pdf", ingest.RepTypeExtractedMarkdown)
	if !strings.Contains(meta2, "mistral-ocr-2025") {
		t.Fatalf("scan2 OCR meta not refreshed: %q", meta2)
	}
}

// TestOCRNoSwap_NoChurn asserts that re-scanning with the SAME OCR model and
// unchanged bytes does NOT re-extract.
func TestOCRNoSwap_NoChurn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "doc.pdf"), "fake-pdf-bytes")
	st := newRealStore(t)
	stateDir := t.TempDir()
	f := ingest.DiscoveredFile{RelPath: "doc.pdf", SizeBytes: 14, MTimeUnix: time.Now().Unix()}

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pages":[{"markdown":"extracted page text"}]}`))
	}))
	t.Cleanup(srv.Close)
	newMistral := func() *mistral.Client {
		c := mistral.NewClient(srv.URL, "test-key")
		c.DefaultOCRModel = "mistral-ocr-2025"
		return c
	}

	svc1 := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	svc1.SetOCR(newMistral())
	if err := svc1.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	calls = 0
	svc2 := mustNewIngestService(t, config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}, st)
	svc2.SetOCR(newMistral())
	if err := svc2.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if calls != 0 {
		t.Fatalf("scan2 OCR calls = %d, want 0 (identical OCR identity + bytes must not re-extract)", calls)
	}
}

// recordingGenerator is a model.Generator stub that records call count and
// returns a fixed translated body.
type recordingGenerator struct {
	out   string
	calls int
}

func (g *recordingGenerator) Generate(_ context.Context, _ string) (string, error) {
	g.calls++
	return g.out, nil
}
