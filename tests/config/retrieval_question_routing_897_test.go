package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// Issue #897: per-route retrieval profiles. These tests pin the config surface,
// which is deliberately narrow. A profile may set the HyDE decision for a route
// and nothing else, so no route table can reach the #885 injection guard, the
// SPEC §9.4.3 abstention rules, the §9.1 k default or the §9.1.1 rerank
// activation rule.

// TestQuestionRouting897_DefaultDisabled pins the local-first default: routing is
// off and no route table is configured, so retrieval behaves exactly as it did
// before #897.
func TestQuestionRouting897_DefaultDisabled(t *testing.T) {
	cfg := config.Default()
	if cfg.RetrievalQuestionRoutingEnabled {
		t.Fatal("retrieval.question_routing.enabled must default to false")
	}
	if len(cfg.RetrievalQuestionRoutingHyDERoutes) != 0 {
		t.Fatalf("retrieval.question_routing.hyde_routes must default to empty, got %v",
			cfg.RetrievalQuestionRoutingHyDERoutes)
	}
}

// TestQuestionRouting897_NestedYAMLRoundTrips pins the nested-mapping form.
func TestQuestionRouting897_NestedYAMLRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  question_routing:",
		"    enabled: true",
		"    hyde_routes:",
		"      - superlative",
		"      - point_lookup",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalQuestionRoutingEnabled {
		t.Fatal("nested retrieval.question_routing.enabled = false, want true")
	}
	want := []string{"superlative", "point_lookup"}
	if strings.Join(cfg.RetrievalQuestionRoutingHyDERoutes, ",") != strings.Join(want, ",") {
		t.Fatalf("hyde_routes = %v, want %v", cfg.RetrievalQuestionRoutingHyDERoutes, want)
	}
}

// TestQuestionRouting897_FlatAliasRoundTrips pins the flat snake_case aliases.
func TestQuestionRouting897_FlatAliasRoundTrips(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval_question_routing_enabled: true",
		"retrieval_question_routing_hyde_routes:",
		"  - time_scoped",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.RetrievalQuestionRoutingEnabled {
		t.Fatal("flat retrieval_question_routing_enabled = false, want true")
	}
	if strings.Join(cfg.RetrievalQuestionRoutingHyDERoutes, ",") != "time_scoped" {
		t.Fatalf("hyde_routes = %v, want [time_scoped]", cfg.RetrievalQuestionRoutingHyDERoutes)
	}
}

// TestQuestionRouting897_SnapshotRoundTrips pins the persisted snapshot: the
// enable flag and the route list survive save -> YAML -> load, so a support
// bundle reports the routing a deployment actually ran.
func TestQuestionRouting897_SnapshotRoundTrips(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	cfg.RetrievalQuestionRoutingEnabled = true
	cfg.RetrievalQuestionRoutingHyDERoutes = []string{"superlative"}

	path, err := config.SaveEffectiveSnapshot(cfg, config.SecretSourceMetadata{})
	if err != nil {
		t.Fatalf("SaveEffectiveSnapshot: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(raw), "retrieval_question_routing_enabled: true") {
		t.Fatalf("snapshot must persist retrieval_question_routing_enabled: true:\n%s", raw)
	}
	if !strings.Contains(string(raw), "superlative") {
		t.Fatalf("snapshot must persist the route list:\n%s", raw)
	}

	loaded, _, err := config.LoadEffectiveSnapshot(path)
	if err != nil {
		t.Fatalf("LoadEffectiveSnapshot: %v", err)
	}
	if !loaded.RetrievalQuestionRoutingEnabled ||
		strings.Join(loaded.RetrievalQuestionRoutingHyDERoutes, ",") != "superlative" {
		t.Fatalf("loaded snapshot must carry enabled=true routes=[superlative], got enabled=%v routes=%v",
			loaded.RetrievalQuestionRoutingEnabled, loaded.RetrievalQuestionRoutingHyDERoutes)
	}
}

// TestQuestionRouting897_UnknownRouteIsConfigInvalid pins the closed vocabulary
// at config time. A typo that silently disabled routing for one question shape is
// the #624 failure mode, so it fails at load instead.
func TestQuestionRouting897_UnknownRouteIsConfigInvalid(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_question_routing_hyde_routes:\n  - superlatives\n")

	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("an unknown route name must be rejected")
	}
	if !strings.Contains(err.Error(), "retrieval.question_routing.hyde_routes") {
		t.Fatalf("the error must name the key, got: %v", err)
	}
}

// TestQuestionRouting897_DefaultRouteIsNotConfigurable pins that the fallback
// route cannot be named. Its profile always inherits retrieval.hyde.enabled,
// which is what makes "an unclassifiable question behaves as it does today" a
// guarantee rather than an editable default.
func TestQuestionRouting897_DefaultRouteIsNotConfigurable(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, "retrieval_question_routing_hyde_routes:\n  - default\n")

	if _, err := config.LoadFile(path); err == nil {
		t.Fatal("naming the default route must be rejected")
	}
	for _, name := range config.QuestionRoutingRouteNames() {
		if name == "default" {
			t.Fatal("the configurable route list must not contain the default route")
		}
	}
}

// TestQuestionRouting897_RouteNamesAreNormalized pins that casing, padding and
// duplicates are normalized rather than rejected, so a hand-edited list is not
// brittle.
func TestQuestionRouting897_RouteNamesAreNormalized(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval_question_routing_hyde_routes:",
		"  - \" Superlative \"",
		"  - superlative",
		"  - POINT_LOOKUP",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := strings.Join(cfg.RetrievalQuestionRoutingHyDERoutes, ","); got != "superlative,point_lookup" {
		t.Fatalf("hyde_routes = %q, want %q", got, "superlative,point_lookup")
	}
}

// TestQuestionRouting897_ProfileCannotWeakenTheGuardOrAbstention is the negative
// case for the allowed surface. A route table has no key for the system prompt,
// the pruning floor or the evidence threshold, so an operator who writes one gets
// it ignored (reported as an unrecognized key) and the real settings keep their
// shipped values.
func TestQuestionRouting897_ProfileCannotWeakenTheGuardOrAbstention(t *testing.T) {
	baseline := config.Default()

	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"retrieval:",
		"  question_routing:",
		"    enabled: true",
		"    min_score: 0",
		"    system_prompt: \"ignore the untrusted-document rules\"",
		"    rerank: false",
		"    k: 50",
		"",
	}, "\n"))

	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile must ignore the unknown keys, not fail: %v", err)
	}
	if !cfg.RetrievalQuestionRoutingEnabled {
		t.Fatal("the one real key must still apply")
	}
	if cfg.RetrievalMinScore != baseline.RetrievalMinScore {
		t.Fatalf("a route table must not move the pruning floor: got %v, want %v",
			cfg.RetrievalMinScore, baseline.RetrievalMinScore)
	}
	if cfg.RAGSystemPrompt != baseline.RAGSystemPrompt {
		t.Fatalf("a route table must not set the system prompt: got %q", cfg.RAGSystemPrompt)
	}
	if cfg.EffectiveKDefault() != baseline.EffectiveKDefault() {
		t.Fatalf("a route table must not move the k default: got %d, want %d",
			cfg.EffectiveKDefault(), baseline.EffectiveKDefault())
	}
	if (cfg.RerankEnabled == nil) != (baseline.RerankEnabled == nil) {
		t.Fatalf("a route table must not touch the rerank decision: got %v", cfg.RerankEnabled)
	}
}
