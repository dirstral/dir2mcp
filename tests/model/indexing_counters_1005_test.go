package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #1005: `dir2mcp status` and the dir2mcp_stats MCP tool each decided for
// themselves which counters to report, and drifted. ResolveIndexingCounters is
// the one decision both now make; these tests pin the three cases it has.

// TestResolveIndexingCountersPrefersAggregate1005 is the #1005 case itself: the
// store says 487 chunks are embedded, the current run embedded none of them
// (the work was done by an offline `reindex` plus an earlier drain), and the
// corpus-wide answer is the one an operator can act on.
func TestResolveIndexingCountersPrefersAggregate1005(t *testing.T) {
	got := model.ResolveIndexingCounters(model.CorpusStats{
		Scanned: 8, Indexed: 8, Representations: 8,
		ChunksTotal: 487, EmbeddedOK: 487, EmbeddedPending: 0, Errors: 0,
	}, true, &model.IndexingCounters{
		Scanned: 8, Indexed: 8, Representations: 0,
		ChunksTotal: 0, EmbeddedOK: 0, Errors: 0,
	})

	if got.EmbeddedOK != 487 {
		t.Errorf("EmbeddedOK=%d, want 487 (the store counted the whole corpus)", got.EmbeddedOK)
	}
	if got.ChunksTotal != 487 {
		t.Errorf("ChunksTotal=%d, want 487", got.ChunksTotal)
	}
	if got.EmbeddedPending != 0 {
		t.Errorf("EmbeddedPending=%d, want 0 (read in the same pass as EmbeddedOK)", got.EmbeddedPending)
	}
	if got.Representations != 8 {
		t.Errorf("Representations=%d, want 8", got.Representations)
	}
}

// TestResolveIndexingCountersUsesLiveRunWithoutAggregate1005 covers the store
// that cannot aggregate: its reconstruction carries the -1 "unknown" sentinel
// for everything chunk-level, so a run that counted the work itself is the
// better answer.
func TestResolveIndexingCountersUsesLiveRunWithoutAggregate1005(t *testing.T) {
	got := model.ResolveIndexingCounters(model.CorpusStats{
		Scanned: 4, Representations: -1, ChunksTotal: -1, EmbeddedOK: -1,
	}, false, &model.IndexingCounters{
		Scanned: 4, Indexed: 4, Representations: 4, ChunksTotal: 40, EmbeddedOK: 40,
	})

	if got.EmbeddedOK != 40 {
		t.Errorf("EmbeddedOK=%d, want 40 (the run counted what the store cannot)", got.EmbeddedOK)
	}
	if got.ChunksTotal != 40 {
		t.Errorf("ChunksTotal=%d, want 40", got.ChunksTotal)
	}
}

// TestResolveIndexingCountersKeepsUnknownSentinelWithoutRun1005 pins the last
// case: no aggregate and no run counting. The -1 sentinel must survive, because
// "nobody counted" and "counted zero" are different claims (SPEC §3.2).
func TestResolveIndexingCountersKeepsUnknownSentinelWithoutRun1005(t *testing.T) {
	got := model.ResolveIndexingCounters(model.CorpusStats{
		Scanned: 4, Indexed: 3, Representations: -1, ChunksTotal: -1, EmbeddedOK: -1,
	}, false, nil)

	if got.Indexed != 3 {
		t.Errorf("Indexed=%d, want 3 (the reconstruction can still count documents)", got.Indexed)
	}
	if got.EmbeddedOK != -1 {
		t.Errorf("EmbeddedOK=%d, want -1 (unknown must not read as zero)", got.EmbeddedOK)
	}
	if got.ChunksTotal != -1 {
		t.Errorf("ChunksTotal=%d, want -1", got.ChunksTotal)
	}
}
