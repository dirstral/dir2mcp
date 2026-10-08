package tests

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
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

func TestServerDoctor1081_ReportsTheCachedBaseline(t *testing.T) {
	tmp := testutil.TempDir(t)
	t.Setenv("MISTRAL_API_KEY", "test-key")
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The resolved text embedding model under MISTRAL_API_KEY is mistral-embed;
	// the cache names the same model, so it is current.
	cache := `{"key":"mistral|mistral-embed|1024|v1|94","baseline":{"probes":32,"probe_set":"v1","p50":0.478,"p90":0.519,"max":0.548,"chunks":94,"embed_model":"mistral-embed","computed_at":"2026-10-06T14:00:00Z"}}`
	if err := os.WriteFile(filepath.Join(stateDir, "evidence_baseline.json"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	check := findCheck(runDoctorChecks(t, tmp), "evidence_threshold")
	if check == nil {
		t.Fatal("doctor report missing the evidence_threshold check")
	}
	for _, want := range []string{"cosine threshold 0.519 from the null baseline p90", "32 probes", "p50=0.478", "max=0.548", "mistral-embed", "94 chunks"} {
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
