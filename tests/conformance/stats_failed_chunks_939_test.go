package conformance

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Whole-payload conformance for SPEC §15.6 `indexing.failed_chunks`
// (spec 0.60.0, dir2mcp #939), validated against the canonical stats.json in
// the pinned submodule — the same file a strict client validates against.
//
// It also carries the executable half of the count invariants. Draft-07 cannot
// express a comparison between sibling fields, so the schema alone CANNOT
// catch a producer whose numbers contradict each other; the spec says so and
// defers enforcement to the producer and to tests like this one. A server that
// reported `total: 5, retryable: 9` would validate perfectly and still be
// wrong, and an operator reading it could not tell which number to act on.

// seedFailedChunk inserts one chunk and marks it terminally failed in the
// given category, which is the state `failed_chunks` counts: still-failed
// chunks, from any run, not failures this run observed.
func seedFailedChunk(t *testing.T, st *store.SQLiteStore, relPath, category string) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument(%s): %v", relPath, err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	var chunkID int64
	if err := st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "h-" + relPath,
		})
		if rerr != nil {
			return rerr
		}
		id, cerr := tx.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: "body of " + relPath, TextHash: "th-" + relPath,
			IndexKind: "text", EmbeddingStatus: "pending",
		}, nil)
		chunkID = id
		return cerr
	}); err != nil {
		t.Fatalf("seed chunk for %s: %v", relPath, err)
	}
	if err := st.MarkFailedWithCategory(ctx, []uint64{uint64(chunkID)}, category, "provider refused the request"); err != nil {
		t.Fatalf("MarkFailedWithCategory(%s): %v", relPath, err)
	}
}

func TestStats_FailedChunksValidatesAndAddsUp_939(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Two retryable categories and one terminal one, so the retryable count is
	// a real subset rather than trivially equal to the total.
	seedFailedChunk(t, st, "a.md", "rate_limit")
	seedFailedChunk(t, st, "b.md", "rate_limit")
	seedFailedChunk(t, st, "c.md", "auth")
	seedFailedChunk(t, st, "d.md", "parse_error")

	cfg := defaultConfig()
	cfg.StateDir = tmp
	retriever := retrieval.NewService(st, nil, nil, nil)
	srv := newServerWithRetriever(t, cfg, retriever, mcp.WithStore(st))
	defer srv.Close()

	structured := callStatsStructured(t, srv, cfg)

	indexing, ok := structured["indexing"].(map[string]interface{})
	if !ok {
		t.Fatalf("indexing missing: %v", sortedKeys(structured))
	}
	raw, present := indexing["failed_chunks"]
	if !present {
		// Guard against a vacuous pass: without the subtree the schema check
		// below would validate a payload that never exercised this field.
		t.Fatalf("seeded four failed chunks but failed_chunks is absent: %v", sortedKeys(indexing))
	}
	failed, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("failed_chunks is not an object: %#v", raw)
	}

	// The contract a strict client enforces.
	assertCanonicalStats(t, structured)

	// The invariants the schema cannot enforce.
	sum, retrySum := sumFailedChunkEntries(t, failed["by_category"])
	total, _ := failed["total"].(float64)
	retryable, _ := failed["retryable"].(float64)
	if sum != total {
		t.Fatalf("by_category sums to %v but total says %v: an operator cannot tell which to act on", sum, total)
	}
	if retrySum != retryable {
		t.Fatalf("retryable entries sum to %v but retryable says %v", retrySum, retryable)
	}
	if retryable > total {
		t.Fatalf("retryable %v exceeds total %v", retryable, total)
	}
	if total != 4 {
		t.Fatalf("total = %v, want 4 seeded failures", total)
	}
	// rate_limit x2 + auth x1 are provider/environment faults; parse_error is
	// a property of the stored bytes and re-sending them re-fails.
	if retryable != 3 {
		t.Fatalf("retryable = %v, want 3 (the parse_error is terminal)", retryable)
	}
}

// sumFailedChunkEntries totals the by_category breakdown and, separately, the
// retryable share of it. It also enforces the per-entry rule that a zero-count
// category is omitted rather than emitted as 0.
func sumFailedChunkEntries(t *testing.T, raw interface{}) (sum, retrySum float64) {
	t.Helper()
	entries, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("by_category is not an array: %#v", raw)
	}
	for _, item := range entries {
		entry, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("by_category entry is not an object: %#v", item)
		}
		count, ok := entry["count"].(float64)
		if !ok || count < 1 {
			t.Fatalf("entry count must be a number >= 1 (zero-count categories are omitted): %#v", entry)
		}
		sum += count
		if entry["retryable"] == true {
			retrySum += count
		}
	}
	return sum, retrySum
}

// TestStats_AdvertisedIndexingMatchesCanonical_939 closes a gap the top-level
// comparison in #850 cannot see. That test compares the OUTER property set, so
// a NESTED optional field can be emitted while the served schema never
// declares it — and `indexing` is additionalProperties:false, which makes
// every payload carrying such a field invalid for a client validating against
// the schema the server itself published (the #387 class).
//
// Found by mutation: removing the `failed_chunks` declaration from the served
// schema left every existing test green.
func TestStats_AdvertisedIndexingMatchesCanonical_939(t *testing.T) {
	t.Parallel()
	served, ok := toolsListSchemas(t)[protocol.ToolNameStats].(map[string]interface{})
	if !ok {
		t.Fatalf("tools/list advertises no outputSchema for %s", protocol.ToolNameStats)
	}
	servedIndexing := nestedSchema(t, served, "indexing", "served")
	canonicalIndexing := nestedSchema(t, canonicalStatsTree(t), "indexing", "canonical")

	assertSameStrings(t, "stats.json", "indexing property",
		schemaPropertyNames(t, canonicalIndexing, "canonical indexing"),
		schemaPropertyNames(t, servedIndexing, "served indexing"))
	assertSameStrings(t, "stats.json", "indexing required field",
		schemaRequired(t, canonicalIndexing, "canonical indexing"),
		schemaRequired(t, servedIndexing, "served indexing"))
}

// nestedSchema returns schema.properties[name] as a schema node.
func nestedSchema(t *testing.T, schema map[string]interface{}, name, label string) map[string]interface{} {
	t.Helper()
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s schema declares no properties object", label)
	}
	node, ok := properties[name].(map[string]interface{})
	if !ok {
		t.Fatalf("%s schema declares no %q object: %#v", label, name, properties[name])
	}
	return node
}
