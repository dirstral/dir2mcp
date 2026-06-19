package cli

import (
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// TestResolveRerankProfile_ColBERTSelector verifies rerank.provider=colbert
// routes to the built-in self-hosted ColBERT profile (dir2mcp#337), as an
// alternative to the hosted cohere path.
func TestResolveRerankProfile_ColBERTSelector(t *testing.T) {
	t.Setenv("COLBERT_BASE_URL", "http://colbert.internal:9000")
	cfg := config.Default()
	cfg.RerankProvider = "colbert"

	p, err, ok := resolveRerankProfile(cfg, true)
	if !ok {
		t.Fatal("colbert selector must be recognised")
	}
	if err != nil {
		t.Fatalf("resolve error: %v", err)
	}
	if p.Kind != provider.KindColBERT {
		t.Fatalf("kind = %q, want colbert", p.Kind)
	}
	if p.BaseURL != "http://colbert.internal:9000" {
		t.Fatalf("base_url = %q, want env-expanded value", p.BaseURL)
	}
}

// TestResolveRerankProfile_CohereSelectorUsesAuto verifies the default
// cohere selector takes the auto-resolution path: with a credential it
// resolves cohere, without one it stays unresolved (rerank off) — unchanged
// pre-#337 behavior.
func TestResolveRerankProfile_CohereSelectorUsesAuto(t *testing.T) {
	cfg := config.Default() // RerankProvider defaults to "cohere"

	t.Run("with credential resolves cohere", func(t *testing.T) {
		t.Setenv("COHERE_API_KEY", "k")
		p, err, ok := resolveRerankProfile(cfg, true)
		if !ok || err != nil {
			t.Fatalf("want ok cohere resolve, got ok=%v err=%v", ok, err)
		}
		if p.Kind != provider.KindCohere {
			t.Fatalf("kind = %q, want cohere", p.Kind)
		}
	})

	t.Run("without credential stays unresolved", func(t *testing.T) {
		t.Setenv("COHERE_API_KEY", "")
		_, err, ok := resolveRerankProfile(cfg, true)
		if !ok {
			t.Fatal("cohere selector must still take the (recognised) auto path")
		}
		if err == nil {
			t.Fatal("auto cohere without credential must not resolve")
		}
	})
}

// TestResolveRerankProfile_UnknownSelectorInert verifies an unrecognised
// rerank.provider value is reported as not-ok, so configureReranker leaves
// reranking off (preserving the prior "unsupported provider disables
// rerank" behavior).
func TestResolveRerankProfile_UnknownSelectorInert(t *testing.T) {
	cfg := config.Default()
	cfg.RerankProvider = "totally-unknown"
	if _, _, ok := resolveRerankProfile(cfg, true); ok {
		t.Fatal("unknown rerank.provider selector must be inert (ok=false)")
	}
}
