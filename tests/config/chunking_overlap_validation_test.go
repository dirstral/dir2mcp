package tests

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestChunkingOverlapMustBeLessThanMax pins the relational guard added for
// #405: when a chunk window is explicitly set (max_tokens > 0), an overlap that
// meets or exceeds it would never advance the window, so Validate must reject
// it. A zero window means "unset, use the chunker default" and is left alone.
func TestChunkingOverlapMustBeLessThanMax(t *testing.T) {
	t.Run("overlap equal to max is rejected", func(t *testing.T) {
		cfg := config.Default()
		cfg.ChunkingMaxTokens = 200
		cfg.ChunkingOverlapTokens = 200
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected overlap == max to fail validation")
		}
	})

	t.Run("overlap greater than max is rejected", func(t *testing.T) {
		cfg := config.Default()
		cfg.ChunkingMaxTokens = 200
		cfg.ChunkingOverlapTokens = 250
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected overlap > max to fail validation")
		}
	})

	t.Run("overlap less than max is accepted", func(t *testing.T) {
		cfg := config.Default()
		cfg.ChunkingMaxTokens = 200
		cfg.ChunkingOverlapTokens = 50
		if err := cfg.Validate(); err != nil {
			t.Fatalf("overlap < max must validate: %v", err)
		}
	})

	t.Run("unset window with nonzero overlap is not gated by the relation", func(t *testing.T) {
		cfg := config.Default()
		cfg.ChunkingMaxTokens = 0
		cfg.ChunkingOverlapTokens = 100
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unset max_tokens must skip the overlap<max relation: %v", err)
		}
	})
}
