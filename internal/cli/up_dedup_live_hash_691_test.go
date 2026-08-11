package cli

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// dedupHashIngestorStub records the content-hash callback wireIngestorHooks
// registers on it. It implements model.Ingestor plus the notifier interface
// under test, and nothing else.
type dedupHashIngestorStub struct {
	onDocHash func(relPath, contentHash string)
}

func (s *dedupHashIngestorStub) Run(context.Context) error { return nil }

func (s *dedupHashIngestorStub) Reindex(context.Context) error { return nil }

func (s *dedupHashIngestorStub) SetOnDocumentContentHash(fn func(relPath, contentHash string)) {
	s.onDocHash = fn
}

// TestWireIngestorHooks_RegistersContentHashHookWhenDedupOn pins the #691
// wiring: with dedup on, ingest reports every durable document write to the
// retrieval service, so cross-file dedup groups on live state.
func TestWireIngestorHooks_RegistersContentHashHookWhenDedupOn(t *testing.T) {
	ret := retrieval.NewService(nil, nil, nil, nil)
	ing := &dedupHashIngestorStub{}

	wireIngestorHooks(ing, ingestorHooks{
		onDocHash: crossFileDedupHashHook(config.Config{DedupRetrieval: true}, ret),
	})

	if ing.onDocHash == nil {
		t.Fatal("dedup on: ingest must report document content hashes")
	}
	// The registered callback must be usable by an ingest goroutine as it is.
	ing.onDocHash("a.md", "H1")
}

// TestWireIngestorHooks_NoContentHashHookWhenDedupOff pins the default: a corpus
// that does not use cross-file dedup carries no notification work.
func TestWireIngestorHooks_NoContentHashHookWhenDedupOff(t *testing.T) {
	ret := retrieval.NewService(nil, nil, nil, nil)
	ing := &dedupHashIngestorStub{}

	wireIngestorHooks(ing, ingestorHooks{
		onDocHash: crossFileDedupHashHook(config.Config{DedupRetrieval: false}, ret),
	})

	if ing.onDocHash != nil {
		t.Fatal("dedup off: no content-hash hook may be registered")
	}
}

// TestCrossFileDedupHashHook_NilServiceIsSafe pins that a missing retrieval
// service produces no hook rather than a nil-pointer call at ingest time.
func TestCrossFileDedupHashHook_NilServiceIsSafe(t *testing.T) {
	if crossFileDedupHashHook(config.Config{DedupRetrieval: true}, nil) != nil {
		t.Fatal("no retrieval service: no hook may be produced")
	}
}
