package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
)

// repSnapshot is a deterministic, store-independent fingerprint of one
// representation and its chunks. It is the unit of comparison for the two-phase
// vs single-pass equivalence property (SPEC §8.6.11): two passes must produce the
// SAME representations/chunks as a single pass.
type repSnapshot struct {
	RepType string
	Meta    string
	Chunks  []chunkSnapshot
}

type chunkSnapshot struct {
	Ordinal   int
	Text      string
	TextHash  string
	IndexKind string
}

// snapshotDoc collects every active transcript representation for a document plus
// each representation's ordered chunks into a deterministic slice, so two runs can
// be compared for byte-equivalence of their derived output.
func snapshotDoc(t *testing.T, st *store.SQLiteStore, relPath string) []repSnapshot {
	t.Helper()
	ctx := context.Background()
	reps, err := st.TranscriptRepresentations(ctx, relPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("TranscriptRepresentations(%s): %v", relPath, err)
	}
	out := make([]repSnapshot, 0, len(reps))
	for _, r := range reps {
		chunks, err := st.GetChunksByRepID(ctx, r.RepID)
		if err != nil {
			t.Fatalf("GetChunksByRepID(%d): %v", r.RepID, err)
		}
		cs := make([]chunkSnapshot, 0, len(chunks))
		for _, c := range chunks {
			cs = append(cs, chunkSnapshot{
				Ordinal:   c.Ordinal,
				Text:      c.Text,
				TextHash:  c.TextHash,
				IndexKind: c.IndexKind,
			})
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].Ordinal < cs[j].Ordinal })
		out = append(out, repSnapshot{
			// rep_type is encoded in meta_json (language + source); use the normalized
			// meta as the stable rep identity so source/translated reps are
			// distinguishable without an extra store accessor.
			RepType: repTypeFromMeta(r.MetaJSON),
			Meta:    normalizeMeta(t, r.MetaJSON),
			Chunks:  cs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RepType < out[j].RepType })
	return out
}

// repTypeFromMeta derives a stable rep identity label from a transcript rep's
// meta_json (source + language), independent of the store's internal rep_type
// column. It is used only to order/compare snapshots deterministically.
func repTypeFromMeta(metaJSON string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(metaJSON), &m); err != nil {
		return metaJSON
	}
	source, _ := m["source"].(string)
	lang, _ := m["language"].(string)
	return fmt.Sprintf("%s/%s", source, lang)
}

// normalizeMeta re-marshals meta_json with sorted keys so two semantically-equal
// metas compare equal regardless of field order.
func normalizeMeta(t *testing.T, metaJSON string) string {
	t.Helper()
	if strings.TrimSpace(metaJSON) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(metaJSON), &m); err != nil {
		t.Fatalf("meta_json not valid json (%q): %v", metaJSON, err)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal meta: %v", err)
	}
	return string(out)
}

// twoPhaseHarness builds an identical corpus + service for an equivalence run and
// returns the corpus rel paths plus the store for snapshotting.
type twoPhaseHarness struct {
	root     string
	stateDir string
	store    *store.SQLiteStore
	svc      *ingest.Service
	relPaths []string
}

// newTwoPhaseHarness writes a fixed corpus (two text files + two audio files) and
// wires a service with a deterministic STT identity, fake transcriber, and fake
// translator (target "en"; source language "de"). twoPhase selects the pass mode.
// A fresh translator instance is returned so callers can assert call counts.
func newTwoPhaseHarness(t *testing.T, twoPhase bool) (*twoPhaseHarness, *fakeTranslator) {
	t.Helper()
	root := t.TempDir()
	stateDir := t.TempDir()

	rel := []string{"notes/a.txt", "notes/b.txt", "audio/one.mp3", "audio/two.mp3"}
	mustWriteFile(t, filepath.Join(root, "notes", "a.txt"), []byte("alpha text body"))
	mustWriteFile(t, filepath.Join(root, "notes", "b.txt"), []byte("beta text body"))
	mustWriteFile(t, filepath.Join(root, "audio", "one.mp3"), []byte("fake-audio-one"))
	mustWriteFile(t, filepath.Join(root, "audio", "two.mp3"), []byte("fake-audio-two"))

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		RootDir:            root,
		StateDir:           stateDir,
		STTProvider:        "off",
		MediaBatchTwoPhase: twoPhase,
	}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro line\n[00:02] second line\n[00:05] third line"})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("de")
	tr := &fakeTranslator{}
	svc.SetTranslator(tr, "mistral", "mistral-small-2506", []string{"en"})

	return &twoPhaseHarness{root: root, stateDir: stateDir, store: st, svc: svc, relPaths: rel}, tr
}

// TestTwoPhase_EquivalentToSinglePass is the core property test (SPEC §8.6.11):
// running media ingest as two ordered passes (transcription then derivation) MUST
// produce the SAME representations and chunks as a single pass over the same
// corpus. Only ordering/reporting differ — never the derived output.
func TestTwoPhase_EquivalentToSinglePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	single, _ := newTwoPhaseHarness(t, false)
	if err := single.svc.Run(ctx); err != nil {
		t.Fatalf("single-pass Run: %v", err)
	}

	two, _ := newTwoPhaseHarness(t, true)
	if err := two.svc.Run(ctx); err != nil {
		t.Fatalf("two-phase Run: %v", err)
	}

	for _, rel := range single.relPaths {
		got := snapshotDoc(t, two.store, rel)
		want := snapshotDoc(t, single.store, rel)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("two-phase output for %s differs from single-pass.\n two-phase: %#v\n single  : %#v", rel, got, want)
		}
	}

	// Sanity: the audio docs must actually have produced BOTH a source and a
	// translated transcript, otherwise the equivalence above is vacuous.
	for _, rel := range []string{"audio/one.mp3", "audio/two.mp3"} {
		snap := snapshotDoc(t, single.store, rel)
		if len(snap) != 2 {
			t.Fatalf("expected source + translated transcript for %s, got %d: %#v", rel, len(snap), snap)
		}
	}
}

// TestTwoPhase_DerivationDoesNotRetranscribe pins the resumability property (SPEC
// §8.6.11 / §7.6/§8.6.7): the derivation pass reuses the cached source transcript
// and never re-invokes the transcriber. With two audio files, the transcriber is
// called exactly once per file across BOTH passes — the derivation pass adds zero
// transcribe calls.
func TestTwoPhase_DerivationDoesNotRetranscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h, _ := newTwoPhaseHarness(t, true)
	tr := &fakeTranscriber{text: "[00:00] one\n[00:02] two"}
	h.svc.SetTranscriber(tr)

	if err := h.svc.Run(ctx); err != nil {
		t.Fatalf("two-phase Run: %v", err)
	}

	// Two audio assets -> the transcription pass transcribes each once; the
	// derivation pass hits the transcript cache and must NOT transcribe again.
	if tr.calls != 2 {
		t.Fatalf("expected exactly 2 transcribe calls (one per audio asset, none in derivation pass), got %d", tr.calls)
	}
}

// TestTwoPhase_ManifestRecordsCarryPass pins that, under the two-phase split, each
// manifest record (SPEC §8.6.11) carries the correct `pass` label: the
// transcription pass records carry "transcription" and the derivation pass records
// carry "derivation", with every asset appearing in both passes.
func TestTwoPhase_ManifestRecordsCarryPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h, _ := newTwoPhaseHarness(t, true)
	manifestPath := filepath.Join(h.stateDir, "run.jsonl")
	// Rebuild the service with the manifest enabled (newTwoPhaseHarness leaves it
	// off); reuse the same corpus/store so the run is otherwise identical.
	cfg := config.Config{
		RootDir:            h.root,
		StateDir:           h.stateDir,
		STTProvider:        "off",
		MediaBatchTwoPhase: true,
		MediaBatchManifest: manifestPath,
		MediaBatchProgress: true,
	}
	svc := mustNewIngestService(t, cfg, h.store)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro line\n[00:02] second line"})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "m", []string{"en"})

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("two-phase Run with manifest: %v", err)
	}

	recs := readManifest(t, manifestPath)
	// 4 assets x 2 passes = 8 records.
	if len(recs) != 8 {
		t.Fatalf("want 8 manifest records (4 assets x 2 passes), got %d: %+v", len(recs), recs)
	}
	passCounts := map[string]int{}
	for _, r := range recs {
		p, _ := r["pass"].(string)
		passCounts[p]++
	}
	if passCounts["transcription"] != 4 {
		t.Fatalf("want 4 transcription-pass records, got %d (%v)", passCounts["transcription"], passCounts)
	}
	if passCounts["derivation"] != 4 {
		t.Fatalf("want 4 derivation-pass records, got %d (%v)", passCounts["derivation"], passCounts)
	}
}

// TestTwoPhase_DisabledIsSinglePassManifest pins that with two-phase OFF (the
// default), manifest records carry NO pass label — byte-identical to the
// pre-split single-pass manifest.
func TestTwoPhase_DisabledIsSinglePassManifest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h, _ := newTwoPhaseHarness(t, false)
	manifestPath := filepath.Join(h.stateDir, "run.jsonl")
	cfg := config.Config{
		RootDir:            h.root,
		StateDir:           h.stateDir,
		STTProvider:        "off",
		MediaBatchManifest: manifestPath,
		MediaBatchProgress: true,
	}
	svc := mustNewIngestService(t, cfg, h.store)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] intro line"})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "m", []string{"en"})

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("single-pass Run with manifest: %v", err)
	}

	recs := readManifest(t, manifestPath)
	if len(recs) != 4 {
		t.Fatalf("want 4 single-pass manifest records (one per asset), got %d", len(recs))
	}
	for _, r := range recs {
		if p, ok := r["pass"]; ok && p != "" {
			t.Fatalf("single-pass manifest record must not carry a pass label, got %v", p)
		}
	}
}
