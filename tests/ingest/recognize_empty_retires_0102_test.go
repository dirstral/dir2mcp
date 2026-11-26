package tests

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Spec 0.102.0 (SPEC §8.9.4): a SUCCESSFUL recognition call that leaves no
// annotation retires the document's earlier recognition representation, so
// statements the backend no longer makes stop answering queries. A FAILED call
// keeps them (§8.9.6, pinned by recognition_failure_950_test.go).

type scriptedRecognizer struct {
	results []model.RecognizeResult
	calls   int
}

func (r *scriptedRecognizer) Recognize(_ context.Context, _ string) (model.RecognizeResult, error) {
	res := r.results[min(r.calls, len(r.results)-1)]
	r.calls++
	return res, nil
}

func TestRecognition0102_EmptySuccessfulRunRetiresTheEarlierStatements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	video := filepath.Join(root, "broadcast.mp4")
	writeFile(t, video, "fake-video-bytes-v1")
	writeFile(t, filepath.Join(root, "broadcast.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nFreeman flies out to centrefield\n")

	rec := &scriptedRecognizer{results: []model.RecognizeResult{
		{Name: "test", Version: "1", Annotations: []model.RecognizedAnnotation{
			{StartMS: 0, EndMS: 1000, Text: "Webb throws a curveball", Event: "pitch"},
		}},
		{Name: "test", Version: "1"},
	}}
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	cfg.RecognizeProvider = "serve"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(rec)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if hits, err := st.SearchBM25(ctx, "curveball", 5, ""); err != nil || len(hits) == 0 {
		t.Fatalf("run 1 must index the recognition statement: hits=%d err=%v", len(hits), err)
	}

	// The media changes, and the backend now answers with no annotation.
	writeFile(t, video, "fake-video-bytes-v2-changed")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(video, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if rec.calls != 2 {
		t.Fatalf("recognizer calls = %d, want 2: the changed media must be recognized again", rec.calls)
	}
	if hits, _ := st.SearchBM25(ctx, "curveball", 5, ""); len(hits) != 0 {
		t.Fatalf("a statement the backend withdrew still answers queries: %d hit(s)", len(hits))
	}
	reps, err := st.ActiveRepresentations(ctx, "broadcast.mp4")
	if err != nil {
		t.Fatalf("ActiveRepresentations: %v", err)
	}
	for _, rep := range reps {
		if rep.RepType == ingest.RepTypeRecognition {
			t.Fatalf("recognition representation %d is still active after an empty successful run", rep.RepID)
		}
	}
	// The retirement is scoped to recognition: the transcript stays.
	if hits, _ := st.SearchBM25(ctx, "Freeman", 5, ""); len(hits) == 0 {
		t.Fatal("the transcript was retired with the recognition representation")
	}
}

func TestRecognition0102_ServeClientDropsInvalidRawSeconds(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"recognizer": {"name": "test-backend", "version": "1"},
			"annotations": [
				{"start_s": 1.0004, "end_s": 1.0001, "text": "reversed, but equal after rounding"},
				{"start_s": -0.0001, "end_s": 0.5, "text": "negative, but zero after rounding"},
				{"start_s": 2.0, "end_s": 2.0, "text": "instantaneous and valid"}
			]
		}`))
	}))
	defer srv.Close()
	res, err := ingest.NewRecognizeServeClient(srv.URL).Recognize(context.Background(), "/abs/clip.mp4")
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if len(res.Annotations) != 1 || res.Annotations[0].Text != "instantaneous and valid" {
		t.Fatalf("want only the valid zero-length annotation, got %+v", res.Annotations)
	}
}

// TestRecognition0102_EmptyAnswerForUnchangedMediaKeepsTheStatements: the other
// half of §8.9.4. A forced reindex of the SAME media against a backend that now
// answers with nothing (a fault, most probably) must keep the earlier
// statements and log a warning, or one faulty backend could erase the
// annotations of a whole corpus.
func TestRecognition0102_EmptyAnswerForUnchangedMediaKeepsTheStatements(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "broadcast.mp4"), "fake-video-bytes-v1")

	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	logs := &syncBuffer{}
	rec := &scriptedRecognizer{results: []model.RecognizeResult{
		{Name: "test", Version: "1", Annotations: []model.RecognizedAnnotation{
			{StartMS: 0, EndMS: 1000, Text: "Webb throws a curveball", Event: "pitch"},
		}},
		{Name: "test", Version: "1"},
	}}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	cfg.RecognizeProvider = "serve"
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(rec)
	svc.SetLogger(log.New(logs, "", 0))
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	f := ingest.DiscoveredFile{RelPath: "broadcast.mp4", SizeBytes: 19, MTimeUnix: time.Now().Unix()}
	for pass := 1; pass <= 2; pass++ {
		if err := svc.ProcessDocument(ctx, f, nil, true); err != nil {
			t.Fatalf("ProcessDocument pass %d: %v", pass, err)
		}
	}
	if rec.calls != 2 {
		t.Fatalf("recognizer calls = %d, want 2 (the second pass is a forced reindex)", rec.calls)
	}
	if hits, _ := st.SearchBM25(ctx, "curveball", 5, ""); len(hits) == 0 {
		t.Fatal("an empty answer for unchanged media erased the earlier statements")
	}
	if !strings.Contains(logs.String(), "returned no annotation for unchanged media") {
		t.Fatalf("no warning names the empty answer; logs:\n%s", logs.String())
	}
}
