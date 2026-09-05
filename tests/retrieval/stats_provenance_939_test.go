package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// model.Stats.CorpusStatsAvailable is PROVENANCE, set at the one place that
// can know it: retrieval.Service.Stats. These pin both branches, because the
// two produce snapshots that look alike in every counter and differ only in
// what is knowable — the ListFiles-only fallback cannot see chunk failures,
// so a nil FailureSummary means "unknown" under it and "none" under the real
// aggregate. dir2mcp_stats keys failed_chunks off this flag (#939 review).

// listOnlyStore satisfies model.Store and nothing more. It has no CorpusStats
// method, so Service.Stats must take the ErrNotImplemented fallback.
type listOnlyStore struct{ docs []model.Document }

func (s *listOnlyStore) Init(context.Context) error { return nil }
func (s *listOnlyStore) UpsertDocument(_ context.Context, d model.Document) error {
	s.docs = append(s.docs, d)
	return nil
}
func (s *listOnlyStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotImplemented
}
func (s *listOnlyStore) ListFiles(_ context.Context, _, _ string, limit, offset int) ([]model.Document, int64, error) {
	if offset >= len(s.docs) {
		return nil, int64(len(s.docs)), nil
	}
	end := offset + limit
	if end > len(s.docs) {
		end = len(s.docs)
	}
	return s.docs[offset:end], int64(len(s.docs)), nil
}
func (s *listOnlyStore) Close() error { return nil }

func TestStatsProvenance_FallbackIsNotAvailable_939(t *testing.T) {
	st := &listOnlyStore{docs: []model.Document{{RelPath: "a.md", DocType: "md", Status: "ok"}}}
	svc := retrieval.NewService(st, nil, nil, nil)

	stats, err := svc.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats via fallback must succeed: %v", err)
	}
	// The counters are real: the fallback is a working degraded mode.
	if stats.TotalDocs != 1 {
		t.Fatalf("fallback TotalDocs = %d, want 1", stats.TotalDocs)
	}
	// But it must not claim an aggregate it never had.
	if stats.CorpusStatsAvailable {
		t.Fatal("CorpusStatsAvailable = true through the ListFiles-only fallback: this is the flag that would let a consumer report unknown failures as zero")
	}
	if stats.FailureSummary != nil {
		t.Fatalf("fallback cannot see chunk failures, got %#v", stats.FailureSummary)
	}
}

func TestStatsProvenance_RealAggregateIsAvailable_939(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := retrieval.NewService(st, nil, nil, nil)

	stats, err := svc.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !stats.CorpusStatsAvailable {
		t.Fatal("CorpusStatsAvailable = false through a store that HAS CorpusStats: an intact corpus could never state zero")
	}
}
