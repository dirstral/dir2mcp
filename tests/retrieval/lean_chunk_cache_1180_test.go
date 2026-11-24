package tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Issue #1180: the retrieval chunk-metadata cache (one SearchHit per embedded
// chunk, warm-loaded at `up` and kept for the daemon's lifetime) used to hold
// every "time" span's per-word timings (Span.Words) and merged-segment
// boundaries (Span.Cues). Nothing that reads the cache uses them, and on a
// transcript corpus they were most of its size. These tests pin that the cache
// no longer holds them AND that every field a served hit needs survives.

const (
	leanWindowMS     = 30000 // one 30 s transcript chunk window
	leanWordsPerSpan = 75    // ~2.5 words a second
	leanCuesPerSpan  = 6     // six 5 s transcript segments merged into the window
)

// leanTranscriptSpan is a fully populated "time" span: word timings, cues and
// every annotation field the served span (mcp buildOpenFileSpan) emits.
func leanTranscriptSpan(startMS int) model.Span {
	words := make([]model.WordSpan, leanWordsPerSpan)
	step := leanWindowMS / leanWordsPerSpan
	for i := range words {
		words[i] = model.WordSpan{T: startMS + i*step, D: step - 20, W: fmt.Sprintf("word%02d", i)}
	}
	cues := make([]model.CueSpan, leanCuesPerSpan)
	cueMS := leanWindowMS / leanCuesPerSpan
	for i := range cues {
		cues[i] = model.CueSpan{T: startMS + i*cueMS, D: cueMS, N: 60}
	}
	return model.Span{
		Kind: "time", StartMS: startMS, EndMS: startMS + leanWindowMS,
		Words: words, Cues: cues,
		Speaker: "S1", SpeakerLabel: "Anchor",
		Entities:   []string{"team:otters"},
		Event:      "interview",
		Sources:    []string{"playbyplay"},
		Attributes: map[string]string{"half": "bottom"},
	}
}

// seedLeanTranscriptCorpus writes n embedded "text" chunks, each carrying the
// full transcript span above, straight into the schema the store created (the
// same shortcut as the #732 walk harness: a large corpus in seconds). It
// returns the extra_json every span was stored with.
func seedLeanTranscriptCorpus(t *testing.T, dbPath string, n int) string {
	t.Helper()
	kind, start, end, extra, err := store.SpanToRow(leanTranscriptSpan(0))
	if err != nil || kind != "time" || start != 0 || end != leanWindowMS {
		t.Fatalf("SpanToRow = (%q, %d, %d, err=%v), want a time span 0..%d", kind, start, end, err, leanWindowMS)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatalf("open seed handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	mustPrep := func(q string) *sql.Stmt {
		st, perr := tx.Prepare(q)
		if perr != nil {
			t.Fatalf("prepare %q: %v", q, perr)
		}
		return st
	}
	docStmt := mustPrep(`INSERT INTO documents (rel_path, doc_type, source_type, title, mtime_unix, status, deleted)
	                     VALUES (?, 'audio', 'local', ?, 1700000000, 'ok', 0)`)
	repStmt := mustPrep(`INSERT INTO representations (rep_id, doc_id, rep_type, rep_hash, created_unix, deleted)
	                     VALUES (?, ?, 'transcript', 'h', 1700000000, 0)`)
	chunkStmt := mustPrep(`INSERT INTO chunks (chunk_id, rep_id, ordinal, rel_path, doc_type, rep_type, text, index_kind, modality, language, embedding_status, deleted)
	                       VALUES (?, ?, ?, ?, 'audio', 'transcript', ?, 'text', 'text', 'en', 'ok', 0)`)
	spanStmt := mustPrep(`INSERT INTO spans (chunk_id, span_kind, start, end, extra_json) VALUES (?, 'time', ?, ?, ?)`)

	const chunksPerDoc = 100
	body := strings.Repeat("the quick brown fox jumps over the lazy dog ", 9)
	docID := 0
	for i := 1; i <= n; i++ {
		if (i-1)%chunksPerDoc == 0 {
			docID++
			if _, err := docStmt.Exec(leanRelPath(docID), "Talk "+strconv.Itoa(docID)); err != nil {
				t.Fatalf("insert document %d: %v", docID, err)
			}
			if _, err := repStmt.Exec(docID, docID); err != nil {
				t.Fatalf("insert representation %d: %v", docID, err)
			}
		}
		if _, err := chunkStmt.Exec(i, docID, (i-1)%chunksPerDoc, leanRelPath(docID), body); err != nil {
			t.Fatalf("insert chunk %d: %v", i, err)
		}
		if _, err := spanStmt.Exec(i, start, end, extra); err != nil {
			t.Fatalf("insert span %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	return extra
}

func leanRelPath(docID int) string { return fmt.Sprintf("media/talk-%05d.mp3", docID) }

func openLeanStore(t *testing.T, n int) (*store.SQLiteStore, string) {
	t.Helper()
	dbPath := filepath.Join(testutil.TempDir(t), "lean.sqlite")
	st := store.NewSQLiteStore(dbPath)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return st, seedLeanTranscriptCorpus(t, dbPath, n)
}

// TestWarmLoadedTimeHitKeepsServedFields_1180 warm-loads a transcript corpus the
// way `up` does and searches it. The hit must carry every field the MCP tools
// serve, and its served span must be exactly the one the full stored span
// renders to; only the word timings and cues are gone from memory.
func TestWarmLoadedTimeHitKeepsServedFields_1180(t *testing.T) {
	ctx := context.Background()
	st, extra := openLeanStore(t, 3)

	idx := index.NewHNSWIndex("")
	for label, vec := range map[uint64][]float32{1: {0.1, 1}, 2: {1, 0}, 3: {0.2, 1}} {
		if err := idx.Upsert(ctx, vec, model.IndexPayload{ChunkID: label}); err != nil {
			t.Fatalf("Upsert(%d): %v", label, err)
		}
	}
	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, nil)
	loaded, err := cli.PreloadEmbeddedChunkMetadataForTest(ctx, st, svc)
	if err != nil || loaded != 3 {
		t.Fatalf("warm-load = (%d, %v), want (3, nil)", loaded, err)
	}

	hits, err := svc.Search(ctx, model.SearchQuery{Query: "fox", K: 1})
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search = (%d hits, %v), want 1 hit", len(hits), err)
	}
	h := hits[0]
	assertLeanHitFields(t, h)
	sp := h.Span
	assertLeanTimeSpan(t, sp)

	full := store.SpanFromRow("time", 0, leanWindowMS, extra)
	if len(full.Words) != leanWordsPerSpan || len(full.Cues) != leanCuesPerSpan {
		t.Fatalf("full span decodes %d words / %d cues, want %d / %d", len(full.Words), len(full.Cues), leanWordsPerSpan, leanCuesPerSpan)
	}
	if got, want := mcp.BuildOpenFileSpan(sp), mcp.BuildOpenFileSpan(full); !reflect.DeepEqual(got, want) {
		t.Fatalf("served span = %v, want the full span's %v", got, want)
	}
}

// assertLeanHitFields checks the hit-level fields the MCP search/ask/citation
// outputs serve for the warm-loaded chunk 2.
func assertLeanHitFields(t *testing.T, h model.SearchHit) {
	t.Helper()
	if h.ChunkID != 2 || h.RelPath != leanRelPath(1) || h.Title != "Talk 1" || h.DocType != "audio" ||
		h.RepType != "transcript" || !strings.HasPrefix(h.Snippet, "the quick brown fox") || h.Language != "en" {
		t.Fatalf("hit = %+v, want chunk 2 of %s with title, doc/rep type, snippet and language", h, leanRelPath(1))
	}
}

// assertLeanTimeSpan checks a warm-loaded time span keeps its bounds and every
// annotation field, and holds no word timings or cues.
func assertLeanTimeSpan(t *testing.T, sp model.Span) {
	t.Helper()
	if sp.Kind != "time" || sp.StartMS != 0 || sp.EndMS != leanWindowMS || sp.Speaker != "S1" ||
		sp.SpeakerLabel != "Anchor" || sp.Event != "interview" || sp.Language != "" {
		t.Fatalf("hit span = %+v, want the stored time span's bounds, speaker and event", sp)
	}
	if !reflect.DeepEqual(sp.Entities, []string{"team:otters"}) ||
		!reflect.DeepEqual(sp.Sources, []string{"playbyplay"}) ||
		!reflect.DeepEqual(sp.Attributes, map[string]string{"half": "bottom"}) {
		t.Fatalf("hit span = %+v, want the stored entities, sources and attributes", sp)
	}
	if sp.Words != nil || sp.Cues != nil {
		t.Fatalf("warm-loaded span holds %d words and %d cues, want none (#1180)", len(sp.Words), len(sp.Cues))
	}
}

// TestRegisteredChunkMetadataDropsWordTimings_1180 covers the embed worker's
// path: it registers spans straight from NextPending, which still carry word
// timings. The cache must drop them and keep the rest.
func TestRegisteredChunkMetadataDropsWordTimings_1180(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Upsert(context.Background(), []float32{1, 0}, model.IndexPayload{ChunkID: 7}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}, nil)
	in := leanTranscriptSpan(60000)
	svc.SetChunkMetadataForIndex("text", 7, model.SearchHit{RelPath: "a.mp3", DocType: "audio", Snippet: "s", Span: in})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "q", K: 1})
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search = (%d hits, %v), want 1 hit", len(hits), err)
	}
	sp := hits[0].Span
	if sp.Words != nil || sp.Cues != nil {
		t.Fatalf("registered span holds %d words and %d cues, want none", len(sp.Words), len(sp.Cues))
	}
	if hits[0].RelPath != "a.mp3" || sp.StartMS != 60000 || sp.EndMS != 60000+leanWindowMS || sp.Event != "interview" {
		t.Fatalf("hit = %+v, want rel_path, bounds and event kept", hits[0])
	}
	if len(in.Words) != leanWordsPerSpan || len(in.Cues) != leanCuesPerSpan {
		t.Fatalf("caller's span was mutated: %d words, %d cues", len(in.Words), len(in.Cues))
	}
}

// TestWarmLoadHeapPerChunk_1180 measures the live heap the warm-loaded cache
// costs per chunk, against the same cache built the pre-#1180 way (every
// span fully decoded). It is a measurement harness, skipped unless
// DIR2MCP_PERF=1 so `go test ./...` stays fast and free of heap-timing noise.
//
//	DIR2MCP_PERF=1 go test ./tests/retrieval -run WarmLoadHeapPerChunk -v
//	DIR2MCP_PERF=1 DIR2MCP_PERF_CHUNKS=100000 go test ./tests/retrieval -run WarmLoadHeapPerChunk -v
func TestWarmLoadHeapPerChunk_1180(t *testing.T) {
	if os.Getenv("DIR2MCP_PERF") != "1" {
		t.Skip("set DIR2MCP_PERF=1 to run the #1180 heap measurement")
	}
	n := 20000
	if raw := os.Getenv("DIR2MCP_PERF_CHUNKS"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			t.Fatalf("DIR2MCP_PERF_CHUNKS=%q: want a positive integer", raw)
		}
		n = v
	}
	ctx := context.Background()
	st, extra := openLeanStore(t, n)

	// Before: the cache as it was, one SearchHit per chunk with its span fully
	// decoded from extra_json (what ListEmbeddedChunkMetadata returned and
	// registerChunkMetadataLocked kept).
	before := liveHeapDelta(func() any {
		m := make(map[uint64]model.SearchHit)
		walkLeanCorpus(ctx, t, st, func(task model.ChunkTask) {
			hit := task.Metadata.ToSearchHit()
			hit.Span = store.SpanFromRow("time", hit.Span.StartMS, hit.Span.EndMS, extra)
			m[task.Label] = hit
		})
		return m
	})
	// After: the same map from the lean listing.
	leanMap := liveHeapDelta(func() any {
		m := make(map[uint64]model.SearchHit)
		walkLeanCorpus(ctx, t, st, func(task model.ChunkTask) { m[task.Label] = task.Metadata.ToSearchHit() })
		return m
	})
	// After, end to end: the retrieval service warm-loaded exactly as `up` does
	// (this also counts its per-label axis map).
	service := liveHeapDelta(func() any {
		svc := retrieval.NewService(st, index.NewHNSWIndex(""), nil, nil)
		if _, err := cli.PreloadEmbeddedChunkMetadataForTest(ctx, st, svc); err != nil {
			t.Fatalf("warm-load: %v", err)
		}
		return svc
	})
	per := func(b int64) int64 { return b / int64(n) }
	t.Logf("chunks=%d window=%dms words/chunk=%d cues/chunk=%d", n, leanWindowMS, leanWordsPerSpan, leanCuesPerSpan)
	t.Logf("before (full spans) map:  %6d B/chunk", per(before))
	t.Logf("after  (lean spans) map:  %6d B/chunk", per(leanMap))
	t.Logf("after  warm-loaded svc:   %6d B/chunk", per(service))
	t.Logf("saving: %d B/chunk; at 5.16M chunks %.2f GiB", per(before)-per(leanMap),
		float64(per(before)-per(leanMap))*5.16e6/(1<<30))
}

func walkLeanCorpus(ctx context.Context, t *testing.T, st *store.SQLiteStore, fn func(model.ChunkTask)) {
	t.Helper()
	var after int64
	for {
		page, err := st.ListEmbeddedChunkMetadata(ctx, "text", 5000, after)
		if err != nil {
			t.Fatalf("ListEmbeddedChunkMetadata: %v", err)
		}
		for _, task := range page {
			fn(task)
		}
		if len(page) < 5000 {
			return
		}
		after = int64(page[len(page)-1].Label)
	}
}

// liveHeapDelta reports how much live heap the value build returns retains,
// measured after a full GC on both sides.
func liveHeapDelta(build func() any) int64 {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	v := build()
	runtime.GC()
	runtime.ReadMemStats(&m1)
	runtime.KeepAlive(v)
	return int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
}
