package tests

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Cold start used to rehydrate the text index, then the code index, then walk
// the embedded-chunk metadata in small pages, all strictly one after the other
// (issue #429 F6). These tests pin that the two index loads now overlap, that a
// failure in either is still reported against the kind that failed rather than
// whichever goroutine lost the race, and that the metadata warm-load pages in
// bulk instead of paying the store's per-page filter hundreds of times.

// startBarrier releases its waiters only once `need` of them have arrived, so a
// waiter that is released proves the participants were in flight at the same
// time. Serial execution cannot satisfy it: the first arrival blocks until the
// (bounded) timeout expires, which the barrier records as a missed rendezvous
// instead of hanging the suite.
type startBarrier struct {
	mu      sync.Mutex
	arrived int
	need    int
	ready   chan struct{}
	missed  atomic.Bool
}

func newStartBarrier(need int) *startBarrier {
	return &startBarrier{need: need, ready: make(chan struct{})}
}

// arriveAndWait registers one participant and blocks until every participant
// has arrived or timeout elapses.
func (b *startBarrier) arriveAndWait(timeout time.Duration) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.need {
		close(b.ready)
	}
	b.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-b.ready:
	case <-timer.C:
		b.missed.Store(true)
	}
}

// overlapped reports whether every participant met the others in time.
func (b *startBarrier) overlapped() bool {
	b.mu.Lock()
	arrived := b.arrived
	b.mu.Unlock()
	return arrived == b.need && !b.missed.Load()
}

// barrierIndex is a model.Index + model.Persistable whose Load rendezvouses on
// a barrier, so two Loads complete only when they run concurrently.
type barrierIndex struct {
	barrier *startBarrier
	timeout time.Duration
	loadErr error

	closed atomic.Bool
}

func (i *barrierIndex) Upsert(context.Context, []float32, model.IndexPayload) error { return nil }
func (i *barrierIndex) Delete(context.Context, []uint64) error                      { return nil }
func (i *barrierIndex) Search(context.Context, []float32, int, model.Filter) ([]model.IndexHit, error) {
	return nil, nil
}
func (i *barrierIndex) Identity(context.Context) (string, error) { return "", nil }
func (i *barrierIndex) Reset(context.Context, string) error      { return nil }
func (i *barrierIndex) Close() error                             { i.closed.Store(true); return nil }
func (i *barrierIndex) Save(context.Context, string) error       { return nil }

func (i *barrierIndex) Load(context.Context, string) error {
	if i.barrier != nil {
		i.barrier.arriveAndWait(i.timeout)
	}
	return i.loadErr
}

// TestUpIndexLoadsRunConcurrently proves the text and code vector indices are
// rehydrated at the same time: each injected Load waits for the other, which
// only completes if the two loads overlap.
func TestUpIndexLoadsRunConcurrently(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	barrier := newStartBarrier(2)
	indices := map[string]*barrierIndex{}
	var indicesMu sync.Mutex

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIndex: func(cfg config.Config, kind string) (model.Index, string) {
			ix := &barrierIndex{barrier: barrier, timeout: raceScaled(3 * time.Second)}
			indicesMu.Lock()
			indices[kind] = ix
			indicesMu.Unlock()
			return ix, ""
		},
	})

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(5*time.Second))
		defer cancel()
		// Both loads have met by the time the server is up; stop it as soon as
		// the barrier releases so the test does not idle in the serve loop.
		go func() {
			select {
			case <-barrier.ready:
			case <-ctx.Done():
			}
			cancel()
		}()
		if code := app.RunWithContext(ctx, []string{"up", "--listen", "127.0.0.1:0"}); code != 0 {
			t.Fatalf("up exit code: got=%d want=0 stderr=%s", code, stderr.String())
		}
	})

	if !barrier.overlapped() {
		t.Fatal("text and code index loads did not overlap: they are still serial")
	}
	indicesMu.Lock()
	defer indicesMu.Unlock()
	if len(indices) != 2 {
		t.Fatalf("expected one index per kind, got %d: %v", len(indices), indices)
	}
}

// TestUpIndexLoadFailureAttribution pins that concurrency did not cost the
// error its identity: a failing load still names the kind that failed and exits
// with the index-load failure code, and when both fail the report is the
// deterministic text-first one rather than whichever goroutine lost the race.
func TestUpIndexLoadFailureAttribution(t *testing.T) {
	cases := []struct {
		name        string
		failKinds   map[string]bool
		wantMessage string
		wantAbsent  string
		wantClosed  string
	}{
		{
			name:        "text only",
			failKinds:   map[string]bool{"text": true},
			wantMessage: "load text index: boom",
			wantAbsent:  "load code index",
			wantClosed:  "code",
		},
		{
			name:        "code only",
			failKinds:   map[string]bool{"code": true},
			wantMessage: "load code index: boom",
			wantAbsent:  "load text index",
			wantClosed:  "text",
		},
		{
			name:        "both fail reports text",
			failKinds:   map[string]bool{"text": true, "code": true},
			wantMessage: "load text index: boom",
			wantAbsent:  "load code index",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("MISTRAL_API_KEY", "test-key")
			t.Setenv("DIR2MCP_AUTH_TOKEN", "")

			indices := map[string]*barrierIndex{}
			var indicesMu sync.Mutex

			var stdout, stderr bytes.Buffer
			app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
				NewIndex: func(cfg config.Config, kind string) (model.Index, string) {
					ix := &barrierIndex{}
					if tc.failKinds[kind] {
						ix.loadErr = errors.New("boom")
					}
					indicesMu.Lock()
					indices[kind] = ix
					indicesMu.Unlock()
					return ix, ""
				},
			})

			var code int
			withWorkingDir(t, tmp, func() {
				ctx, cancel := context.WithTimeout(context.Background(), raceScaled(10*time.Second))
				defer cancel()
				code = app.RunWithContext(ctx, []string{"up", "--listen", "127.0.0.1:0"})
			})

			if code == 0 {
				t.Fatalf("expected a non-zero exit code, got 0 stderr=%s", stderr.String())
			}
			if got := stderr.String(); !strings.Contains(got, tc.wantMessage) {
				t.Fatalf("stderr missing %q:\n%s", tc.wantMessage, got)
			}
			if got := stderr.String(); strings.Contains(got, tc.wantAbsent) {
				t.Fatalf("stderr should report exactly one failure, found %q:\n%s", tc.wantAbsent, got)
			}
			if tc.wantClosed != "" {
				indicesMu.Lock()
				sibling := indices[tc.wantClosed]
				indicesMu.Unlock()
				if sibling == nil {
					t.Fatalf("no %s index was constructed", tc.wantClosed)
				}
				if !sibling.closed.Load() {
					t.Fatalf("the surviving %s index was leaked instead of closed", tc.wantClosed)
				}
			}
		})
	}
}

// pagingChunkStore is a minimal model.Store that records how the startup
// warm-load pages through the embedded-chunk listing.
type pagingChunkStore struct {
	mu    sync.Mutex
	calls []preloadCall
}

type preloadCall struct {
	kind         string
	limit        int
	afterChunkID int64
}

func (s *pagingChunkStore) Init(context.Context) error                           { return nil }
func (s *pagingChunkStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *pagingChunkStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (s *pagingChunkStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (s *pagingChunkStore) Close() error { return nil }

// ListEmbeddedChunkMetadata serves the "text" kind one full page followed by a
// short one, and the "code" kind a single short page, so the caller's paging
// (page size, keyset seek, stop condition) is fully exercised.
func (s *pagingChunkStore) ListEmbeddedChunkMetadata(_ context.Context, indexKind string, limit int, afterChunkID int64) ([]model.ChunkTask, error) {
	s.mu.Lock()
	s.calls = append(s.calls, preloadCall{kind: indexKind, limit: limit, afterChunkID: afterChunkID})
	s.mu.Unlock()

	if indexKind != "text" || afterChunkID != 0 {
		return nil, nil
	}
	tasks := make([]model.ChunkTask, limit)
	for i := range tasks {
		tasks[i] = model.ChunkTask{
			Label:     uint64(i + 1),
			IndexKind: indexKind,
			Metadata:  model.ChunkMetadata{ChunkID: uint64(i + 1), RelPath: "a.md", DocType: "md"},
		}
	}
	return tasks, nil
}

// TestUpEmbeddedChunkMetadataPreloadPagesInBulk pins the cold-start warm-load's
// paging shape (issue #429 F6). The store re-evaluates its embedded-chunk filter
// per page, so the walk costs O(chunks^2 / page size): the page COUNT is what
// makes a large corpus wait, and a regression to a small page size is a
// multi-minute cold start, not a rounding error. It also pins the keyset seek,
// since an OFFSET-style rescan would reintroduce the same quadratic.
func TestUpEmbeddedChunkMetadataPreloadPagesInBulk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")

	st := &pagingChunkStore{}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return st },
	})

	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		if code := app.RunWithContext(ctx, []string{"up", "--listen", "127.0.0.1:0"}); code != 0 {
			t.Fatalf("up exit code: got=%d want=0 stderr=%s", code, stderr.String())
		}
	})

	st.mu.Lock()
	calls := append([]preloadCall(nil), st.calls...)
	st.mu.Unlock()

	if len(calls) != 3 {
		t.Fatalf("expected 3 listings (text full page, text short page, code short page), got %d: %+v", len(calls), calls)
	}
	// A page small enough to make the walk quadratic in practice is the very
	// regression this pins; 2000 is the floor, not the configured value.
	const minPageSize = 2000
	for i, call := range calls {
		if call.limit < minPageSize {
			t.Fatalf("call %d asked for %d rows per page, want at least %d", i, call.limit, minPageSize)
		}
	}
	if calls[0].kind != "text" || calls[0].afterChunkID != 0 {
		t.Fatalf("first listing should start the text walk at the beginning, got %+v", calls[0])
	}
	if calls[1].kind != "text" || calls[1].afterChunkID != int64(calls[0].limit) {
		t.Fatalf("second listing should keyset-seek past the full first page (after=%d), got %+v", calls[0].limit, calls[1])
	}
	if calls[2].kind != "code" || calls[2].afterChunkID != 0 {
		t.Fatalf("third listing should start the code walk at the beginning, got %+v", calls[2])
	}
}
