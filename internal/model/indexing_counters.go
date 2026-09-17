package model

// IndexingCounters is the counter block that `dir2mcp status` prints and the
// dir2mcp_stats MCP tool returns. It exists so the two surfaces pass the same
// shape into ResolveIndexingCounters instead of each assembling its own.
type IndexingCounters struct {
	Scanned         int64
	Indexed         int64
	Skipped         int64
	Deleted         int64
	Representations int64
	ChunksTotal     int64
	EmbeddedOK      int64
	EmbeddedPending int64
	Errors          int64
	Unknown         int64
}

// ResolveIndexingCounters decides which source answers for the counter block,
// and is the ONE place that decision is made: `status` and dir2mcp_stats both
// call it, so the two surfaces cannot drift apart (#1005).
//
// The store aggregate wins whenever it ran, for every counter. Each one is a
// claim about the WHOLE corpus, and only the store holds that. `live` carries
// the in-process counters of the CURRENT indexing run: they start at zero on
// every daemon start and never see work any other process did.
//
// #1005 is the bill for mixing the two. An operator ran `reindex` with the
// daemon down, so the run that wrote 487 chunks was not the run whose counters
// `status` printed. The daemon then embedded all 487, but its own run counters
// still read embedded=0, while `pending` came from the store. The operator saw
// "embedded=0 pending=487 errors=0" (read: indexing is stuck) for a corpus that
// was fully embedded and that `ask` was answering from at that same moment.
//
// When the aggregate could not run, `agg` holds a best-effort reconstruction
// that cannot see chunks at all (the CLI marks those counters with the -1
// "unknown" sentinel). A live run that counted the work itself beats that, so
// `live` wins there. A nil `live` means no run is counting, which leaves the
// reconstruction as the only answer available.
func ResolveIndexingCounters(agg CorpusStats, aggAvailable bool, live *IndexingCounters) IndexingCounters {
	if !aggAvailable && live != nil {
		return *live
	}
	return IndexingCounters{
		Scanned:         agg.Scanned,
		Indexed:         agg.Indexed,
		Skipped:         agg.Skipped,
		Deleted:         agg.Deleted,
		Representations: agg.Representations,
		ChunksTotal:     agg.ChunksTotal,
		EmbeddedOK:      agg.EmbeddedOK,
		EmbeddedPending: agg.EmbeddedPending,
		Errors:          agg.Errors,
		Unknown:         agg.Unknown,
	}
}
