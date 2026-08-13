package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// The two store read paths a retrieval candidate comes from must both carry a
// recognition annotation's attribution (issue #856, design 0004 §7).
//
// The filter judges a candidate on the span the candidate carries, so a read
// path that drops the attribution turns the filter into "reject everything" for
// the candidates it produces. Both paths are joined reads that already select
// the span row, so the attribution costs no extra query:
//
//   - SearchBM25 is the lexical candidate. Its FTS query joins the spans table.
//   - ListEmbeddedChunkMetadata is the daemon's warm load, which becomes the
//     in-memory chunk metadata the vector path materialises a hit from.

const (
	attributionEvent    = "home_run"
	attributionEntityID = "player:heliot-ramos"
)

// embeddedAnnotationChunk seeds a live document, a recognition representation,
// and one embedded chunk whose "time" span carries the attribution.
func embeddedAnnotationChunk(t *testing.T, st *store.SQLiteStore, relPath, text string, span model.Span) uint64 {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "video", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID: doc.DocID, RepType: "recognition", RepHash: relPath, CreatedUnix: 1,
	})
	if err != nil {
		t.Fatalf("UpsertRepresentation: %v", err)
	}
	chunkID, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID: repID, Ordinal: 0, Text: text,
		IndexKind: "text", EmbeddingStatus: "ok",
	}, []model.Span{span})
	if err != nil {
		t.Fatalf("InsertChunkWithSpans: %v", err)
	}
	return uint64(chunkID)
}

func attributionStore(t *testing.T) (*store.SQLiteStore, uint64) {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	chunkID := embeddedAnnotationChunk(t, st, "game.mp4",
		"Heliot Ramos hits a home run to left field",
		model.Span{
			Kind: "time", StartMS: 3346398, EndMS: 3354398,
			Entities: []string{attributionEntityID, "team:san-francisco-giants"},
			Event:    attributionEvent,
		})
	return st, chunkID
}

func assertAttribution(t *testing.T, path string, span model.Span) {
	t.Helper()
	if span.Event != attributionEvent {
		t.Fatalf("%s: event = %q, want %q", path, span.Event, attributionEvent)
	}
	// Exact membership, not a substring of the joined ids: an id is an opaque
	// token, and "player:heliot-ramos-old" must not pass for
	// "player:heliot-ramos".
	found := false
	for _, entityID := range span.Entities {
		if entityID == attributionEntityID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("%s: entities = %v, want %s among them", path, span.Entities, attributionEntityID)
	}
}

// TestALexicalHitCarriesTheAnnotationAttribution: the lexical candidate the
// hybrid path fuses must arrive with its attribution, because the filter now
// judges it (before #856 it was fused unjudged).
func TestALexicalHitCarriesTheAnnotationAttribution(t *testing.T) {
	st, chunkID := attributionStore(t)
	hits, err := st.SearchBM25(context.Background(), "home run", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	for _, hit := range hits {
		if hit.ChunkID != chunkID {
			continue
		}
		assertAttribution(t, "SearchBM25", hit.Span)
		return
	}
	t.Fatalf("SearchBM25 returned %d hits, none of them chunk %d", len(hits), chunkID)
}

// TestWarmLoadedMetadataCarriesTheAnnotationAttribution: after a restart the
// vector path materialises a hit from this metadata, so the attribution must
// survive the reload as well as the first indexing run.
func TestWarmLoadedMetadataCarriesTheAnnotationAttribution(t *testing.T) {
	st, chunkID := attributionStore(t)
	tasks, err := st.ListEmbeddedChunkMetadata(context.Background(), "text", 100, 0)
	if err != nil {
		t.Fatalf("ListEmbeddedChunkMetadata: %v", err)
	}
	for _, task := range tasks {
		if task.Metadata.ChunkID != chunkID {
			continue
		}
		assertAttribution(t, "ListEmbeddedChunkMetadata", task.Metadata.Span)
		assertAttribution(t, "ChunkMetadata.ToSearchHit", task.Metadata.ToSearchHit().Span)
		return
	}
	t.Fatalf("warm load returned %d chunks, none of them chunk %d", len(tasks), chunkID)
}
