package tests

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Ingest secret-scanning coverage (dir2mcp #681).
//
// The ingest exclusion policy had two holes, and both of them ended with a
// credential in the index:
//
//  1. it scanned only the first 64 KiB of a source file, so a credential further
//     in was chunked, embedded, and returned by `search` and `ask`;
//  2. it scanned the SOURCE BYTES only, so a credential that becomes text through
//     OCR, transcription, extraction, or translation was never tested against the
//     patterns at all.
//
// Every test here withholds nothing but the document: the assertions look at the
// persisted status, the skip reason, and the live representations, which is what
// retrieval reads.

// secret681 is a synthetic AWS access key id. It matches the FIRST default
// pattern (`AKIA[0-9A-Z]{16}`), which is the shape SPEC §7.2 lists first, and it
// is not a real credential.
const secret681 = "AKIA2ZZZZZZZZZZZZZZQ"

// secretScanConfig returns a config with the shipped default secret patterns, so
// the tests exercise the policy an operator gets out of the box rather than one
// invented for the test.
func secretScanConfig(root, stateDir string) config.Config {
	return config.Config{
		RootDir:        root,
		StateDir:       stateDir,
		STTProvider:    "off",
		SecretPatterns: config.Default().SecretPatterns,
	}
}

// filler681 returns n bytes of ordinary prose that matches no secret pattern.
func filler681(n int) string {
	line := "the quarterly report covers delivery, revenue and customer risk\n"
	var b strings.Builder
	b.Grow(n + len(line))
	for b.Len() < n {
		b.WriteString(line)
	}
	return b.String()[:n]
}

// docFor681 reads back the persisted document row.
func docFor681(t *testing.T, st *store.SQLiteStore, relPath string) model.Document {
	t.Helper()
	doc, err := st.GetDocumentByPath(context.Background(), relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	return doc
}

// assertWithheld681 fails unless the document is recorded as withheld under the
// §15.2 `secret_excluded` reason AND has nothing left that retrieval can return.
func assertWithheld681(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	doc := docFor681(t, st, relPath)
	if doc.Status != "secret_excluded" {
		t.Errorf("%s status = %q, want secret_excluded", relPath, doc.Status)
	}
	if doc.SkipReason != model.SkipReasonSecretExcluded {
		t.Errorf("%s skip_reason = %q, want %q", relPath, doc.SkipReason, model.SkipReasonSecretExcluded)
	}
	reps, err := st.ActiveRepresentations(context.Background(), relPath)
	if err != nil {
		t.Fatalf("ActiveRepresentations(%s): %v", relPath, err)
	}
	if len(reps) != 0 {
		t.Errorf("%s still has %d live representation(s) %+v; its text is searchable", relPath, len(reps), reps)
	}
}

// TestSecretScan_PastTheHeadSample_IsWithheld is the first half of #681. The
// credential sits well past the 64 KiB head sample that ingest used to scan, so
// on `main` the document is indexed and `search` can return the tail that holds
// it.
func TestSecretScan_PastTheHeadSample_IsWithheld(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	// 200 KiB of prose, then the key: more than three times the old sample.
	writeFile(t, filepath.Join(root, "runbook.md"), filler681(200*1024)+"\naws_key = "+secret681+"\n")
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertWithheld681(t, st, "runbook.md")
}

// TestSecretScan_WithinTheHeadSample_StaysWithheld pins the behaviour that must
// NOT change: a credential in the first 64 KiB was already excluded and still is.
func TestSecretScan_WithinTheHeadSample_StaysWithheld(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "creds.txt"), "aws_key = "+secret681+"\n"+filler681(4*1024))
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertWithheld681(t, st, "creds.txt")
}

// TestSecretScan_CleanLargeFile_StaysIndexed is the false-positive guard for the
// wider scan: reading every byte of a large document must not start excluding
// ordinary text.
func TestSecretScan_CleanLargeFile_StaysIndexed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "handbook.md"), filler681(300*1024))
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	doc := docFor681(t, st, "handbook.md")
	if doc.Status != "ok" {
		t.Fatalf("handbook.md status = %q, want ok; the wider scan produced a false positive", doc.Status)
	}
	if types := activeRepTypes(t, st, "handbook.md"); !types[ingest.RepTypeRawText] {
		t.Errorf("handbook.md has no raw_text representation (have %v)", types)
	}
}

// TestSecretScan_BinaryPayloadInATextDocType_IsNotIndexed pins the one case that
// sits between the two scan targets. A binary payload can classify into a
// text-oriented doc_type (a .parquet reaches "data"), and #398 already refuses to
// put such content on the raw-text path, so it is never chunked and never
// indexed. It therefore keeps the cheap head sample instead of paying for a full
// scan that could not protect anything. What matters for #681 is the outcome: no
// searchable text either way.
func TestSecretScan_BinaryPayloadInATextDocType_IsNotIndexed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	// A NUL byte in the head is the #398 binary signal; the credential sits far
	// past the head sample.
	writeFile(t, filepath.Join(root, "export.parquet"),
		"PAR1\x00binary column data\n"+filler681(100*1024)+"\naws_key = "+secret681+"\n")
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reps, err := st.ActiveRepresentations(context.Background(), "export.parquet")
	if err != nil {
		t.Fatalf("ActiveRepresentations: %v", err)
	}
	if len(reps) != 0 {
		t.Errorf("export.parquet has %d live representation(s) %+v; binary content must never be indexed as text",
			len(reps), reps)
	}
}

// TestSecretScan_OnlyInExtractedText_WithholdsTheDocument is the second half of
// #681, in its most ordinary form: a scanned contract or a screenshot. The source
// bytes hold no readable credential; only the extractor's output does, so on
// `main` nothing is scanned and the extracted text is indexed.
func TestSecretScan_OnlyInExtractedText_WithholdsTheDocument(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "contract.pdf"), "%PDF-1.7 opaque page image bytes")
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	svc.SetOCR(&fakeOCR{text: "# Deployment\n\nUse aws_key = " + secret681 + " for the staging account.\n"})
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertWithheld681(t, st, "contract.pdf")
}

// TestSecretScan_OnlyInTranscript_WithholdsTheDocument is the spoken form: the
// credential is audio in the source and text only after transcription.
func TestSecretScan_OnlyInTranscript_WithholdsTheDocument(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "standup.mp3"), "fake-audio")
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] the key is\n[00:02] aws_key = " + secret681})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertWithheld681(t, st, "standup.mp3")
}

// TestSecretScan_OnlyInTranslation_RetiresTheCleanTranscript covers the ordering
// case: a clean transcript is persisted first, and the credential appears only in
// a derived translation. The verdict is document-wide, so the transcript that was
// already written must be retired too. Otherwise the surrounding context of the
// credential stays searchable and the document reports as healthy.
func TestSecretScan_OnlyInTranslation_RetiresTheCleanTranscript(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "briefing.mp3"), "fake-audio")
	st := newRealStore(t)

	cfg := secretScanConfig(root, stateDir)
	cfg.MediaTranslateEnabled = true
	cfg.MediaTranslateTargetLangs = []string{"en"}

	svc := mustNewIngestService(t, cfg, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] guten morgen\n[00:02] zweiter satz"})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetTranscriptLanguage("de")
	// The translator emits the credential the source transcript never held.
	svc.SetTranslator(&secretTranslator681{}, "mistral", "mistral-small-2506", cfg.MediaTranslateTargetLangs)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertWithheld681(t, st, "briefing.mp3")
}

// secretTranslator681 is a model.Generator that puts a credential in its
// translated output while the source transcript stays clean. It answers the
// windowed prompt in the numbered 1:1 shape the parser expects, so translation
// takes its normal path rather than the malformed-response fallback.
type secretTranslator681 struct{}

func (secretTranslator681) Generate(_ context.Context, prompt string) (string, error) {
	var out strings.Builder
	for i := 1; i <= countNumberedSourceLines681(prompt); i++ {
		fmt.Fprintf(&out, "%d: the key is aws_key = %s\n", i, secret681)
	}
	if out.Len() > 0 {
		return out.String(), nil
	}
	// Per-line prompt shape: one line in, one line out.
	return "the key is aws_key = " + secret681, nil
}

// numberedSourceLine681 matches the "N: text" lines the windowed translate
// prompt uses to enumerate the cues it wants translated.
var numberedSourceLine681 = regexp.MustCompile(`(?m)^\s*\d+:\s`)

func countNumberedSourceLines681(prompt string) int {
	return len(numberedSourceLine681.FindAllString(prompt, -1))
}

// TestSecretScan_OnlyInSubtitleSidecar_WithholdsTheMedia covers the authored
// derived text: a .vtt sidecar carries no credential of its own into the index
// (the sidecar file is classified as a never-indexed skip), but its cues become
// the MEDIA document's searchable transcript. So the media document is the one
// that has to be withheld.
func TestSecretScan_OnlyInSubtitleSidecar_WithholdsTheMedia(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	writeFile(t, filepath.Join(root, "talk.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nthe key is aws_key = "+secret681+"\n")
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertWithheld681(t, st, "talk.mp3")
	if doc := docFor681(t, st, "talk.vtt"); doc.Status == "ok" {
		t.Errorf("talk.vtt status = %q; the sidecar file must never be indexed on its own", doc.Status)
	}
}

// TestSecretScan_AnnotationIsRefusedWithoutTouchingTheDocument covers the one
// derived-text path that must NOT withhold its document. `dir2mcp_annotate`
// generates text ABOUT an already-scanned document, so a match says the model
// wrote a credential shape, not that the corpus file holds one. The annotation is
// refused with the sentinel the MCP layer maps to FORBIDDEN, and the healthy
// document keeps its representations.
func TestSecretScan_AnnotationIsRefusedWithoutTouchingTheDocument(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.md"), "a clean note about the deployment\n")
	st := newRealStore(t)
	ctx := context.Background()

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	doc := docFor681(t, st, "notes.md")

	annotation := map[string]interface{}{"summary": "aws_key = " + secret681}
	preview, err := svc.StoreAnnotationRepresentations(ctx, doc, annotation, true)
	if !errors.Is(err, ingest.ErrSecretExcluded) {
		t.Fatalf("StoreAnnotationRepresentations error = %v, want ErrSecretExcluded", err)
	}
	if preview != "" {
		t.Errorf("preview = %q, want empty; a refused annotation must not be echoed back", preview)
	}
	if strings.Contains(err.Error(), secret681) {
		t.Errorf("the refusal message quotes the matched payload: %q", err.Error())
	}

	if got := docFor681(t, st, "notes.md"); got.Status != "ok" {
		t.Errorf("notes.md status = %q after a refused annotation, want ok", got.Status)
	}
	if types := activeRepTypes(t, st, "notes.md"); !types[ingest.RepTypeRawText] {
		t.Errorf("a refused annotation retired the document's own representations (have %v)", types)
	}
}

// TestSecretScan_WithheldVideoIsNotAlsoBrandedAnError guards the interaction with
// the #398/#495 unsearchable-video verdict. A video whose only text source is its
// transcript produces nothing once that transcript is withheld, which looks
// exactly like "no representation produced". It must not be stamped
// status="error" on top: that would overwrite the secret_excluded row and count
// one document as both a skip and an error. A withheld document is unsearchable
// by design, not by failure.
func TestSecretScan_WithheldVideoIsNotAlsoBrandedAnError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "briefing.mp4"), "fake-video")
	// The sidecar is the video's only text source, and it carries the credential.
	// It also keeps the fixture off ffmpeg, which a synthetic video cannot satisfy.
	writeFile(t, filepath.Join(root, "briefing.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nthe key is aws_key = "+secret681+"\n")
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	indexState := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(indexState)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	assertWithheld681(t, st, "briefing.mp4")
	if doc := docFor681(t, st, "briefing.mp4"); doc.ErrorMessage != "" {
		t.Errorf("briefing.mp4 error_message = %q, want empty on a withheld document", doc.ErrorMessage)
	}
	// The corpus holds two files: the withheld video and its sidecar, which is
	// itself never indexed. So both count as skips, neither as an error, and the
	// #426 identity still balances.
	snap := indexState.Snapshot()
	if snap.Errors != 0 || snap.Indexed != 0 {
		t.Fatalf("counters = indexed:%d skipped:%d errors:%d, want indexed 0 and errors 0",
			snap.Indexed, snap.Skipped, snap.Errors)
	}
	if snap.Indexed+snap.Skipped+snap.Errors != snap.Scanned {
		t.Fatalf("indexed:%d + skipped:%d + errors:%d != scanned:%d (#426)",
			snap.Indexed, snap.Skipped, snap.Errors, snap.Scanned)
	}
}

// TestSecretScan_DerivedVerdictSurvivesTheNextScan guards the incremental gate. A
// derived verdict cannot be recomputed from the source bytes, so it has to be
// carried on the row. Without the carry the second scan rebuilds the document as
// "ok", writes that over the withheld row, and then early-returns on the
// unchanged-content gate: the corpus reports an indexed document whose
// representations are gone.
func TestSecretScan_DerivedVerdictSurvivesTheNextScan(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "standup.mp3"), "fake-audio")
	st := newRealStore(t)

	transcript := "[00:00] the key is\n[00:02] aws_key = " + secret681
	for _, pass := range []string{"first", "second"} {
		svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
		svc.SetTranscriber(&fakeTranscriber{text: transcript})
		svc.SetSTTIdentity("whisper", "whisper-large-v3")
		if err := svc.Run(context.Background()); err != nil {
			t.Fatalf("%s scan: %v", pass, err)
		}
		assertWithheld681(t, st, "standup.mp3")
	}
}

// TestSecretScan_RemovingTheSecretLetsTheDocumentBackIn is the other side of the
// carry: it is conditional on the content being unchanged, so an edited file is
// re-derived and decided again. A withheld document must not be withheld forever.
func TestSecretScan_RemovingTheSecretLetsTheDocumentBackIn(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	path := filepath.Join(root, "runbook.md")
	writeFile(t, path, filler681(100*1024)+"\naws_key = "+secret681+"\n")
	st := newRealStore(t)

	first := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := first.Run(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	assertWithheld681(t, st, "runbook.md")

	// The operator removes the credential.
	writeFile(t, path, filler681(100*1024)+"\nthe key now lives in the secret manager\n")
	second := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	if err := second.Run(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	doc := docFor681(t, st, "runbook.md")
	if doc.Status != "ok" {
		t.Fatalf("runbook.md status = %q after the credential was removed, want ok", doc.Status)
	}
	if types := activeRepTypes(t, st, "runbook.md"); !types[ingest.RepTypeRawText] {
		t.Errorf("runbook.md was not re-indexed after the credential was removed (have %v)", types)
	}
}

// TestSecretScan_DerivedExclusionCountsAsSkipNotIndexed keeps the #426 accounting
// invariant: a withheld document counts as exactly one skip and never also as
// indexed, so scanned = indexed + skipped + errors still holds.
func TestSecretScan_DerivedExclusionCountsAsSkipNotIndexed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "standup.mp3"), "fake-audio")
	st := newRealStore(t)

	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] aws_key = " + secret681})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	indexState := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(indexState)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snap := indexState.Snapshot()
	if snap.Scanned != 1 || snap.Skipped != 1 || snap.Indexed != 0 || snap.Errors != 0 {
		t.Fatalf("counters = scanned:%d indexed:%d skipped:%d errors:%d, want 1/0/1/0",
			snap.Scanned, snap.Indexed, snap.Skipped, snap.Errors)
	}
}

// TestSecretScan_DerivedExclusionEmitsOneFileSkip pins the SPEC §3.2 invariant
// that a terminal skip raises exactly one file_skip event, carrying the §15.2
// reason so the honest-coverage aggregate names the right cause.
func TestSecretScan_DerivedExclusionEmitsOneFileSkip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := t.TempDir()
	writeFile(t, filepath.Join(root, "standup.mp3"), "fake-audio")
	st := newRealStore(t)

	var reasons []string
	svc := mustNewIngestService(t, secretScanConfig(root, stateDir), st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] aws_key = " + secret681})
	svc.SetSTTIdentity("whisper", "whisper-large-v3")
	svc.SetOnDocumentSkip(func(relPath, _, reason string) {
		if relPath == "standup.mp3" {
			reasons = append(reasons, reason)
		}
	})
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(reasons) != 1 || reasons[0] != model.SkipReasonSecretExcluded {
		t.Fatalf("file_skip reasons = %v, want exactly one %q", reasons, model.SkipReasonSecretExcluded)
	}
}
