package tests

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subexport"
	"github.com/dirstral/dir2mcp/internal/subtitle"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Subtitle write-back (SPEC §8.6.14).

const emitFakeTranscript = "[00:00] intro line\n[00:02] second line\n[00:05] third line"

// emitHarness is a corpus + store + service wired for write-back: STT off
// (fake transcriber), fake translator to "en", source language "de".
type emitHarness struct {
	root     string
	stateDir string
	cfg      config.Config
	store    *store.SQLiteStore
}

func newEmitHarness(t *testing.T, mutate func(cfg *config.Config)) *emitHarness {
	t.Helper()
	root := testutil.TempDir(t)
	stateDir := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	cfg.MediaSubtitlesEmitEnabled = true
	cfg.MediaSubtitlesEmitFormats = []string{"vtt", "ttml"}
	cfg.MediaSubtitlesTTMLEnabled = true
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}
	if mutate != nil {
		mutate(&cfg)
	}
	return &emitHarness{root: root, stateDir: stateDir, cfg: cfg, store: st}
}

// service builds a fresh service over the harness store, as a new process would,
// with the given fake transcript text and STT model identity.
func (h *emitHarness) service(t *testing.T, transcript, sttModel string) (*ingest.Service, *fakeTranscriber) {
	t.Helper()
	svc := mustNewIngestService(t, h.cfg, h.store)
	tr := &fakeTranscriber{text: transcript}
	svc.SetTranscriber(tr)
	svc.SetSTTIdentity("whisper", sttModel)
	svc.SetTranscriptLanguage("de")
	svc.SetTranslator(&fakeTranslator{}, "mistral", "mistral-small-2506", []string{"en"})
	return svc, tr
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func emitRows(t *testing.T, st *store.SQLiteStore) map[string]store.EmittedArtifact {
	t.Helper()
	rows, err := st.AllEmittedArtifacts(context.Background())
	if err != nil {
		t.Fatalf("AllEmittedArtifacts: %v", err)
	}
	out := map[string]store.EmittedArtifact{}
	for _, r := range rows {
		out[r.RelPath] = r
	}
	return out
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// TestSubtitleEmit_WritesBesideMediaByteIdenticalToExport pins the core
// contract: after ingest, one VTT per transcript language and one bilingual TTML
// sit beside the media, each byte-identical to what the export renderer
// produces, each recorded as owned; an authored sidecar that already binds is
// left untouched and its (language, format) is not written.
func TestSubtitleEmit_WritesBesideMediaByteIdenticalToExport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, nil)
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	mustWriteFile(t, filepath.Join(h.root, "audio", "two.mp3"), []byte("fake-audio-two"))
	authored := "WEBVTT\n\n1\n00:00:00.000 --> 00:00:02.000\nhuman cue\n"
	authoredPath := filepath.Join(h.root, "audio", "two.en.vtt")
	mustWriteFile(t, authoredPath, []byte(authored))
	authoredInfo, _ := os.Stat(authoredPath)

	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	renderer, err := subexport.NewRenderer(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertVTTMatchesExport(t, h, renderer, "audio/one.mp3", "de")
	assertVTTMatchesExport(t, h, renderer, "audio/one.mp3", "en")
	assertBilingualTTMLMatchesExport(t, h, renderer, "audio/one.mp3", "de", "en")

	// The authored sidecar is untouched and its (en, vtt) was not re-written;
	// two.mp3 took its transcript from the sidecar, so it has no "de" to write.
	if readFile(t, authoredPath) != authored {
		t.Fatalf("authored two.en.vtt was overwritten")
	}
	if info, _ := os.Stat(authoredPath); !info.ModTime().Equal(authoredInfo.ModTime()) {
		t.Fatalf("authored two.en.vtt mtime changed")
	}
	if fileExists(filepath.Join(h.root, "audio", "two.de.vtt")) {
		t.Fatalf("two.de.vtt must not exist: two.mp3 has no de transcript (sidecar took precedence)")
	}
	if !fileExists(filepath.Join(h.root, "audio", "two.ttml")) {
		t.Fatalf("two.ttml should be written from the authored en transcript (no TTML bound)")
	}

	rows := emitRows(t, h.store)
	assertOwnedRowsMatchDisk(t, h, rows, "audio/one.de.vtt", "audio/one.en.vtt", "audio/one.ttml", "audio/two.ttml")
	if _, ok := rows["audio/two.en.vtt"]; ok {
		t.Fatalf("the authored sidecar must never be recorded as owned")
	}
	if mode := fileMode(t, filepath.Join(h.root, "audio", "one.de.vtt")); mode&0o044 == 0 {
		t.Fatalf("written subtitle should be group/world readable (before umask), got %o", mode)
	}
}

// assertVTTMatchesExport checks the written `<stem>.<lang>.vtt` equals the export
// renderer's output for the same document and language.
func assertVTTMatchesExport(t *testing.T, h *emitHarness, r subexport.Renderer, relMedia, lang string) {
	t.Helper()
	stem := strings.TrimSuffix(relMedia, filepath.Ext(relMedia))
	got := readFile(t, filepath.Join(h.root, filepath.FromSlash(stem+"."+lang+".vtt")))
	want, err := r.RenderTimed(context.Background(), h.store, relMedia, lang, "vtt")
	if err != nil {
		t.Fatalf("RenderTimed %s: %v", lang, err)
	}
	// The written file is the export render plus its provenance marker (§8.6.14).
	want = subtitle.StampVTT(want)
	if got != want {
		t.Fatalf("%s.%s.vtt differs from export output:\n--- file ---\n%s\n--- export ---\n%s", stem, lang, got, want)
	}
}

// assertBilingualTTMLMatchesExport checks the written `<stem>.ttml` equals the
// bilingual export render and carries both language runs.
func assertBilingualTTMLMatchesExport(t *testing.T, h *emitHarness, r subexport.Renderer, relMedia, primary, secondary string) {
	t.Helper()
	stem := strings.TrimSuffix(relMedia, filepath.Ext(relMedia))
	got := readFile(t, filepath.Join(h.root, filepath.FromSlash(stem+".ttml")))
	want, _, err := r.RenderTTML(context.Background(), h.store, relMedia, primary, secondary)
	if err != nil {
		t.Fatalf("RenderTTML: %v", err)
	}
	want = subtitle.StampTTML(want)
	if got != want {
		t.Fatalf("%s.ttml differs from export output", stem)
	}
	if !strings.Contains(got, `xml:lang="`+primary+`"`) || !strings.Contains(got, `xml:lang="`+secondary+`"`) {
		t.Fatalf("%s.ttml must be bilingual (%s + %s runs), got:\n%s", stem, primary, secondary, got)
	}
}

// assertOwnedRowsMatchDisk checks each rel path has an ownership row whose size
// and mtime match the file and whose hash is a hex SHA-256.
func assertOwnedRowsMatchDisk(t *testing.T, h *emitHarness, rows map[string]store.EmittedArtifact, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		r, ok := rows[rel]
		if !ok {
			t.Fatalf("no ownership row for %s; rows=%v", rel, rows)
		}
		info, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if r.SizeBytes != info.Size() || r.MTimeUnix != info.ModTime().Unix() || len(r.ContentSHA256) != 64 {
			t.Fatalf("ownership row for %s does not match disk: %+v vs size=%d mtime=%d", rel, r, info.Size(), info.ModTime().Unix())
		}
	}
}

// TestSubtitleEmit_OwnedFilesDoNotChangeIdentityOrBecomeAuthored pins the
// ownership rule: a second scan (fresh process) over the written files sees the
// same document identity, re-transcribes nothing, keeps the STT-derived
// transcript (NOT a sidecar one), and leaves the files untouched.
func TestSubtitleEmit_OwnedFilesDoNotChangeIdentityOrBecomeAuthored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, nil)
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))

	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	doc1, err := h.store.GetDocumentByPath(ctx, "audio/one.mp3")
	if err != nil {
		t.Fatal(err)
	}
	snap1 := snapshotDoc(t, h.store, "audio/one.mp3")
	vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
	info1, _ := os.Stat(vttPath)

	svc2, tr2 := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	doc2, err := h.store.GetDocumentByPath(ctx, "audio/one.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if doc1.ContentHash == "" || doc1.ContentHash != doc2.ContentHash {
		t.Fatalf("content hash changed across the rescan: %q -> %q (owned sidecars leaked into the fingerprint)", doc1.ContentHash, doc2.ContentHash)
	}
	if doc2.SidecarFingerprint != "" {
		t.Fatalf("owned files must not appear in the sidecar fingerprint, got %q", doc2.SidecarFingerprint)
	}
	if tr2.calls != 0 {
		t.Fatalf("second scan must not re-transcribe an unchanged document, got %d calls", tr2.calls)
	}
	if snap2 := snapshotDoc(t, h.store, "audio/one.mp3"); !reflect.DeepEqual(snap1, snap2) {
		t.Fatalf("representations changed across the rescan:\n%+v\n%+v", snap1, snap2)
	}
	reps, err := h.store.TranscriptRepresentations(ctx, "audio/one.mp3")
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reps {
		if subexport.RepIsSidecar(rep.MetaJSON) {
			t.Fatalf("an owned VTT was re-ingested as an authored sidecar transcript: %s", rep.MetaJSON)
		}
	}
	if info2, _ := os.Stat(vttPath); !info2.ModTime().Equal(info1.ModTime()) || info2.Size() != info1.Size() {
		t.Fatalf("owned file was rewritten on a steady-state rescan under if_missing")
	}
}

// TestSubtitleEmit_EditedFileBecomesAuthored pins the other half of ownership:
// a written VTT that someone edits stops being owned, is never overwritten, and
// becomes the document's authored transcript on the next scan.
func TestSubtitleEmit_EditedFileBecomesAuthored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, nil)
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))

	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	doc1, _ := h.store.GetDocumentByPath(ctx, "audio/one.mp3")

	vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
	edited := readFile(t, vttPath) + "\n4\n00:00:08.000 --> 00:00:09.000\nedited by a human\n"
	if err := os.WriteFile(vttPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(30 * time.Second)
	if err := os.Chtimes(vttPath, later, later); err != nil {
		t.Fatal(err)
	}

	svc2, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if readFile(t, vttPath) != edited {
		t.Fatalf("edited file was overwritten")
	}
	if _, owned := emitRows(t, h.store)["audio/one.de.vtt"]; owned {
		t.Fatalf("edited file must no longer be recorded as owned")
	}
	doc2, _ := h.store.GetDocumentByPath(ctx, "audio/one.mp3")
	if doc2.ContentHash == doc1.ContentHash {
		t.Fatalf("an authored sidecar must change the document identity (content hash unchanged)")
	}
	reps, _ := h.store.TranscriptRepresentations(ctx, "audio/one.mp3")
	sawSidecarDE := false
	for _, rep := range reps {
		if subexport.RepIsSidecar(rep.MetaJSON) && strings.EqualFold(subexport.RepLanguage(rep.MetaJSON), "de") {
			sawSidecarDE = true
		}
	}
	if !sawSidecarDE {
		t.Fatalf("the edited VTT should now be the authored de transcript; reps=%+v", reps)
	}
}

// TestSubtitleEmit_RefreshRewritesOwnedOnRederivation pins the policies: under
// if_missing a re-derived transcript leaves the owned file as written; under
// refresh the owned file is rewritten to the new render. A forced reindex with a
// different fake transcript stands in for the STT identity change of §8.6.7.
func TestSubtitleEmit_RefreshRewritesOwnedOnRederivation(t *testing.T) {
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
				t.Fatalf("Run: %v", err)
			}
			vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
			before := readFile(t, vttPath)

			svc2, _ := h.service(t, "[00:00] a completely new transcript\n[00:03] from a better model", "whisper-large-v4")
			if err := svc2.Reindex(ctx); err != nil {
				t.Fatalf("Reindex: %v", err)
			}
			after := readFile(t, vttPath)
			renderer, _ := subexport.NewRenderer(h.cfg)
			want, err := renderer.RenderTimed(ctx, h.store, "audio/one.mp3", "de", "vtt")
			if err != nil {
				t.Fatal(err)
			}
			want = subtitle.StampVTT(want)
			switch policy {
			case "if_missing":
				if after != before {
					t.Fatalf("if_missing must leave the owned file as written")
				}
			case "refresh":
				if after == before {
					t.Fatalf("refresh must rewrite the owned file after re-derivation")
				}
				if after != want {
					t.Fatalf("refreshed file differs from export output")
				}
				if row := emitRows(t, h.store)["audio/one.de.vtt"]; row.ContentSHA256 == "" {
					t.Fatalf("refreshed file must stay owned")
				}
			}
		})
	}
}

// TestSubtitleEmit_TwoPhaseWritesSameFilesAndRecordsOutputs pins §8.6.11 ×
// §8.6.14: the two-phase split writes the same bytes single-pass writes, the
// transcription pass writes nothing, and the derivation-pass manifest records
// the artifacts under outputs.
func TestSubtitleEmit_TwoPhaseWritesSameFilesAndRecordsOutputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	single := newEmitHarness(t, nil)
	mustWriteFile(t, filepath.Join(single.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := single.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("single Run: %v", err)
	}

	manifestPath := filepath.Join(testutil.TempDir(t), "run.jsonl")
	two := newEmitHarness(t, func(cfg *config.Config) {
		cfg.MediaBatchTwoPhase = true
		cfg.MediaBatchManifest = manifestPath
	})
	mustWriteFile(t, filepath.Join(two.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc2, _ := two.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("two-phase Run: %v", err)
	}

	for _, name := range []string{"one.de.vtt", "one.en.vtt", "one.ttml"} {
		a := readFile(t, filepath.Join(single.root, "audio", name))
		b := readFile(t, filepath.Join(two.root, "audio", name))
		if a != b {
			t.Fatalf("%s differs between single-pass and two-phase", name)
		}
	}

	outputs := manifestOutputsByPass(t, manifestPath, "audio/one.mp3")
	for _, o := range outputs["transcription"] {
		if strings.HasPrefix(o, "vtt") || o == "ttml" {
			t.Fatalf("transcription pass must write no subtitle artifact, outputs=%v", outputs["transcription"])
		}
	}
	assertContainsAll(t, outputs["derivation"], "vtt:de", "vtt:en", "ttml")
}

// manifestOutputsByPass reads the run manifest and returns, for relPath, the
// `outputs` list of each pass keyed by pass name.
func manifestOutputsByPass(t *testing.T, manifestPath, relPath string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, rec := range readManifest(t, manifestPath) {
		if rec["rel_path"] != relPath {
			continue
		}
		var outs []string
		if raw, ok := rec["outputs"].([]any); ok {
			for _, o := range raw {
				outs = append(outs, o.(string))
			}
		}
		pass, _ := rec["pass"].(string)
		out[pass] = outs
	}
	return out
}

// assertContainsAll fails unless every want value is present in got.
func assertContainsAll(t *testing.T, got []string, want ...string) {
	t.Helper()
	have := map[string]struct{}{}
	for _, g := range got {
		have[g] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			t.Fatalf("missing %q in %v", w, got)
		}
	}
}

// TestSubtitleEmit_GroupedRenditionsWriteOneSetOnTheGroupStem pins the §8.6.5
// interplay: with renditions grouped, the files take the group stem (case
// preserved, markers stripped) so one set serves every rendition, and that set
// is owned and excluded from binding on the next scan.
func TestSubtitleEmit_GroupedRenditionsWriteOneSetOnTheGroupStem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, func(cfg *config.Config) {
		cfg.MediaVariantsGroup = true
		cfg.MediaVariantsSelect = "best"
		cfg.MediaSubtitlesEmitFormats = []string{"vtt"}
	})
	mustWriteFile(t, filepath.Join(h.root, "Show_128k.mp3"), []byte("fake-low"))
	mustWriteFile(t, filepath.Join(h.root, "Show_320k.mp3"), []byte("fake-high-bitrate"))

	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fileExists(filepath.Join(h.root, "Show.de.vtt")) || !fileExists(filepath.Join(h.root, "Show.en.vtt")) {
		entries, _ := os.ReadDir(h.root)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected Show.de.vtt and Show.en.vtt on the group stem, dir=%v", names)
	}
	// Compare the exact names in the listing, not a stat: on a case-insensitive
	// filesystem (macOS, Windows) a stat of show.de.vtt finds Show.de.vtt.
	entries, err := os.ReadDir(h.root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	listed := map[string]bool{}
	for _, e := range entries {
		listed[e.Name()] = true
	}
	for _, bad := range []string{"Show_320k.de.vtt", "Show_128k.de.vtt", "show.de.vtt"} {
		if listed[bad] {
			t.Fatalf("unexpected %s", bad)
		}
	}

	svc2, tr2 := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if tr2.calls != 0 {
		t.Fatalf("group-stem files must bind as owned (no re-transcription), got %d calls", tr2.calls)
	}
}

// TestSubtitleEmit_DisabledWritesNothing pins the default: with write-back off
// nothing is written and nothing is recorded, even with TTML enabled.
func TestSubtitleEmit_DisabledWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, func(cfg *config.Config) { cfg.MediaSubtitlesEmitEnabled = false })
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(h.root, "audio"))
	if len(entries) != 1 {
		t.Fatalf("write-back disabled must leave the directory as found, got %d entries", len(entries))
	}
	if rows := emitRows(t, h.store); len(rows) != 0 {
		t.Fatalf("no ownership rows expected, got %v", rows)
	}
}

// TestSubtitleEmit_OutputDirMirrorsCorpusTree pins media.subtitles.emit.dir: with
// an output root the corpus is not written to; the files land under the root at
// the corpus-relative path and are owned like any other.
func TestSubtitleEmit_OutputDirMirrorsCorpusTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	outRoot := testutil.TempDir(t)
	h := newEmitHarness(t, func(cfg *config.Config) {
		cfg.MediaSubtitlesEmitDir = outRoot
		cfg.MediaSubtitlesEmitFormats = []string{"srt"}
	})
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fileExists(filepath.Join(h.root, "audio", "one.de.srt")) {
		t.Fatalf("corpus must not be written to when emit.dir is set")
	}
	if !fileExists(filepath.Join(outRoot, "audio", "one.de.srt")) || !fileExists(filepath.Join(outRoot, "audio", "one.en.srt")) {
		t.Fatalf("expected srt files under the output root")
	}
	if _, ok := emitRows(t, h.store)["audio/one.de.srt"]; !ok {
		t.Fatalf("output-root artifacts must be recorded by corpus-relative path")
	}
}

// TestSubtitleEmit_OwnershipSurvivesDisablingWriteBack pins that ownership is a
// property of the record, not of the feature flag: an operator who enables
// write-back, scans, and then disables it must not find the files dir2mcp wrote
// re-ingested as authored transcripts on the next scan.
func TestSubtitleEmit_OwnershipSurvivesDisablingWriteBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, nil)
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	doc1, _ := h.store.GetDocumentByPath(ctx, "audio/one.mp3")

	h.cfg.MediaSubtitlesEmitEnabled = false
	svc2, tr2 := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	doc2, _ := h.store.GetDocumentByPath(ctx, "audio/one.mp3")
	if doc2.ContentHash != doc1.ContentHash || doc2.SidecarFingerprint != "" {
		t.Fatalf("disabling write-back changed the document identity: hash %q -> %q, fingerprint %q", doc1.ContentHash, doc2.ContentHash, doc2.SidecarFingerprint)
	}
	if tr2.calls != 0 {
		t.Fatalf("disabling write-back must not re-transcribe, got %d calls", tr2.calls)
	}
	reps, _ := h.store.TranscriptRepresentations(ctx, "audio/one.mp3")
	for _, rep := range reps {
		if subexport.RepIsSidecar(rep.MetaJSON) {
			t.Fatalf("owned file became an authored transcript after write-back was disabled: %s", rep.MetaJSON)
		}
	}
}

// TestSubtitleEmit_RefreshNeverOverwritesSameStatEdit pins the hash check that
// guards a rewrite: an edit that preserves the file's size AND mtime is invisible
// to discovery's stat test, so under `refresh` the rewrite path must verify the
// on-disk bytes against the recorded hash, treat the mismatch as an edit, drop
// the record and leave the file alone.
func TestSubtitleEmit_RefreshNeverOverwritesSameStatEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, func(cfg *config.Config) {
		cfg.MediaSubtitlesEmitPolicy = "refresh"
		cfg.MediaSubtitlesEmitFormats = []string{"vtt"}
	})
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
	original := readFile(t, vttPath)
	info, _ := os.Stat(vttPath)

	// Same length, different bytes, and the mtime restored: a stat cannot tell.
	edited := []byte(original)
	edited[len(edited)-2] = 'X'
	if err := os.WriteFile(vttPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(vttPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	svc2, _ := h.service(t, "[00:00] a completely new transcript\n[00:03] from a better model", "whisper-large-v4")
	if err := svc2.Reindex(ctx); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if readFile(t, vttPath) != string(edited) {
		t.Fatalf("refresh overwrote an edited file whose size and mtime matched the record")
	}
	if _, owned := emitRows(t, h.store)["audio/one.de.vtt"]; owned {
		t.Fatalf("the edited file must no longer be recorded as owned")
	}
}

// TestSubtitleEmit_RefreshKeepsTheOldFileWhenTheRecordFails pins the rollback of
// a refresh rewrite: when the new file cannot be recorded, the old owned file is
// put back, unchanged, so its row still describes the disk and no backup is
// left behind.
func TestSubtitleEmit_RefreshKeepsTheOldFileWhenTheRecordFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, func(cfg *config.Config) {
		cfg.MediaSubtitlesEmitPolicy = "refresh"
		cfg.MediaSubtitlesEmitFormats = []string{"vtt"}
	})
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
	original := readFile(t, vttPath)
	rowBefore, ok := emitRows(t, h.store)["audio/one.de.vtt"]
	if !ok {
		t.Fatalf("expected an ownership row after the first run")
	}

	blockEmittedArtifactWrites(t, filepath.Join(h.stateDir, "meta.sqlite"))

	svc2, _ := h.service(t, "[00:00] a completely new transcript\n[00:03] from a better model", "whisper-large-v4")
	if err := svc2.Reindex(ctx); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if got := readFile(t, vttPath); got != original {
		t.Fatalf("a failed record must put the old owned file back, got:\n%s", got)
	}
	rowAfter, ok := emitRows(t, h.store)["audio/one.de.vtt"]
	if !ok || rowAfter.ContentSHA256 != rowBefore.ContentSHA256 {
		t.Fatalf("the old row must stay and describe the restored file: before %+v, after %+v (ok=%v)", rowBefore, rowAfter, ok)
	}
	info, err := os.Stat(vttPath)
	if err != nil || info.Size() != rowAfter.SizeBytes || info.ModTime().Unix() != rowAfter.MTimeUnix {
		t.Fatalf("the restored file must match its row by size and mtime: %v", err)
	}
	assertNoSubtitleScratchFiles(t, filepath.Join(h.root, "audio"))
}

// blockEmittedArtifactWrites makes every later write of an ownership row fail,
// with a sqlite trigger on the state database at dbPath.
func blockEmittedArtifactWrites(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TRIGGER block_emit_insert BEFORE INSERT ON emitted_artifacts BEGIN SELECT RAISE(ABORT, 'blocked by test'); END`,
		`CREATE TRIGGER block_emit_update BEFORE UPDATE ON emitted_artifacts BEGIN SELECT RAISE(ABORT, 'blocked by test'); END`,
	} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
	}
}

// assertNoSubtitleScratchFiles fails when dir holds a write-back backup or
// temporary file.
func assertNoSubtitleScratchFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dir2mcp-subtitle-") {
			t.Fatalf("a backup or temp file was left behind: %s", e.Name())
		}
	}
}

// TestSubtitleEmit_DeletedOwnedFileIsRecreatedAndRowRefreshed pins that an owned
// file removed from disk is written again on the next scan (under if_missing
// too: it is missing), and that its stale ownership row does not outlive the
// deletion — the recreated file gets a fresh row matching the new stat.
func TestSubtitleEmit_DeletedOwnedFileIsRecreatedAndRowRefreshed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, func(cfg *config.Config) { cfg.MediaSubtitlesEmitFormats = []string{"vtt"} })
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
	before := emitRows(t, h.store)["audio/one.de.vtt"]
	if err := os.Remove(vttPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // a new mtime, at second resolution

	svc2, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if !fileExists(vttPath) {
		t.Fatalf("deleted owned file must be written again")
	}
	after, ok := emitRows(t, h.store)["audio/one.de.vtt"]
	if !ok {
		t.Fatalf("recreated file must be recorded as owned")
	}
	info, _ := os.Stat(vttPath)
	if after.MTimeUnix != info.ModTime().Unix() || after.MTimeUnix == before.MTimeUnix {
		t.Fatalf("ownership row must describe the recreated file: before=%d after=%d disk=%d", before.MTimeUnix, after.MTimeUnix, info.ModTime().Unix())
	}
}

// TestSubtitleEmit_RowsAreScopedToTheirOutputRoot pins SPEC §8.6.14: an
// ownership row applies only under the output root it was written under. After
// a run with `dir` set, switching to beside-the-media must not treat an
// in-corpus file that shares the rel_path as owned — here an authored one.
func TestSubtitleEmit_RowsAreScopedToTheirOutputRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	outRoot := testutil.TempDir(t)
	h := newEmitHarness(t, func(cfg *config.Config) {
		cfg.MediaSubtitlesEmitDir = outRoot
		cfg.MediaSubtitlesEmitFormats = []string{"vtt"}
	})
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	row := emitRows(t, h.store)["audio/one.de.vtt"]
	if row.OutputRoot != filepath.Clean(outRoot) {
		t.Fatalf("row must record its output root, got %q", row.OutputRoot)
	}

	// An authored file lands in the corpus at the same rel_path, then dir is
	// switched off. The old row must not claim it.
	authored := "WEBVTT\n\n1\n00:00:00.000 --> 00:00:01.000\nhuman\n"
	inCorpus := filepath.Join(h.root, "audio", "one.de.vtt")
	mustWriteFile(t, inCorpus, []byte(authored))
	h.cfg.MediaSubtitlesEmitDir = ""
	svc2, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if readFile(t, inCorpus) != authored {
		t.Fatalf("authored in-corpus file was overwritten")
	}
	reps, _ := h.store.TranscriptRepresentations(ctx, "audio/one.mp3")
	sawSidecar := false
	for _, rep := range reps {
		if subexport.RepIsSidecar(rep.MetaJSON) {
			sawSidecar = true
		}
	}
	if !sawSidecar {
		t.Fatalf("the authored file must bind as a sidecar; a stale row from another root claimed it")
	}
}

// TestSubtitleEmit_InCorpusSidecarDoesNotTouchOutputRootRow pins the other
// direction of the root scope: while `dir` is set, sidecar discovery runs over
// the corpus and must not read the rows of the output root. An authored file
// in the corpus at the same rel_path as an output-root row must neither be
// claimed as owned nor drop that row.
func TestSubtitleEmit_InCorpusSidecarDoesNotTouchOutputRootRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	outRoot := testutil.TempDir(t)
	h := newEmitHarness(t, func(cfg *config.Config) {
		cfg.MediaSubtitlesEmitDir = outRoot
		cfg.MediaSubtitlesEmitFormats = []string{"vtt"}
	})
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	before, ok := emitRows(t, h.store)["audio/one.de.vtt"]
	if !ok || before.OutputRoot != filepath.Clean(outRoot) {
		t.Fatalf("expected an output-root row for audio/one.de.vtt, got %+v (ok=%v)", before, ok)
	}

	authored := "WEBVTT\n\n1\n00:00:00.000 --> 00:00:01.000\nhuman\n"
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.de.vtt"), []byte(authored))
	svc2, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	after, ok := emitRows(t, h.store)["audio/one.de.vtt"]
	if !ok {
		t.Fatalf("an in-corpus file dropped the output-root row that shares its rel_path")
	}
	if after.OutputRoot != before.OutputRoot || after.ContentSHA256 != before.ContentSHA256 {
		t.Fatalf("output-root row changed: before %+v, after %+v", before, after)
	}
	reps, _ := h.store.TranscriptRepresentations(ctx, "audio/one.mp3")
	sawSidecar := false
	for _, rep := range reps {
		if subexport.RepIsSidecar(rep.MetaJSON) {
			sawSidecar = true
		}
	}
	if !sawSidecar {
		t.Fatalf("the authored in-corpus file must bind as a sidecar; an output-root row claimed it")
	}
}

// TestSubtitleEmit_LostStateDatabaseDoesNotFreezeTheArchive pins the recovery
// half of SPEC §8.6.14: with the state database gone (a fresh store over the
// same corpus), the VTT/TTML files dir2mcp wrote prove themselves by their
// provenance marker, so they are adopted as owned output rather than ingested
// as authored transcripts. STT runs again, the transcript stays STT-sourced, the
// files are left as they are, and the adoption is persisted as a row.
func TestSubtitleEmit_LostStateDatabaseDoesNotFreezeTheArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, nil)
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
	written := readFile(t, vttPath)
	if !subtitle.ReadProvenance([]byte(written)).Intact {
		t.Fatalf("written VTT must carry an intact provenance marker:\n%s", written)
	}

	// The state database is lost: a fresh store over the same corpus.
	fresh := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	if err := fresh.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	h.store = fresh
	// Only the database is replaced; the transcript cache in the same state folder
	// may serve the STT result, so what matters is where the transcript comes from.
	svc2, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	reps, _ := fresh.TranscriptRepresentations(ctx, "audio/one.mp3")
	if len(reps) == 0 {
		t.Fatalf("the document must have its STT transcript after a state loss")
	}
	for _, rep := range reps {
		if subexport.RepIsSidecar(rep.MetaJSON) {
			t.Fatalf("a marked file was ingested as an authored transcript: %s", rep.MetaJSON)
		}
	}
	doc, _ := fresh.GetDocumentByPath(ctx, "audio/one.mp3")
	if doc.SidecarFingerprint != "" {
		t.Fatalf("adopted files must not enter the sidecar fingerprint, got %q", doc.SidecarFingerprint)
	}
	if readFile(t, vttPath) != written {
		t.Fatalf("an adopted file must be left as it is under if_missing")
	}
	if _, ok := emitRows(t, fresh)["audio/one.de.vtt"]; !ok {
		t.Fatalf("the adoption must be persisted as an ownership row")
	}
}

// TestSubtitleEmit_EditedMarkedFileIsAuthoredAfterStateLoss pins that the
// marker cannot launder an edit: a marked file whose body was changed fails the
// hash and binds as an authored sidecar, exactly as without a marker.
func TestSubtitleEmit_EditedMarkedFileIsAuthoredAfterStateLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newEmitHarness(t, func(cfg *config.Config) { cfg.MediaSubtitlesEmitFormats = []string{"vtt"} })
	mustWriteFile(t, filepath.Join(h.root, "audio", "one.mp3"), []byte("fake-audio-one"))
	svc, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	vttPath := filepath.Join(h.root, "audio", "one.de.vtt")
	edited := readFile(t, vttPath) + "\n00:00:08.000 --> 00:00:09.000\nfixed by an editor\n"
	if err := os.WriteFile(vttPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	if err := fresh.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	h.store = fresh
	svc2, _ := h.service(t, emitFakeTranscript, "whisper-large-v3")
	if err := svc2.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	reps, _ := fresh.TranscriptRepresentations(ctx, "audio/one.mp3")
	saw := false
	for _, rep := range reps {
		if subexport.RepIsSidecar(rep.MetaJSON) && strings.EqualFold(subexport.RepLanguage(rep.MetaJSON), "de") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("an edited marked file must bind as the authored de transcript")
	}
	if readFile(t, vttPath) != edited {
		t.Fatalf("an edited file must never be overwritten")
	}
}
