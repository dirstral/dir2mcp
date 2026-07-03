package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
)

// TestForegroundRefusesSecondInstance pins the #434 fix: `up --foreground`
// (the launchd/systemd service target) must take the single-instance pid
// lock, not just the double-fork daemon child. Two writers on the same
// meta.sqlite + HNSW index files corrupt the index, so a foreground start
// that finds a live owner must refuse and exit non-zero.
//
// The test pre-plants a pid file pointing at the (live) test process, then
// starts `up --foreground`; the guard must bail before serving.
func TestForegroundRefusesSecondInstance(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	// A pid file whose owner is alive (this test process) simulates an already
	// running server for the same corpus.
	if err := os.WriteFile(filepath.Join(stateDir, "server.pid"), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		// Generous timeout: if the guard is broken the server would serve until
		// this fires (and exit 0), so a fast non-zero return is the pass signal.
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(10*time.Second))
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
		if code == 0 {
			t.Fatalf("foreground up succeeded despite a live pid file; single-instance guard not applied. stderr=%s", stderr.String())
		}
	})
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("expected an 'already running' refusal, got stderr=%s", stderr.String())
	}
}

// TestForegroundCleansStalePidAndStarts verifies the guard reconciles a
// stale pid file (owner dead) rather than blocking a legitimate restart.
// A pid that is not alive must be removed and the foreground server must
// then acquire the lock and run until the context ends.
func TestForegroundCleansStalePidAndStarts(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	// PID 2^31-1 is (practically) never a live process, so the guard should
	// treat this pid file as stale, remove it, and proceed.
	if err := os.WriteFile(filepath.Join(stateDir, "server.pid"), []byte("2147483647\n"), 0o600); err != nil {
		t.Fatalf("write stale pid file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), raceScaled(2*time.Second))
		defer cancel()
		code := app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
		if code != 0 {
			t.Fatalf("foreground up refused to start over a stale pid file: code=%d stderr=%s", code, stderr.String())
		}
	})
	if strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stale pid file was treated as a live instance: %s", stderr.String())
	}
}

// TestEmbedWorkerRejectsInProcessMemoryBroker pins the #434 fix: the
// standalone embed-worker must refuse the in-process "memory" broker,
// whose queue is process-local, so the worker would create its own empty
// queue, never see the daemon's jobs, and silently no-op. All other
// distributed prerequisites are satisfied so the broker check is what
// fails.
func TestEmbedWorkerRejectsInProcessMemoryBroker(t *testing.T) {
	tmp := t.TempDir()
	// Distributed on + a shared Tier-C backend (qdrant) so we get past those
	// prerequisites; the broker is left unset, defaulting to the in-process
	// memory broker that the standalone worker must reject.
	writeWorkerConfig(t, tmp, "root_dir: .\nstate_dir: .dir2mcp\n"+
		"index_backend: qdrant\nqdrant_url: http://127.0.0.1:6334\n"+
		"distributed_embed_enabled: true\n")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "embed-worker"})
		if code != 2 {
			t.Fatalf("unexpected exit code: got=%d want=2 stderr=%s", code, stderr.String())
		}
	})

	payload := decodeCLIError(t, stderr.Bytes())
	if payload.Error.Code != "CONFIG_INVALID" || payload.ExitCode != 2 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	msg := strings.ToLower(payload.Error.Message)
	if !strings.Contains(msg, "in-process") || !strings.Contains(msg, "memory") || !strings.Contains(msg, "broker") {
		t.Fatalf("expected an in-process/memory broker rejection, got: %q", payload.Error.Message)
	}
}

// TestDoctorDeepProbeFailsOnBadCreds pins the #434 fix: `doctor --deep`
// must actively probe the embedding provider so a present-but-invalid
// credential fails instead of passing as ok (construct-only never touches
// the network). The provider points at a local endpoint returning 401.
func TestDoctorDeepProbeFailsOnBadCreds(t *testing.T) {
	// Local OpenAI-compatible endpoint that rejects every embed request, as a
	// bad/expired key would.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	writeWorkerConfig(t, tmp, ""+
		"root_dir: .\nstate_dir: .dir2mcp\n"+
		"providers:\n"+
		"  probe:\n"+
		"    kind: openai\n"+
		"    base_url: "+srv.URL+"/v1\n"+
		"    embed_text_model: probe-model\n"+
		"model:\n"+
		"  embed:\n"+
		"    provider: probe\n")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		code := app.RunWithContext(context.Background(), []string{"--json", "doctor", "--deep"})
		if code == 0 {
			t.Fatalf("doctor --deep passed with an unauthorized embed endpoint; probe not run. stdout=%s", stdout.String())
		}
	})

	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor JSON: %v body=%q", err, stdout.String())
	}
	var embed *struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	for i := range report.Checks {
		if report.Checks[i].Name == "provider.embed" {
			embed = &report.Checks[i]
		}
	}
	if embed == nil {
		t.Fatalf("doctor report missing provider.embed check: %+v", report.Checks)
	}
	if embed.Status != "error" {
		t.Fatalf("provider.embed status = %q, want error (probe hit a 401); detail=%q", embed.Status, embed.Detail)
	}
	if !strings.Contains(embed.Detail, "probe") {
		t.Fatalf("provider.embed detail = %q, want a probe-failure explanation", embed.Detail)
	}
}

// TestDoctorWithoutDeepDoesNotProbe verifies the default (non-deep) doctor
// stays construct-only: the same bad-credential endpoint must NOT be probed,
// so provider.embed resolves ok and no network call is made.
func TestDoctorWithoutDeepDoesNotProbe(t *testing.T) {
	probed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probed = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tmp := t.TempDir()
	writeWorkerConfig(t, tmp, ""+
		"root_dir: .\nstate_dir: .dir2mcp\n"+
		"providers:\n"+
		"  probe:\n"+
		"    kind: openai\n"+
		"    base_url: "+srv.URL+"/v1\n"+
		"    embed_text_model: probe-model\n"+
		"model:\n"+
		"  embed:\n"+
		"    provider: probe\n")

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	withWorkingDir(t, tmp, func() {
		_ = app.RunWithContext(context.Background(), []string{"--json", "doctor"})
	})
	if probed {
		t.Fatalf("default doctor probed the provider endpoint; probing must be gated behind --deep. stderr=%s", stderr.String())
	}
}
