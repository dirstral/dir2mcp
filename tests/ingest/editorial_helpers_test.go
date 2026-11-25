package tests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/editorial"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// editorialEnv is a corpus with a real SQLite store and an ingest service, set
// up so that the editorial apply step can run without a scan: documents are
// seeded directly, and the media duration probe is a table.
type editorialEnv struct {
	t        *testing.T
	ctx      context.Context
	root     string
	stateDir string
	st       *store.SQLiteStore
	svc      *ingest.Service
	mu       sync.Mutex
	duration map[string]time.Duration
	reports  []editorial.Report
}

func newEditorialEnv(t *testing.T, folds []string) *editorialEnv {
	t.Helper()
	root := t.TempDir()
	e := &editorialEnv{t: t, ctx: context.Background(), root: root, stateDir: filepath.Join(root, ".dir2mcp"),
		duration: map[string]time.Duration{}}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.openStore()
	e.newService(folds, nil)
	return e
}

func (e *editorialEnv) openStore() {
	e.st = store.NewSQLiteStore(filepath.Join(e.stateDir, "meta.sqlite"))
	if err := e.st.Init(e.ctx); err != nil {
		e.t.Fatalf("store init: %v", err)
	}
	st := e.st
	e.t.Cleanup(func() { _ = st.Close() })
}

// newService builds a fresh ingest service over the same corpus, as a restart
// with a changed config would.
func (e *editorialEnv) newService(folds, secretPatterns []string) {
	cfg := config.Default()
	cfg.RootDir = e.root
	cfg.StateDir = e.stateDir
	cfg.AnnotationsNameFolds = folds
	if secretPatterns != nil {
		cfg.SecretPatterns = secretPatterns
	}
	e.svc = mustNewIngestService(e.t, cfg, e.st)
	e.svc.ProbeDurationFunc = func(_ context.Context, path string) (time.Duration, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.duration[filepath.Base(path)], nil
	}
	e.svc.SetOnEditorialImport(func(_ string, r editorial.Report) {
		e.mu.Lock()
		e.reports = append(e.reports, r)
		e.mu.Unlock()
	})
}

// addDoc writes the file and its document row.
func (e *editorialEnv) addDoc(relPath, docType, status string, duration time.Duration) {
	e.t.Helper()
	abs := filepath.Join(e.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("media bytes of "+relPath), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.setDuration(relPath, duration)
	if err := e.st.UpsertDocument(e.ctx, model.Document{
		RelPath: relPath, DocType: docType, SourceType: "filesystem", Status: status,
		ContentHash: "hash-" + relPath,
	}); err != nil {
		e.t.Fatalf("UpsertDocument(%s): %v", relPath, err)
	}
}

func (e *editorialEnv) setDuration(relPath string, d time.Duration) {
	e.mu.Lock()
	e.duration[filepath.Base(relPath)] = d
	e.mu.Unlock()
}

// stage validates and stages an import file the way `annotate import` does.
func (e *editorialEnv) stage(source, keyKind string, lines ...string) {
	e.t.Helper()
	batch, problems, err := editorial.ParseFile(strings.NewReader(strings.Join(lines, "\n")), keyKind, source)
	if err != nil || len(problems) > 0 {
		e.t.Fatalf("ParseFile: err=%v problems=%+v", err, problems)
	}
	now := time.Now()
	if _, err := editorial.Stage(e.stateDir, editorial.NewStaged(batch, source, keyKind, now), now); err != nil {
		e.t.Fatalf("Stage: %v", err)
	}
}

func (e *editorialEnv) apply() {
	e.t.Helper()
	if err := e.svc.ApplyEditorialImports(e.ctx); err != nil {
		e.t.Fatalf("ApplyEditorialImports: %v", err)
	}
}

func (e *editorialEnv) lastReport() editorial.Report {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.reports) == 0 {
		e.t.Fatal("no import report was emitted")
	}
	return e.reports[len(e.reports)-1]
}

// edAnnotation is one stored editorial chunk with its span.
type edAnnotation struct {
	RepType    string
	Text       string
	SpanKind   string
	Start, End int
	Entities   []string          `json:"entities"`
	Event      string            `json:"event"`
	Sources    []string          `json:"sources"`
	Attributes map[string]string `json:"attributes"`
	Derivation string            `json:"derivation"`
}

// annotations reads the live editorial chunks of a document, per source rep in
// rep_type order and then in chunk order.
func (e *editorialEnv) annotations(relPath string) []edAnnotation {
	e.t.Helper()
	return e.chunks(relPath, "r.rep_type LIKE 'editorial@%'")
}

// transcriptChunks reads the live transcript chunks of a document.
func (e *editorialEnv) transcriptChunks(relPath string) []edAnnotation {
	e.t.Helper()
	return e.chunks(relPath, "r.rep_type = 'transcript'")
}

func (e *editorialEnv) chunks(relPath, repPredicate string) []edAnnotation {
	e.t.Helper()
	db, err := e.st.EnsureDB(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer e.st.ReleaseDB()
	rows, err := db.QueryContext(e.ctx, `
		SELECT c.rep_type, c.text, COALESCE(s.span_kind, ''), COALESCE(s.start, 0), COALESCE(s.end, 0), COALESCE(s.extra_json, '')
		  FROM chunks c
		  JOIN representations r ON r.rep_id = c.rep_id
		  JOIN documents d ON d.doc_id = r.doc_id
		  LEFT JOIN spans s ON s.chunk_id = c.chunk_id
		 WHERE d.rel_path = ? AND `+repPredicate+` AND c.deleted = 0 AND r.deleted = 0
		 ORDER BY r.rep_type, c.ordinal`, relPath)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []edAnnotation
	for rows.Next() {
		var a edAnnotation
		var extra string
		if err := rows.Scan(&a.RepType, &a.Text, &a.SpanKind, &a.Start, &a.End, &extra); err != nil {
			e.t.Fatal(err)
		}
		if extra != "" {
			if err := json.Unmarshal([]byte(extra), &a); err != nil {
				e.t.Fatalf("decode extra_json %s: %v", extra, err)
			}
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// markAllEmbedded marks every live chunk embedded, as the embed worker would.
func (e *editorialEnv) markAllEmbedded() {
	e.t.Helper()
	db, err := e.st.EnsureDB(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	rows, err := db.QueryContext(e.ctx, `SELECT chunk_id FROM chunks WHERE deleted = 0`)
	if err != nil {
		e.st.ReleaseDB()
		e.t.Fatal(err)
	}
	var labels []uint64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			e.t.Fatal(err)
		}
		labels = append(labels, uint64(id))
	}
	_ = rows.Close()
	e.st.ReleaseDB()
	if err := e.st.MarkEmbedded(e.ctx, labels); err != nil {
		e.t.Fatal(err)
	}
}

// pendingChunks counts the live chunks that wait for an embedding.
func (e *editorialEnv) pendingChunks() int {
	e.t.Helper()
	db, err := e.st.EnsureDB(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer e.st.ReleaseDB()
	var n int
	if err := db.QueryRowContext(e.ctx, `SELECT COUNT(*) FROM chunks WHERE deleted = 0 AND embedding_status = 'pending'`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// editorialMeta reads the meta_json and rep_hash of the source's editorial
// representation on a document.
func (e *editorialEnv) editorialMeta(relPath, source string) (map[string]any, string) {
	e.t.Helper()
	db, err := e.st.EnsureDB(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer e.st.ReleaseDB()
	var meta, hash string
	err = db.QueryRowContext(e.ctx, `
		SELECT r.meta_json, r.rep_hash FROM representations r JOIN documents d ON d.doc_id = r.doc_id
		 WHERE d.rel_path = ? AND r.rep_type = ? AND r.deleted = 0`, relPath, editorial.RepTypeForSource(source)).Scan(&meta, &hash)
	if err != nil {
		e.t.Fatalf("read editorial meta of %s: %v", relPath, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(meta), &out); err != nil {
		e.t.Fatal(err)
	}
	return out, hash
}

// seedTranscript writes a transcript representation with two time chunks.
func (e *editorialEnv) seedTranscript(relPath string) {
	e.t.Helper()
	doc, err := e.st.GetDocumentByPath(e.ctx, relPath)
	if err != nil {
		e.t.Fatal(err)
	}
	repID, err := e.st.UpsertRepresentation(e.ctx, model.Representation{DocID: doc.DocID, RepType: "transcript", RepHash: "t1", CreatedUnix: 1})
	if err != nil {
		e.t.Fatal(err)
	}
	for i, text := range []string{"spoken words at the start", "more spoken words"} {
		if _, err := e.st.InsertChunkWithSpans(e.ctx, model.Chunk{RepID: repID, Ordinal: i, Text: text, IndexKind: "text"},
			[]model.Span{{Kind: "time", StartMS: i * 60000, EndMS: (i + 1) * 60000}}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// docWithHash is the document row of a video with the given content hash.
func docWithHash(relPath, hash string) model.Document {
	return model.Document{RelPath: relPath, DocType: "video", SourceType: "filesystem", Status: "ok", ContentHash: hash}
}
