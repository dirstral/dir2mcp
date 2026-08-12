package tests

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestKDefault_OutOfBoundIsRejectedAtLoad pins the SPEC §9.1 bound: rag.k_default
// MUST satisfy the same 1..50 bound the k request field does, and a configured
// value outside it is CONFIG_INVALID AT LOAD.
//
// The failure has to happen here. A default that asks for a k the tool schemas
// forbid can otherwise only fail later, at a request the operator did not write,
// which is the worst place to discover it.
//
// On main the check was `k_default >= 0`, so 0 and 99 both loaded happily.
func TestKDefault_OutOfBoundIsRejectedAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	for _, k := range []int{-1, 0, 51, 1000} {
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			writeFile(t, path, fmt.Sprintf("rag_k_default: %d\n", k))
			_, err := config.LoadFile(path)
			if err == nil {
				t.Fatalf("rag.k_default: %d loaded without error", k)
			}
			want := fmt.Sprintf("rag.k_default must be between %d and %d", config.RAGKMin, config.RAGKMax)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error=%v want substring %q", err, want)
			}
		})
	}
}

// TestKDefault_InBoundLoads is the converse: every value the k request field
// accepts is a legal rag.k_default, including both ends of the bound.
func TestKDefault_InBoundLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	for _, k := range []int{config.RAGKMin, 15, 23, config.RAGKMax} {
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			writeFile(t, path, fmt.Sprintf("rag_k_default: %d\n", k))
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("rag.k_default: %d was rejected: %v", k, err)
			}
			if cfg.RAGKDefault != k {
				t.Fatalf("RAGKDefault=%d want %d", cfg.RAGKDefault, k)
			}
			if got := cfg.EffectiveKDefault(); got != k {
				t.Fatalf("EffectiveKDefault=%d want the configured %d", got, k)
			}
		})
	}
}

// TestKDefault_ShippedDefaultMatchesTheFallback pins the agreement issue #654
// asks for: config.Default(), the generated config and the runtime fallback all
// state ONE number, and it is the number the canonical §16 config template
// carries. Before this change Default() said 10 while every runtime path used
// 15, so introspection and behavior disagreed.
func TestKDefault_ShippedDefaultMatchesTheFallback(t *testing.T) {
	cfg := config.Default()
	if cfg.RAGKDefault != config.RAGKFallback {
		t.Fatalf("Default().RAGKDefault=%d want the shipped fallback %d", cfg.RAGKDefault, config.RAGKFallback)
	}
	if got := cfg.EffectiveKDefault(); got != config.RAGKFallback {
		t.Fatalf("Default().EffectiveKDefault()=%d want %d", got, config.RAGKFallback)
	}
	if config.RAGKFallback < config.RAGKMin || config.RAGKFallback > config.RAGKMax {
		t.Fatalf("the shipped fallback %d is outside its own bound %d..%d", config.RAGKFallback, config.RAGKMin, config.RAGKMax)
	}
}

// TestKDefault_UnsetConfigResolvesToTheFallback covers the third precedence step
// (SPEC §9.1): a Config that carries no rag.k_default resolves to the shipped
// fallback, never to 0, which would retrieve nothing.
func TestKDefault_UnsetConfigResolvesToTheFallback(t *testing.T) {
	var cfg config.Config
	if got := cfg.EffectiveKDefault(); got != config.RAGKFallback {
		t.Fatalf("EffectiveKDefault on an unset config = %d, want the fallback %d", got, config.RAGKFallback)
	}
}
