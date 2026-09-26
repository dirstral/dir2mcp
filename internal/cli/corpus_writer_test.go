package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/model"
)

type mutableCorpusStore struct {
	mu   sync.RWMutex
	docs []model.Document
}

func (m *mutableCorpusStore) Init(context.Context) error { return nil }
func (m *mutableCorpusStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (m *mutableCorpusStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}
func (m *mutableCorpusStore) Close() error { return nil }

func (m *mutableCorpusStore) ListFiles(_ context.Context, _ string, _ string, limit, offset int) ([]model.Document, int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if offset >= len(m.docs) {
		return []model.Document{}, int64(len(m.docs)), nil
	}
	end := offset + limit
	if end > len(m.docs) {
		end = len(m.docs)
	}
	return append([]model.Document(nil), m.docs[offset:end]...), int64(len(m.docs)), nil
}

func (m *mutableCorpusStore) setDocs(docs []model.Document) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs = append([]model.Document(nil), docs...)
}

func TestRunCorpusWriterWithInterval_UpdatesSnapshotWhileRunning(t *testing.T) {
	stateDir := t.TempDir()
	store := &mutableCorpusStore{}
	store.setDocs([]model.Document{
		{RelPath: "src/a.go", DocType: "code"},
		{RelPath: "docs/a.md", DocType: "md"},
	})

	idxState := appstate.NewIndexingState(appstate.ModeIncremental)
	idxState.SetRunning(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCorpusWriterWithInterval(ctx, stateDir, store, idxState, io.Discard, nil, 20*time.Millisecond)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("corpus writer goroutine did not exit after cancel")
		}
	}()

	corpusPath := filepath.Join(stateDir, "corpus.json")
	waitForCondition(t, 2*time.Second, func() bool {
		_, err := os.Stat(corpusPath)
		return err == nil
	})

	initial := readCorpusFile(t, corpusPath)
	if initial.TotalDocs != 2 {
		t.Fatalf("expected initial total_docs=2, got %d", initial.TotalDocs)
	}
	eps := 1e-3
	if math.Abs(initial.CodeRatio-0.5) > eps {
		t.Fatalf("expected initial code_ratio around 0.5 (±%f), got %f", eps, initial.CodeRatio)
	}

	store.setDocs([]model.Document{
		{RelPath: "src/a.go", DocType: "code"},
		{RelPath: "docs/a.md", DocType: "md"},
		{RelPath: "docs/b.md", DocType: "md"},
	})

	waitForCondition(t, 2*time.Second, func() bool {
		updated, err := readCorpusFileMaybe(t, corpusPath)
		if err != nil {
			// ignore transient read/unmarshal errors during partial writes
			return false
		}
		return updated.TotalDocs == 3 && updated.DocCounts["md"] == 2
	})
	updated := readCorpusFile(t, corpusPath)
	if math.Abs(updated.CodeRatio-0.3333) > eps {
		t.Fatalf("expected updated code_ratio around 0.3333 (±%f), got %f", eps, updated.CodeRatio)
	}
}

func TestWriteCorpusSnapshot_ConcurrentWriters(t *testing.T) {
	stateDir := t.TempDir()
	store := &mutableCorpusStore{}
	store.setDocs([]model.Document{
		{RelPath: "src/a.go", DocType: "code"},
		{RelPath: "docs/a.md", DocType: "md"},
	})
	idxState := appstate.NewIndexingState(appstate.ModeIncremental)

	const writers = 16
	const writesPerWriter = 20

	var wg sync.WaitGroup
	errCh := make(chan error, writers*writesPerWriter)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writesPerWriter; j++ {
				if err := writeCorpusSnapshot(context.Background(), stateDir, store, idxState, io.Discard, nil); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("writeCorpusSnapshot failed under concurrent writes: %v", err)
	}

	// Ensure final corpus snapshot is valid JSON and has expected fields.
	final := readCorpusFile(t, filepath.Join(stateDir, "corpus.json"))
	if final.TotalDocs != 2 {
		t.Fatalf("expected total_docs=2, got %d", final.TotalDocs)
	}
	if final.DocCounts["code"] != 1 || final.DocCounts["md"] != 1 {
		t.Fatalf("unexpected doc counts: %#v", final.DocCounts)
	}
}

// A helper to parse ndjson emitter output into events.
func parseEvents(t *testing.T, buf *bytes.Buffer) []ndjsonEvent {
	t.Helper()
	var events []ndjsonEvent
	trimmed := bytes.TrimSpace(buf.Bytes())
	if len(trimmed) == 0 {
		// nothing was written
		return events
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	for _, line := range lines {
		var ev ndjsonEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

func TestWriteCorpusSnapshot_WithEmitter(t *testing.T) {
	stateDir := t.TempDir()
	store := &mutableCorpusStore{}
	// include one doc with an unexpected status to force emission
	store.setDocs([]model.Document{
		{RelPath: "src/a.go", DocType: "code", Status: "foo"},
	})
	idxState := appstate.NewIndexingState(appstate.ModeIncremental)

	var buf bytes.Buffer
	emitter := newNDJSONEmitter(&buf, true)
	if err := writeCorpusSnapshot(context.Background(), stateDir, store, idxState, io.Discard, emitter); err != nil {
		t.Fatalf("writeCorpusSnapshot failed: %v", err)
	}

	events := parseEvents(t, &buf)
	if len(events) == 0 {
		t.Fatal("expected emitter to produce at least one event")
	}
	found := false
	for _, ev := range events {
		if ev.Event == "unexpected_document_statuses" && ev.Level == "warning" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning unexpected_document_statuses event, got %+v", events)
	}

	// also verify the snapshot file still contains expected counts
	final := readCorpusFile(t, filepath.Join(stateDir, "corpus.json"))
	if final.TotalDocs != 1 {
		t.Fatalf("expected total_docs=1, got %d", final.TotalDocs)
	}
}

func TestWriteCorpusSnapshot_EmitterDisabled(t *testing.T) {
	stateDir := t.TempDir()
	store := &mutableCorpusStore{}
	store.setDocs([]model.Document{
		{RelPath: "src/a.go", DocType: "code", Status: "foo"},
	})
	idxState := appstate.NewIndexingState(appstate.ModeIncremental)

	var buf bytes.Buffer
	emitter := newNDJSONEmitter(&buf, false)
	if err := writeCorpusSnapshot(context.Background(), stateDir, store, idxState, io.Discard, emitter); err != nil {
		t.Fatalf("writeCorpusSnapshot failed: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output when emitter.disabled, got %q", buf.String())
	}
	// also ensure parseEvents handles the empty buffer gracefully
	events := parseEvents(t, &buf)
	if len(events) != 0 {
		t.Fatalf("expected no events from empty buffer, got %+v", events)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	if fn() {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(time.Duration(10+rand.Intn(11)) * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func readCorpusFile(t *testing.T, path string) corpusSnapshot {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	var snap corpusSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal corpus file: %v", err)
	}
	return snap
}

func readCorpusFileMaybe(t *testing.T, path string) (corpusSnapshot, error) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return corpusSnapshot{}, err
	}
	var snap corpusSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return corpusSnapshot{}, err
	}
	return snap, nil
}

// syncBuffer is a bytes.Buffer guarded by its own mutex so the test harness can
// inspect emitter output without racing the emitter's writes. The emitter must
// still serialize its own writes; this only protects the test's reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// TestNDJSONEmitter_ConcurrentEmitNoInterleave asserts that many goroutines
// sharing a single emitter produce non-interleaved, individually-valid JSON
// lines. Each event carries a payload large enough to exceed PIPE_BUF so that
// an unsynchronized fmt.Fprintln would interleave; the mutex in Emit prevents
// it. Run with -race to also catch the data race on e.out.
func TestNDJSONEmitter_ConcurrentEmitNoInterleave(t *testing.T) {
	t.Parallel()

	var out syncBuffer
	emitter := newNDJSONEmitter(&out, true)

	const goroutines = 32
	const perGoroutine = 50
	// Payload comfortably larger than the 512-byte POSIX PIPE_BUF minimum so a
	// non-atomic write would tear.
	payload := strings.Repeat("x", 2048)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				emitter.Emit("info", "query_metrics", map[string]any{
					"goroutine": id,
					"seq":       i,
					"blob":      payload,
				})
			}
		}(g)
	}
	wg.Wait()

	scanner := bufio.NewScanner(bytes.NewReader(out.Bytes()))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev ndjsonEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("corrupt/interleaved NDJSON line %d: %v\nline: %q", lines, err, string(line))
		}
		if ev.Event != "query_metrics" {
			t.Fatalf("unexpected event on line %d: %q", lines, ev.Event)
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan emitter output: %v", err)
	}
	if want := goroutines * perGoroutine; lines != want {
		t.Fatalf("expected %d non-interleaved lines, got %d", want, lines)
	}
}

// TestWriteConnectionFile_AtomicNoTempLeftover asserts that a successful write
// leaves a complete, valid file with its 0o600 mode preserved and no leftover
// temp files in the directory.
func TestWriteConnectionFile_AtomicNoTempLeftover(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "connection.json")
	payload := connectionPayload{
		Transport:   "http",
		URL:         "http://127.0.0.1:8080/mcp",
		Headers:     map[string]string{"Authorization": "Bearer redacted"},
		TokenSource: "file",
		TokenFile:   filepath.Join(dir, "token"),
	}

	if err := writeConnectionFile(path, payload); err != nil {
		t.Fatalf("writeConnectionFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat connection.json: %v", err)
	}
	// Windows has no POSIX mode bits, so the owner-only check is unix-only.
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("expected connection.json mode 0o600, got %o", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read connection.json: %v", err)
	}
	var got connectionPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("connection.json is not valid JSON: %v", err)
	}
	if got.URL != payload.URL || got.TokenFile != payload.TokenFile {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, payload)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Fatalf("leftover temp file after atomic write: %q", e.Name())
		}
	}
}

// TestWriteConnectionFile_ConcurrentReadNeverPartial hammers writeConnectionFile
// from one goroutine while another repeatedly reads the file; because the write
// is atomic (temp + rename) the reader must always see either no file or a
// complete, valid JSON document — never a truncated one.
func TestWriteConnectionFile_ConcurrentReadNeverPartial(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "connection.json")
	payload := connectionPayload{
		Transport: "http",
		URL:       "http://127.0.0.1:8080/mcp",
		Headers:   map[string]string{"Authorization": "Bearer " + strings.Repeat("a", 4096)},
	}

	const writes = 200
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			if err := writeConnectionFile(path, payload); err != nil {
				t.Errorf("writeConnectionFile: %v", err)
				return
			}
		}
		close(done)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) || windowsTransientOpenError(err) {
					continue
				}
				t.Errorf("read connection.json: %v", err)
				return
			}
			var got connectionPayload
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Errorf("reader saw partial/corrupt connection.json: %v\nbytes=%d", err, len(raw))
				return
			}
		}
	}()

	wg.Wait()
}

// windowsTransientOpenError reports a Windows open that failed only because a
// rename replaced the file at that moment (ERROR_ACCESS_DENIED = 5,
// ERROR_SHARING_VIOLATION = 32). The reader retries; the property under test
// is that a read never returns a partial file.
func windowsTransientOpenError(err error) bool {
	return runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32)))
}

// aggregateCorpusStore answers CorpusStats with fixed corpus-wide totals, which
// is what a sqlite state dir does. mutableCorpusStore deliberately does not, so
// only this one exercises the aggregate path.
type aggregateCorpusStore struct {
	mutableCorpusStore
	stats model.CorpusStats
}

func (a *aggregateCorpusStore) CorpusStats(context.Context) (model.CorpusStats, error) {
	return a.stats, nil
}

// TestWriteCorpusSnapshot_ProgressEventsStayPerRun_1005 pins the split the
// #1005 review asked for: corpus.json states the CORPUS, while scan_progress
// and embed_progress report THIS RUN. Feeding the corpus totals into the events
// would make an incremental pass over an indexed corpus announce its finished
// numbers on the first tick, so a client's progress display reads as complete
// before the run has scanned a file.
func TestWriteCorpusSnapshot_ProgressEventsStayPerRun_1005(t *testing.T) {
	stateDir := t.TempDir()
	store := &aggregateCorpusStore{stats: model.CorpusStats{
		DocCounts:       map[string]int64{"md": 400},
		TotalDocs:       400,
		Scanned:         400,
		Indexed:         400,
		Representations: 400,
		ChunksTotal:     487,
		EmbeddedOK:      487,
	}}

	// The run has just started: two files scanned, nothing embedded yet.
	idxState := appstate.NewIndexingState(appstate.ModeIncremental)
	idxState.SetRunning(true)
	idxState.AddScanned(2)
	idxState.AddIndexed(1)

	var buf bytes.Buffer
	emitter := newNDJSONEmitter(&buf, true)
	if err := writeCorpusSnapshot(context.Background(), stateDir, store, idxState, io.Discard, emitter); err != nil {
		t.Fatalf("writeCorpusSnapshot: %v", err)
	}

	events := decodeNDJSON(t, buf.String())
	scan, ok := events["scan_progress"]
	if !ok {
		t.Fatalf("no scan_progress event: %q", buf.String())
	}
	if got, _ := scan["scanned"].(float64); got != 2 {
		t.Errorf("scan_progress[scanned] = %v, want 2 (this run), not the corpus total", scan["scanned"])
	}
	embed, ok := events["embed_progress"]
	if !ok {
		t.Fatalf("no embed_progress event: %q", buf.String())
	}
	if got, _ := embed["embedded"].(float64); got != 0 {
		t.Errorf("embed_progress[embedded] = %v, want 0 (this run has embedded nothing)", embed["embedded"])
	}

	// The snapshot on disk is the other half of the split: it states the corpus,
	// which is what `status` and dir2mcp_stats report.
	snap := readCorpusFile(t, filepath.Join(stateDir, "corpus.json"))
	if snap.Indexing.EmbeddedOK != 487 {
		t.Errorf("corpus.json embedded_ok = %d, want 487 (the corpus, not the run)", snap.Indexing.EmbeddedOK)
	}
	if snap.Indexing.ChunksTotal != 487 {
		t.Errorf("corpus.json chunks_total = %d, want 487", snap.Indexing.ChunksTotal)
	}
}

// drainingCorpusStore reports aggregate stats that a test can change while the
// writer goroutine is reading them.
type drainingCorpusStore struct {
	mutableCorpusStore
	mu    sync.Mutex
	stats model.CorpusStats
}

func (d *drainingCorpusStore) CorpusStats(context.Context) (model.CorpusStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats, nil
}

func (d *drainingCorpusStore) setStats(s model.CorpusStats) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stats = s
}

func readEmbeddedOK(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	var snap struct {
		Indexing struct {
			EmbeddedOK      int64 `json:"embedded_ok"`
			EmbeddedPending int64 `json:"embedded_pending"`
		} `json:"indexing"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return -1
	}
	return snap.Indexing.EmbeddedOK
}

// TestRunCorpusWriterWithInterval_KeepsRefreshingWhileTheQueueDrains_1008 pins
// the rule that "still working" is not the same as "still scanning".
//
// Running goes false when the scan stops, but the embed worker keeps draining
// for minutes and every completion changes embedded_ok/embedded_pending. The
// writer used to skip its tick on !Running, so corpus.json froze on the numbers
// of a run that had embedded nothing: the "embedded=0 pending=487" report in
// #1005. #1007 stopped status, the bundle and the MCP tool from trusting the
// file; this stops the file from lying to anything that reads it raw (#1008).
func TestRunCorpusWriterWithInterval_KeepsRefreshingWhileTheQueueDrains_1008(t *testing.T) {
	stateDir := t.TempDir()
	store := &drainingCorpusStore{}
	store.setDocs([]model.Document{{RelPath: "a.md", DocType: "md"}})
	// Scanning has finished and 5 chunks are still queued.
	store.setStats(model.CorpusStats{
		DocCounts: map[string]int64{"md": 1}, TotalDocs: 1,
		ChunksTotal: 5, EmbeddedOK: 0, EmbeddedPending: 5,
	})

	// The decisive part of the setup: the run is NOT running.
	idxState := appstate.NewIndexingState(appstate.ModeIncremental)
	idxState.SetRunning(false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCorpusWriterWithInterval(ctx, stateDir, store, idxState, io.Discard, nil, 10*time.Millisecond)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("corpus writer goroutine did not exit after cancel")
		}
	}()

	corpusPath := filepath.Join(stateDir, "corpus.json")
	waitForCondition(t, 2*time.Second, func() bool {
		_, err := os.Stat(corpusPath)
		return err == nil
	})
	if got := readEmbeddedOK(t, corpusPath); got != 0 {
		t.Fatalf("initial embedded_ok = %d, want 0", got)
	}

	// The embed worker drains the queue while nothing is scanning.
	store.setStats(model.CorpusStats{
		DocCounts: map[string]int64{"md": 1}, TotalDocs: 1,
		ChunksTotal: 5, EmbeddedOK: 5, EmbeddedPending: 0,
	})

	waitForCondition(t, 2*time.Second, func() bool {
		return readEmbeddedOK(t, corpusPath) == 5
	})

	// And once the queue is empty with no run active, it must stop rewriting:
	// an idle daemon should not churn an unchanged file forever.
	store.setStats(model.CorpusStats{
		DocCounts: map[string]int64{"md": 1}, TotalDocs: 1,
		ChunksTotal: 5, EmbeddedOK: 99, EmbeddedPending: 0,
	})
	time.Sleep(150 * time.Millisecond) // many ticks at a 10ms interval
	if got := readEmbeddedOK(t, corpusPath); got != 5 {
		t.Fatalf("embedded_ok = %d after the queue drained; the writer kept "+
			"rewriting an idle corpus, want it parked at 5", got)
	}
}

// TestLockAtomicWritePath_SerializesOnePath pins the in-process serialization
// that keeps concurrent writers of one state file from racing each other's
// rename on Windows: a second lock on the same path, in any spelling, waits for
// the first, and a lock on a different path does not.
func TestLockAtomicWritePath_SerializesOnePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corpus.json")
	unlock := lockAtomicWritePath(path)

	other := make(chan struct{})
	go func() {
		defer close(other)
		lockAtomicWritePath(filepath.Join(dir, "connection.json"))()
	}()
	select {
	case <-other:
	case <-time.After(2 * time.Second):
		t.Fatal("a lock on a different path waited for the lock on corpus.json")
	}

	same := make(chan struct{})
	go func() {
		defer close(same)
		lockAtomicWritePath(filepath.Join(dir, "sub", "..", "corpus.json"))()
	}()
	select {
	case <-same:
		t.Fatal("a second lock on the same path did not wait for the first")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-same:
	case <-time.After(2 * time.Second):
		t.Fatal("the second lock on the same path did not get the lock after unlock")
	}
}
