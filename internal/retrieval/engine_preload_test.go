package retrieval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

type blockingMetadataSource struct{}

func (blockingMetadataSource) ListEmbeddedChunkMetadata(ctx context.Context, _ string, _, _ int) ([]model.ChunkTask, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPreloadEngineChunkMetadata_ContextTimeout(t *testing.T) {
	t.Parallel()

	// we only care that preloadEngineChunkMetadata returns when the context
	// deadline is hit. the function will never dereference any fields of the
	// service if the metadata source blocks, so it's safe to construct the
	// service with nil dependencies. this keeps the test focused and avoids
	// needing to wire anything up.
	//
	// See preloadEngineChunkMetadata and blockingMetadataSource{} for the relevant logic.
	svc := NewService(nil, nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	total, err := preloadEngineChunkMetadata(ctx, blockingMetadataSource{}, svc)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded error, got: %v", err)
	}
	if total != 0 {
		t.Fatalf("total=%d want=0", total)
	}
}
