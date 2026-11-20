package tests

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestSecretPolicy_TwoPhaseRetiresSummaryOfUncheckedTranslation covers a
// summary made over a translated transcript (SPEC §7.2, §9.7). After a broader
// policy change, the transcription pass of the two-phase split retires the
// unchecked translation before the stamp clears the pending mark. A summary
// derived from that translation is unchecked too: it must be retired with it.
// Translation is off in the second run, so no later pass checks it again.
// Without the retirement, the summary that holds the codename stays live.
func TestSecretPolicy_TwoPhaseRetiresSummaryOfUncheckedTranslation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	mustWriteFile(t, filepath.Join(root, "media", "clip.mp3"), []byte("fake-audio"))
	stateDir := testutil.TempDir(t)
	dbPath := filepath.Join(stateDir, "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	run := func(patterns []string, twoPhase, translate bool) {
		t.Helper()
		cfg := config.Default()
		cfg.RootDir = root
		cfg.StateDir = stateDir
		cfg.STTProvider = "off"
		cfg.MediaBatchTwoPhase = twoPhase
		cfg.SecretPatterns = patterns
		cfg.RetrievalHierarchicalEnabled = true
		cfg.RetrievalHierarchicalLevels = []string{config.HierarchicalLevelDocument}
		cfg.RetrievalHierarchicalSourceReps = []string{"transcript-en"}
		cfg.RetrievalHierarchicalMaxTokens = config.DefaultHierarchicalMaxTokens
		cfg.RetrievalHierarchicalPromptVersion = config.DefaultHierarchicalPromptVersion
		svc := mustNewIngestService(t, cfg, st)
		svc.SetTranscriber(&fakeTranscriber{text: "[00:00] the report covers the regional budget and the new bridge"})
		svc.SetSTTIdentity("whisper", "large-v3")
		svc.SetTranscriptLanguage("de")
		if translate {
			svc.SetTranslator(&codenameTranslator{}, "mistral", "model-a", []string{"en"})
		}
		svc.SetSummarizer(&codenameSummarizer{}, "mistral", "mistral-small-latest")
		if err := svc.Run(ctx); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	defaults := config.Default().SecretPatterns
	// Single pass: the translation and its summary are made inline.
	run(defaults, false, true)
	if n := liveSummaries(t, dbPath); n == 0 {
		t.Fatal("the first run made no summary over the translation; the test proves nothing")
	}
	broader := append(append([]string(nil), defaults...), `BLUEHERON-[a-z]+-[0-9]{4}`)
	run(broader, true, false)
	if n := liveChunksWith(t, dbPath, "BLUEHERON"); n != 0 {
		t.Fatalf("%d live chunk(s) still hold text that the new policy matches", n)
	}
	if n := pendingMarks(t, dbPath); n != 0 {
		t.Fatalf("%d document(s) still wait for re-evaluation after the scan", n)
	}
}

func liveSummaries(t *testing.T, dbPath string) int {
	t.Helper()
	return countRows(t, dbPath, `SELECT COUNT(*) FROM representations WHERE deleted = 0 AND (rep_type = 'summary' OR rep_type LIKE 'summary-%')`)
}

func liveChunksWith(t *testing.T, dbPath, needle string) int {
	t.Helper()
	return countRows(t, dbPath, `SELECT COUNT(*) FROM chunks WHERE deleted = 0 AND instr(text, ?) > 0`, needle)
}

func countRows(t *testing.T, dbPath, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}
