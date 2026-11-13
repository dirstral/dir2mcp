package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subexport"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestSecretPolicy_TwoPhaseRechecksExistingTranslations pins SPEC §7.2 (spec
// 0.93.0) under the two-phase split (§8.6.11). After a broader policy change,
// the transcription pass re-reads a media document and stamps its content hash,
// which clears the pending mark. The derivation pass keeps a translation that
// already exists under the current derivation identity, so without a fix an
// old translation that the new pattern matches stays searchable. The new
// pattern here matches only the translation text (codenameTranslator writes a
// codename that the source transcript does not hold), so only a check of the
// translation can withhold the document.
func TestSecretPolicy_TwoPhaseRechecksExistingTranslations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "media", "clip.mp3"), []byte("fake-audio"))
	stateDir := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const rel = "media/clip.mp3"

	run := func(patterns []string) {
		t.Helper()
		cfg := config.Default()
		cfg.RootDir = root
		cfg.StateDir = stateDir
		cfg.STTProvider = "off"
		cfg.MediaBatchTwoPhase = true
		cfg.SecretPatterns = patterns
		svc := mustNewIngestService(t, cfg, st)
		svc.SetTranscriber(&fakeTranscriber{text: "[00:00] the report covers the regional budget and the new bridge"})
		svc.SetSTTIdentity("whisper", "large-v3")
		svc.SetTranscriptLanguage("de")
		svc.SetTranslator(&codenameTranslator{}, "mistral", "model-a", []string{"en"})
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	liveTranslation := func() bool {
		t.Helper()
		reps, err := st.TranscriptRepresentations(ctx, rel)
		if err != nil {
			return false
		}
		for _, rep := range reps {
			if subexport.RepIsTranslation(rep.MetaJSON) {
				return true
			}
		}
		return false
	}

	defaults := config.Default().SecretPatterns
	run(defaults)
	if d, err := st.GetDocumentByPath(ctx, rel); err != nil || d.Status != string(model.DocumentStatusOK) {
		t.Fatalf("after the first run: status=%q err_msg=%q err=%v", d.Status, d.ErrorMessage, err)
	}
	if !liveTranslation() {
		t.Fatal("the first run produced no translation; the test proves nothing")
	}

	broader := append(append([]string(nil), defaults...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	run(broader)
	doc := requireStatus(t, st, rel, model.DocumentStatusSecretExcluded)
	if doc.SkipReason != model.SkipReasonSecretExcluded {
		t.Errorf("skip_reason = %q, want %q", doc.SkipReason, model.SkipReasonSecretExcluded)
	}
	if liveTranslation() {
		t.Error("a translation that the new pattern matches is still live")
	}
	reps, err := st.ActiveRepresentations(ctx, rel)
	if err == nil {
		for _, rep := range reps {
			if strings.Contains(rep.MetaJSON, "translat") {
				t.Errorf("live representation %d still carries translation metadata", rep.RepID)
			}
		}
	}
}

// codenameTranslator is fakeTranslator with one word of its output replaced by
// a codename, so the translation holds text that the source does not.
type codenameTranslator struct {
	fakeTranslator
}

func (c *codenameTranslator) Generate(ctx context.Context, prompt string) (string, error) {
	out, err := c.fakeTranslator.Generate(ctx, prompt)
	return strings.ReplaceAll(out, "bridge", "BLUEHERON-alpha-2026"), err
}
