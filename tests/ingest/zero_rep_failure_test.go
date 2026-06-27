package tests

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestProcessDocument_TranscriptProviderFailure_PersistsErrorStatus is the
// regression guard for #413: an audio document whose transcription fails with a
// genuine provider failure produces zero representations, yet the previous code
// left it status="ok" — invisible to CorpusStats.Errors / RecentFailures and
// reporting errors=0 after a restart (the audio was silently unsearchable). The
// failure must now be persisted as status="error" (with an error_message) and
// surface in RecentFailures / CorpusStats.Errors, while the ingest run itself
// still does NOT hard-fail (the batch continues).
func TestProcessDocument_TranscriptProviderFailure_PersistsErrorStatus(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "talk.mp3"), "fake-audio")
	st := newRealStore(t)

	svc := sttService(t, root, t.TempDir(), st, "whisper", "whisper-large-v3", "en",
		&fakeTranscriber{err: errors.New("provider down")})

	f := ingest.DiscoveredFile{RelPath: "talk.mp3", SizeBytes: 10, MTimeUnix: time.Now().Unix()}

	// A per-document transcript provider failure must not hard-fail the run.
	if err := svc.ProcessDocument(context.Background(), f, nil, false); err != nil {
		t.Fatalf("ProcessDocument hard-failed on a per-document transcript provider failure: %v", err)
	}

	// The document is persisted as status="error" with a descriptive message
	// rather than being silently left status="ok".
	doc, err := st.GetDocumentByPath(context.Background(), "talk.mp3")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if doc.Status != "error" {
		t.Fatalf("doc status = %q, want \"error\" (a zero-representation provider failure must not stay ok)", doc.Status)
	}
	if doc.ErrorMessage == "" {
		t.Fatal("doc error_message is empty; the failure carries no descriptive message")
	}

	// It surfaces in RecentFailures (newest-first, status='error').
	failures, err := st.RecentFailures(context.Background(), 20)
	if err != nil {
		t.Fatalf("RecentFailures: %v", err)
	}
	if !recentFailuresContain(failures, "talk.mp3") {
		t.Fatalf("talk.mp3 missing from RecentFailures: %+v", failures)
	}

	// And CorpusStats.Errors — the persisted count that survives a restart —
	// includes it (the bug reported errors=0 here).
	stats, err := st.CorpusStats(context.Background())
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.Errors < 1 {
		t.Fatalf("CorpusStats.Errors = %d, want >= 1", stats.Errors)
	}
}

func recentFailuresContain(docs []model.Document, relPath string) bool {
	for _, d := range docs {
		if d.RelPath == relPath {
			return true
		}
	}
	return false
}
