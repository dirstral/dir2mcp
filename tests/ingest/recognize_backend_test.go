package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

func healthyStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitFor polls cond up to 5s. The managed-backend lifecycle is asynchronous
// (SIGTERM + reap), so assertions on process death must poll.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRecognizeBackend_ManagedLifecycle_LaunchHealthyAndKilledOnShutdown(t *testing.T) {
	t.Parallel()
	stub := healthyStub(t)
	root := t.TempDir()

	cfg := config.Config{
		RootDir: root, StateDir: t.TempDir(),
		RecognizeProvider:     "serve",
		RecognizeServeURL:     stub.URL,
		RecognizeServeCommand: "sleep 60",
	}
	svc := mustNewIngestService(t, cfg, &fakeIngestStore{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.StartRecognizeBackend(ctx); err != nil {
		t.Fatalf("StartRecognizeBackend: %v", err)
	}
	pid := svc.RecognizeBackendPID()
	if pid == 0 || !pidAlive(pid) {
		t.Fatalf("expected a live managed backend process, pid=%d", pid)
	}

	// Daemon shutdown (ctx cancel) must terminate the child.
	cancel()
	waitFor(t, "managed backend to be terminated on shutdown", func() bool { return !pidAlive(pid) })
}

func TestRecognizeBackend_CommandExitsBeforeHealthy_FailsStartup(t *testing.T) {
	t.Parallel()
	// A stub that is never healthy, and a command that exits immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		RootDir: t.TempDir(), StateDir: t.TempDir(),
		RecognizeProvider:     "serve",
		RecognizeServeURL:     srv.URL,
		RecognizeServeCommand: "true",
	}
	svc := mustNewIngestService(t, cfg, &fakeIngestStore{})

	err := svc.StartRecognizeBackend(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exited before becoming healthy") {
		t.Fatalf("expected exited-before-healthy error, got %v", err)
	}
}

func TestRecognizeBackend_NeverHealthy_TimesOutAndTerminatesChild(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		RootDir: t.TempDir(), StateDir: t.TempDir(),
		RecognizeProvider:     "serve",
		RecognizeServeURL:     srv.URL,
		RecognizeServeCommand: "sleep 60",
	}
	svc := mustNewIngestService(t, cfg, &fakeIngestStore{})
	svc.SetRecognizeHealthWait(600 * time.Millisecond)

	err := svc.StartRecognizeBackend(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("expected health-timeout error, got %v", err)
	}
	// The useless child must not be left behind.
	pid := svc.RecognizeBackendPID()
	if pid == 0 {
		t.Fatal("expected the child to have been launched")
	}
	waitFor(t, "unhealthy managed backend to be terminated", func() bool { return !pidAlive(pid) })
}

func TestRecognizeBackend_ConnectOnly_UnreachableIsWarningNotError(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		RootDir: t.TempDir(), StateDir: t.TempDir(),
		RecognizeProvider: "serve",
		// A port nothing listens on: connect-only mode warns and proceeds;
		// per-document ingest errors remain the hard signal.
		RecognizeServeURL: "http://127.0.0.1:1",
	}
	svc := mustNewIngestService(t, cfg, &fakeIngestStore{})
	if err := svc.StartRecognizeBackend(context.Background()); err != nil {
		t.Fatalf("connect-only unreachable backend must not fail startup, got %v", err)
	}
	if svc.RecognizeBackendPID() != 0 {
		t.Fatal("connect-only mode must not launch a process")
	}
}

func TestRecognizeBackend_NoOpWhenRecognitionOff(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "keep.txt"), "x")
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir()}, &fakeIngestStore{})
	if err := svc.StartRecognizeBackend(context.Background()); err != nil {
		t.Fatalf("recognition off must be a lifecycle no-op, got %v", err)
	}
}

func TestRecognizeConfig_ServeCommandRequiresServeProvider(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.RecognizeProvider = "off"
	cfg.RecognizeServeCommand = "dirstral-annotate serve --roster r.json"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "CONFIG_INVALID") || !strings.Contains(err.Error(), "recognize.serve_command") {
		t.Fatalf("expected CONFIG_INVALID for serve_command without serve provider, got %v", err)
	}

	cfg = config.Default()
	cfg.RecognizeProvider = "serve"
	cfg.RecognizeServeURL = "http://127.0.0.1:8765"
	cfg.RecognizeServeCommand = "dirstral-annotate serve --roster r.json"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid managed config rejected: %v", err)
	}
}

var _ model.Recognizer = (*fakeRecognizer)(nil)
