package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dir2mcp/internal/cli"
	"dir2mcp/internal/config"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/model"
	"dir2mcp/internal/store"
)

type commandTestNoopStore struct{}

func (s *commandTestNoopStore) Init(context.Context) error { return nil }
func (s *commandTestNoopStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (s *commandTestNoopStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}
func (s *commandTestNoopStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return []model.Document{}, 0, nil
}
func (s *commandTestNoopStore) Close() error { return nil }

type commandTestRetrieverStub struct {
	askResult model.AskResult
	askErr    error

	searchHits []model.SearchHit
	searchErr  error

	askCalled    bool
	searchCalled bool

	lastAskQuestion string
	lastAskQuery    model.SearchQuery
	lastSearchQuery model.SearchQuery

	// used by the new IndexingComplete accessor
	indexingComplete bool
}

func (s *commandTestRetrieverStub) Search(_ context.Context, q model.SearchQuery) ([]model.SearchHit, error) {
	s.searchCalled = true
	s.lastSearchQuery = q
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return append([]model.SearchHit(nil), s.searchHits...), nil
}

func (s *commandTestRetrieverStub) Ask(_ context.Context, question string, q model.SearchQuery) (model.AskResult, error) {
	s.askCalled = true
	s.lastAskQuestion = question
	s.lastAskQuery = q
	if s.askErr != nil {
		return model.AskResult{}, s.askErr
	}
	return s.askResult, nil
}

func (s *commandTestRetrieverStub) OpenFile(_ context.Context, _ string, _ model.Span, _ int) (string, error) {
	return "", model.ErrNotImplemented
}

func (s *commandTestRetrieverStub) Stats(_ context.Context) (model.Stats, error) {
	return model.Stats{}, model.ErrNotImplemented
}

func (s *commandTestRetrieverStub) IndexingComplete(_ context.Context) (bool, error) {
	return s.indexingComplete, nil
}

// compile-time assertion ensuring our stub satisfies the Retriever interface
var _ model.Retriever = (*commandTestRetrieverStub)(nil)

func TestStatusReadsCorpusSnapshotHuman(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	raw := `{
  "ts": "2026-03-01T00:00:00Z",
  "indexing": {
    "mode": "incremental",
    "running": false,
    "scanned": 12,
    "indexed": 8,
    "skipped": 3,
    "deleted": 1,
    "representations": 11,
    "chunks_total": 77,
    "embedded_ok": 74,
    "errors": 0
  },
  "doc_counts": {"code": 5, "md": 3},
  "total_docs": 8,
  "code_ratio": 0.625
}`
	if err := os.WriteFile(filepath.Join(stateDir, "corpus.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("write corpus.json: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"status"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	out := stdout.String()
	if !strings.Contains(out, "corpus_json") {
		t.Fatalf("expected source in stdout, got: %s", out)
	}
	if !strings.Contains(out, "scanned=12") || !strings.Contains(out, "indexed=8") {
		t.Fatalf("expected indexing stats in stdout, got: %s", out)
	}
	if !strings.Contains(out, "code=5") || !strings.Contains(out, "md=3") {
		t.Fatalf("expected doc counts in stdout, got: %s", out)
	}
}

func TestStatusJSONMode(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	raw := `{
  "ts": "2026-03-01T00:00:00Z",
  "indexing": {"mode":"incremental","running":false,"scanned":1,"indexed":1,"skipped":0,"deleted":0,"representations":1,"chunks_total":1,"embedded_ok":1,"errors":0},
  "doc_counts": {"code": 1},
  "total_docs": 1,
  "code_ratio": 1.0
}`
	if err := os.WriteFile(filepath.Join(stateDir, "corpus.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("write corpus.json: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "status"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	var payload struct {
		Source   string `json:"source"`
		StateDir string `json:"state_dir"`
		Snapshot struct {
			TotalDocs int64 `json:"total_docs"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal status payload: %v\nraw=%s", err, stdout.String())
	}
	if payload.Source != "corpus_json" {
		t.Fatalf("Source=%q want=%q", payload.Source, "corpus_json")
	}
	if payload.Snapshot.TotalDocs != 1 {
		t.Fatalf("TotalDocs=%d want=1", payload.Snapshot.TotalDocs)
	}
	if payload.StateDir == "" {
		t.Fatal("expected state_dir to be present")
	}
}

func TestStatusFallsBackToComputedSnapshot(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath:     "docs/a.md",
		DocType:     "md",
		SourceType:  "file",
		SizeBytes:   10,
		MTimeUnix:   1,
		ContentHash: "h1",
		Status:      "ok",
	}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(context.Background(), "docs/a.md")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	repID, err := st.UpsertRepresentation(context.Background(), model.Representation{
		DocID:   doc.DocID,
		RepType: "raw_text",
		RepHash: "rep-hash",
	})
	if err != nil {
		t.Fatalf("upsert representation: %v", err)
	}
	if _, err := st.InsertChunkWithSpans(context.Background(), model.Chunk{
		RepID:           repID,
		Ordinal:         0,
		Text:            "hello world",
		TextHash:        "chunk-hash",
		IndexKind:       "text",
		EmbeddingStatus: "ok",
	}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close sqlite store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "status"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	var payload struct {
		Source   string `json:"source"`
		Snapshot struct {
			TotalDocs int64            `json:"total_docs"`
			DocCounts map[string]int64 `json:"doc_counts"`
			Indexing  struct {
				Scanned         int64 `json:"scanned"`
				Indexed         int64 `json:"indexed"`
				Representations int64 `json:"representations"`
				ChunksTotal     int64 `json:"chunks_total"`
				EmbeddedOK      int64 `json:"embedded_ok"`
			} `json:"indexing"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal status payload: %v\nraw=%s", err, stdout.String())
	}
	if payload.Source != "computed" {
		t.Fatalf("Source=%q want=%q", payload.Source, "computed")
	}
	if payload.Snapshot.TotalDocs != 1 || payload.Snapshot.DocCounts["md"] != 1 {
		t.Fatalf("unexpected computed snapshot: %+v", payload.Snapshot)
	}
	if payload.Snapshot.Indexing.Scanned != 1 || payload.Snapshot.Indexing.Indexed != 1 {
		t.Fatalf("unexpected lifecycle counters: %+v", payload.Snapshot.Indexing)
	}
	if payload.Snapshot.Indexing.Representations != 1 || payload.Snapshot.Indexing.ChunksTotal != 1 || payload.Snapshot.Indexing.EmbeddedOK != 1 {
		t.Fatalf("unexpected representation/chunk counters: %+v", payload.Snapshot.Indexing)
	}
}

func TestStatusNoStateReturnsExitCode1(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"status"})
		if code != 1 {
			t.Fatalf("unexpected exit code: got=%d want=1 stderr=%s", code, stderr.String())
		}
	})

	if !strings.Contains(stderr.String(), "no state found") {
		t.Fatalf("expected no-state message, got: %s", stderr.String())
	}
}

func TestConfigInitCreatesConfigFile(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"config", "init"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	raw, err := os.ReadFile(filepath.Join(tmp, ".dir2mcp.yaml"))
	if err != nil {
		t.Fatalf("read .dir2mcp.yaml: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "root_dir:") || !strings.Contains(text, "state_dir:") {
		t.Fatalf("expected baseline config keys, got:\n%s", text)
	}
	if strings.Contains(strings.ToLower(text), "mistral_api_key") {
		t.Fatalf("config file must not persist MISTRAL_API_KEY, got:\n%s", text)
	}
}

func TestConfigInitPatchesMissingKeysPreservesExistingValues(t *testing.T) {
	tmp := t.TempDir()
	initial := "root_dir: /custom/root\n"
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte(initial), 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"config", "init"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	cfg, err := config.LoadFile(filepath.Join(tmp, ".dir2mcp.yaml"))
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if cfg.RootDir != "/custom/root" {
		t.Fatalf("RootDir=%q want=%q", cfg.RootDir, "/custom/root")
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		t.Fatal("expected StateDir to be populated after patching missing keys")
	}
}

func TestConfigInitJSONOutput(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "config", "init"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	var payload struct {
		Path      string   `json:"path"`
		Created   bool     `json:"created"`
		Updated   bool     `json:"updated"`
		NextSteps []string `json:"next_steps"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal config init payload: %v\nraw=%s", err, stdout.String())
	}
	if payload.Path != ".dir2mcp.yaml" {
		t.Fatalf("Path=%q want=%q", payload.Path, ".dir2mcp.yaml")
	}
	if !payload.Created || payload.Updated {
		t.Fatalf("unexpected create/update flags: created=%t updated=%t", payload.Created, payload.Updated)
	}
	if len(payload.NextSteps) == 0 {
		t.Fatal("expected non-empty next_steps")
	}
}

func TestAskAnswerModeWithFlagsAndCitations(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")

	stub := &commandTestRetrieverStub{
		askResult: model.AskResult{
			Question: "what is alpha?",
			Answer:   "alpha is documented in [docs/a.md]",
			Citations: []model.Citation{
				{ChunkID: 11, RelPath: "docs/a.md", Span: model.Span{Kind: "lines", StartLine: 3, EndLine: 6}},
			},
			Hits: []model.SearchHit{
				{ChunkID: 11, RelPath: "docs/a.md", DocType: "md", RepType: "raw_text", Score: 0.98, Snippet: "alpha"},
			},
			IndexingComplete: true,
		},
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever {
			return stub
		},
	})

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{
			"ask",
			"--k", "3",
			"--index", "both",
			"--path-prefix", "docs/",
			"--file-glob", "*.md",
			"--doc-types", "md,code",
			"what is alpha?",
		})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	if !stub.askCalled {
		t.Fatal("expected Ask to be called")
	}
	if stub.lastAskQuestion != "what is alpha?" {
		t.Fatalf("lastAskQuestion=%q want=%q", stub.lastAskQuestion, "what is alpha?")
	}
	if stub.lastAskQuery.K != 3 || stub.lastAskQuery.Index != "both" {
		t.Fatalf("unexpected query forwarding: %+v", stub.lastAskQuery)
	}
	if stub.lastAskQuery.PathPrefix != "docs/" || stub.lastAskQuery.FileGlob != "*.md" {
		t.Fatalf("unexpected filter forwarding: %+v", stub.lastAskQuery)
	}
	if len(stub.lastAskQuery.DocTypes) != 2 || stub.lastAskQuery.DocTypes[0] != "md" || stub.lastAskQuery.DocTypes[1] != "code" {
		t.Fatalf("unexpected doc_types forwarding: %+v", stub.lastAskQuery.DocTypes)
	}

	out := stdout.String()
	if !strings.Contains(out, "alpha is documented") {
		t.Fatalf("expected answer in stdout, got: %s", out)
	}
	if !strings.Contains(out, "Citations") {
		t.Fatalf("expected citations section in stdout, got: %s", out)
	}
}

func TestAskSearchOnlyCallsSearch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")

	stub := &commandTestRetrieverStub{
		searchHits: []model.SearchHit{
			{ChunkID: 10, RelPath: "docs/a.md", DocType: "md", RepType: "raw_text", Score: 0.9, Snippet: "alpha"},
		},
	}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever {
			return stub
		},
	})

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"ask", "--mode", "search_only", "alpha"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	if !stub.searchCalled {
		t.Fatal("expected Search to be called")
	}
	if stub.askCalled {
		t.Fatal("did not expect Ask to be called in search_only mode")
	}
	if !strings.Contains(stdout.String(), "Search results") || !strings.Contains(stdout.String(), "docs/a.md") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}
}

func TestAskJSONOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")

	stub := &commandTestRetrieverStub{
		askResult: model.AskResult{
			Question:         "q",
			Answer:           "a",
			Citations:        []model.Citation{{ChunkID: 1, RelPath: "docs/a.md", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2}}},
			Hits:             []model.SearchHit{{ChunkID: 1, RelPath: "docs/a.md", Score: 0.8, Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2}}},
			IndexingComplete: true,
		},
	}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever {
			return stub
		},
	})

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "ask", "q"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	var payload struct {
		Question         string                   `json:"question"`
		Answer           string                   `json:"answer"`
		Citations        []map[string]interface{} `json:"citations"`
		Hits             []map[string]interface{} `json:"hits"`
		IndexingComplete bool                     `json:"indexing_complete"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal ask payload: %v\nraw=%s", err, stdout.String())
	}
	if payload.Question != "q" || payload.Answer != "a" {
		t.Fatalf("unexpected question/answer payload: %+v", payload)
	}
	if len(payload.Citations) != 1 || len(payload.Hits) != 1 {
		t.Fatalf("unexpected citations/hits payload: %+v", payload)
	}
	if !payload.IndexingComplete {
		t.Fatalf("expected indexing_complete=true, got payload=%+v", payload)
	}
}

// Search-only mode with JSON output should still report the current indexing
// state. The implementation obtains the boolean via the dedicated
// IndexingComplete accessor, so only Search (not Ask) is invoked.
func TestAskSearchOnlyJSONIncludesIndexingComplete(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")

	stub := &commandTestRetrieverStub{
		searchHits: []model.SearchHit{
			{ChunkID: 10, RelPath: "docs/a.md", DocType: "md", RepType: "raw_text", Score: 0.9, Snippet: "alpha"},
		},
		indexingComplete: true,
	}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever {
			return stub
		},
	})

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "ask", "--mode", "search_only", "alpha"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})

	if !stub.searchCalled {
		t.Fatal("expected Search to be called")
	}
	if stub.askCalled {
		t.Fatal("did not expect Ask to be called now that a dedicated accessor exists")
	}

	var payload struct {
		Question         string                   `json:"question"`
		Answer           string                   `json:"answer"`
		Citations        []map[string]interface{} `json:"citations"`
		Hits             []map[string]interface{} `json:"hits"`
		IndexingComplete bool                     `json:"indexing_complete"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal ask payload: %v\nraw=%s", err, stdout.String())
	}
	if !payload.IndexingComplete {
		t.Fatalf("expected indexing_complete=true got %+v", payload)
	}
}

func TestAskNoContextResponse(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")

	stub := &commandTestRetrieverStub{
		askResult: model.AskResult{
			Question: "q",
			Answer:   "No relevant context found in the indexed corpus.",
		},
	}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return &commandTestNoopStore{} },
		NewRetriever: func(config.Config, model.Store) model.Retriever {
			return stub
		},
	})

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"ask", "q"})
		if code != 0 {
			t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
		}
	})
	if !strings.Contains(stdout.String(), "No relevant context found in the indexed corpus.") {
		t.Fatalf("expected no-context response in stdout, got: %s", stdout.String())
	}
}

func TestAskNonPositiveKDefaultsToSearchK(t *testing.T) {
	t.Setenv("MISTRAL_API_KEY", "test-key")

	for _, rawK := range []string{"0", "-1"} {
		t.Run("k="+rawK, func(t *testing.T) {
			tmp := t.TempDir()

			stub := &commandTestRetrieverStub{
				askResult: model.AskResult{
					Question: "q",
					Answer:   "a",
				},
			}

			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
				NewStore: func(config.Config) model.Store { return &commandTestNoopStore{} },
				NewRetriever: func(config.Config, model.Store) model.Retriever {
					return stub
				},
			})

			withWorkingDir(t, tmp, func() {
				code := app.RunWithContext(context.Background(), []string{"ask", "--k", rawK, "q"})
				if code != 0 {
					t.Fatalf("unexpected exit code: %d stderr=%s", code, stderr.String())
				}
			})

			if !stub.askCalled {
				t.Fatal("expected Ask to be called")
			}
			if stub.lastAskQuery.K != mcp.DefaultSearchK {
				t.Fatalf("expected default k=%d, got=%d", mcp.DefaultSearchK, stub.lastAskQuery.K)
			}
		})
	}
}

func TestAskKAboveMaxFails(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"ask", "--k", "51", "q"})
		if code != 1 {
			t.Fatalf("unexpected exit code: got=%d want=1 stderr=%s", code, stderr.String())
		}
	})

	errOut := stderr.String()
	if !strings.Contains(errOut, "invalid ask flags") || !strings.Contains(errOut, fmt.Sprintf("k must be <= %d", mcp.MaxSearchK)) {
		t.Fatalf("expected k upper-bound error, got: %s", errOut)
	}
}

func TestAskMissingQuestionFails(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)

	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"ask", "--k", "2"})
		if code != 1 {
			t.Fatalf("unexpected exit code: got=%d want=1 stderr=%s", code, stderr.String())
		}
	})
	if !strings.Contains(stderr.String(), "requires a question argument") {
		t.Fatalf("expected missing-question message, got: %s", stderr.String())
	}
}
