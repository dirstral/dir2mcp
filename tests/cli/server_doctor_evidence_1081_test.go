package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Issue #1081, SPEC §9.4.3 (spec 0.80.0): `dir2mcp doctor` reports the cosine
// evidence threshold in effect and the null baseline the daemon cached in the
// state dir, without a daemon and without touching a provider.

func runDoctorChecks(t *testing.T, tmp string) []struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
} {
	t.Helper()
	var report struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		if code := app.Run([]string{"--json", "doctor"}); code != 0 {
			t.Fatalf("doctor exit=%d stderr=%q", code, stderr.String())
		}
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("decode doctor JSON: %v body=%q", err, stdout.String())
		}
	})
	return report.Checks
}

func TestServerDoctor1081_BaselineNotComputedYet(t *testing.T) {
	tmp := testutil.TempDir(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	check := findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil {
		t.Fatal("doctor report missing the evidence_threshold check")
	}
	if check.Status != "ok" {
		t.Fatalf("status = %q, want ok (informational row)", check.Status)
	}
	for _, want := range []string{"cosine threshold 0.050", "fixed floor", "not computed yet", "rerank 0.02"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("detail = %q, want substring %q", check.Detail, want)
		}
	}
}

// seedEmbeddedChunks creates meta.sqlite under stateDir with n embedded
// chunks, the count the daemon registers at load and keys the cache by.
func seedEmbeddedChunks(t *testing.T, stateDir string, n int) {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertDocument(ctx, model.Document{RelPath: "docs/a.md", DocType: "md", SourceType: "file", SizeBytes: 10, MTimeUnix: 1, ContentHash: "h1", Status: "ok"}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "docs/a.md")
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{DocID: doc.DocID, RepType: "raw_text", RepHash: "rep-hash"})
	if err != nil {
		t.Fatalf("upsert representation: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{RepID: repID, Ordinal: i, Text: fmt.Sprintf("chunk %d", i), TextHash: fmt.Sprintf("hash-%d", i), IndexKind: "text", EmbeddingStatus: "ok"},
			[]model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}}); err != nil {
			t.Fatalf("insert chunk %d: %v", i, err)
		}
	}
}

// writeBaselineCache writes the cache file the daemon would write for a
// baseline over chunks chunks of the resolved model, under the given key.
func writeBaselineCache(t *testing.T, stateDir, key string, chunks int) {
	t.Helper()
	cache := fmt.Sprintf(`{"key":%q,"baseline":{"probes":32,"probe_set":"v1","p50":0.478,"p90":0.519,"max":0.548,"chunks":%d,"embed_model":"mistral-embed","computed_at":"2026-10-06T14:00:00Z"}}`, key, chunks)
	if err := os.WriteFile(filepath.Join(stateDir, "evidence_baseline.json"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
}

// expectedBaselineKey is the key the daemon builds for this configuration:
// the configured embed identity, the resolved text model and the chunk count.
func expectedBaselineKey(t *testing.T, tmp string, chunks int) string {
	t.Helper()
	var key string
	testutil.WithWorkingDir(t, tmp, func() {
		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		key = retrieval.EvidenceBaselineKey(cfg.Providers().EmbedIdentity(), "mistral-embed", chunks)
	})
	return key
}

func TestServerDoctor1081_ReportsTheCachedBaseline(t *testing.T) {
	tmp := testutil.TempDir(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte("rag:\n  evidence_threshold: auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The resolved text embedding model under MISTRAL_API_KEY is mistral-embed,
	// the store holds 3 embedded chunks, and the cache names both, so its key
	// is the key the daemon builds: the cache is current.
	seedEmbeddedChunks(t, stateDir, 3)
	writeBaselineCache(t, stateDir, expectedBaselineKey(t, tmp, 3), 3)
	check := findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil {
		t.Fatal("doctor report missing the evidence_threshold check")
	}
	for _, want := range []string{"cosine threshold 0.519 from the null baseline p90", "32 probes", "p50=0.478", "max=0.548", "mistral-embed", "3 chunks"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("detail = %q, want substring %q", check.Detail, want)
		}
	}

	// A pinned threshold is named as such, and the baseline is still shown.
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte("rag:\n  evidence_threshold: 0.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check = findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil {
		t.Fatal("doctor report missing the evidence_threshold check")
	}
	for _, want := range []string{"cosine threshold 0.600 pinned by rag.evidence_threshold", "32 probes"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("pinned detail = %q, want substring %q", check.Detail, want)
		}
	}
}

// A cache computed over another chunk count is not the baseline the daemon
// applies: the service keys the cache by the chunk count and recomputes.
// doctor shows the floor and names both counts.
func TestServerDoctor1081_StaleChunkCountIsNotPresentedAsCurrent(t *testing.T) {
	tmp := testutil.TempDir(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte("rag:\n  evidence_threshold: auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedEmbeddedChunks(t, stateDir, 3)
	writeBaselineCache(t, stateDir, expectedBaselineKey(t, tmp, 94), 94)
	check := findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil {
		t.Fatal("doctor report missing the evidence_threshold check")
	}
	for _, want := range []string{"cosine threshold 0.050 (fixed floor)", "stale: computed over 94 chunks, the index holds 3", "recomputes"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("stale-count detail = %q, want substring %q", check.Detail, want)
		}
	}
	if strings.Contains(check.Detail, "0.519") {
		t.Fatalf("stale-count detail = %q, must not present the cached p90 as the threshold", check.Detail)
	}

	// A cache whose key differs only in the embed identity is stale too.
	writeBaselineCache(t, stateDir, "other-identity|mistral-embed|v1|3", 3)
	check = findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil || !strings.Contains(check.Detail, "stale: the embed identity changed") || strings.Contains(check.Detail, "0.519") {
		t.Fatalf("stale-identity detail = %+v, want the floor and the identity reason", check)
	}

	// With no store to count chunks, the cache cannot be verified and the
	// floor is reported.
	if err := os.Remove(filepath.Join(stateDir, "meta.sqlite")); err != nil {
		t.Fatal(err)
	}
	check = findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil || !strings.Contains(check.Detail, "cannot verify the cache against the index: no index store") || strings.Contains(check.Detail, "0.519") {
		t.Fatalf("unverifiable detail = %+v, want the floor and the reason", check)
	}
}

// A baseline cached under another text embedding model is stale: the floor
// is shown as the threshold, the cached p90 is not, and the row says why.
func TestServerDoctor1081_StaleBaselineIsNotPresentedAsCurrent(t *testing.T) {
	tmp := testutil.TempDir(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := `{"key":"openai|nomic-embed-text:latest|768|v1|94","baseline":{"probes":32,"probe_set":"v1","p50":0.478,"p90":0.519,"max":0.548,"chunks":94,"embed_model":"nomic-embed-text:latest"}}`
	if err := os.WriteFile(filepath.Join(stateDir, "evidence_baseline.json"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	check := findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil {
		t.Fatal("doctor report missing the evidence_threshold check")
	}
	for _, want := range []string{"cosine threshold 0.050 (fixed floor)", "stale", `model "nomic-embed-text:latest", configured "mistral-embed"`, "recomputes"} {
		if !strings.Contains(check.Detail, want) {
			t.Fatalf("stale detail = %q, want substring %q", check.Detail, want)
		}
	}
	if strings.Contains(check.Detail, "0.519") {
		t.Fatalf("stale detail = %q, must not present the cached p90 as the threshold", check.Detail)
	}
}
