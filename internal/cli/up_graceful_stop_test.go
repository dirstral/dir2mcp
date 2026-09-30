package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestUp_GracefulCancelExitsZero pins the #434 precondition the launchd
// KeepAlive{SuccessfulExit=false} and systemd Restart=on-failure contracts
// depend on: a SIGTERM-triggered graceful stop of `up --foreground` (the
// supervised service target) must exit 0, not the interrupt code — otherwise
// a `dir2mcp down` (which SIGTERMs the daemon) would look like a crash and the
// supervisor would respawn it.
//
// The signal is modeled by cancelling the run context (exactly what
// signal.NotifyContext does on SIGTERM). The test asserts both halves of the
// chain: runUp marks the stop graceful, and resolveProcessExitCode then keeps
// the exit at 0.
func TestUp_GracefulCancelExitsZero(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	// Skip the network embed-preflight probe so the daemon reaches the serving
	// loop without valid live credentials (mirrors the tests/cli TestMain).
	t.Setenv("DIR2MCP_SKIP_EMBED_PROBE", "1")
	t.Chdir(tmp)

	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	app := NewAppWithIO(io.Discard, io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
	}()

	// Wait until the daemon is serving (connection.json published), then stop
	// it the way a signal would.
	connPath := connectionFilePath(stateDir)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(connPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-codeCh
			t.Fatal("daemon never became ready (connection.json not written)")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()

	var code int
	select {
	case code = <-codeCh:
	case <-time.After(15 * time.Second):
		t.Fatal("up did not exit after context cancellation")
	}

	if code != exitSuccess {
		t.Fatalf("graceful stop returned code %d from the serve loop, want %d", code, exitSuccess)
	}
	if !app.serverGracefulStop {
		t.Fatal("serverGracefulStop not set; a signal stop would be misread as an interrupt")
	}
	// The Run() wrapper maps a cancelled clean exit; with the graceful flag it
	// must stay 0 so the supervisor does not treat `down` as a crash.
	if got := resolveProcessExitCode(context.Canceled, code, app.serverGracefulStop); got != exitSuccess {
		t.Fatalf("graceful SIGTERM exit code = %d, want %d", got, exitSuccess)
	}
}

// blockingIngestor is a fake initial ingest. Run signals started, blocks until
// its context is done, and then returns the wrapped context error, the way a
// real scan does when a stop cancels it. Reindex is not used by `up`.
type blockingIngestor struct {
	started chan struct{}
}

// Run closes started, waits for ctx to end, and returns ctx.Err() wrapped.
func (b *blockingIngestor) Run(ctx context.Context) error {
	close(b.started)
	<-ctx.Done()
	return fmt.Errorf("scan corpus: %w", ctx.Err())
}

// Reindex returns nil. The up path never calls it.
func (b *blockingIngestor) Reindex(context.Context) error { return nil }

// failingIngestor is a fake initial ingest that fails at once with a real,
// non-context error.
type failingIngestor struct{}

// Run returns a genuine ingestion failure.
func (failingIngestor) Run(context.Context) error { return errors.New("disk on fire") }

// Reindex returns nil. The up path never calls it.
func (failingIngestor) Reindex(context.Context) error { return nil }

// setupGracefulUpEnv prepares a temp corpus with the env the up path needs to
// reach its serve loop offline. It returns the state directory.
func setupGracefulUpEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	t.Setenv("DIR2MCP_SKIP_EMBED_PROBE", "1")
	t.Chdir(tmp)
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	return stateDir
}

// TestUp_GracefulCancelDuringInitialIngestExitsZero pins #1091: a stop that
// cancels the context while the initial ingest still runs is a graceful stop.
// The ingest returns the context error, and `up` must exit 0 with the graceful
// flag set, not exit 3 (fatal ingestion error, SPEC §2.4). The fake ingestor
// blocks until the cancel, so the cancel always lands mid-ingest.
func TestUp_GracefulCancelDuringInitialIngestExitsZero(t *testing.T) {
	setupGracefulUpEnv(t)

	ing := &blockingIngestor{started: make(chan struct{})}
	app := NewAppWithIOAndHooks(io.Discard, io.Discard, RuntimeHooks{
		NewIngestor: func(config.Config, model.Store) (model.Ingestor, error) { return ing, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
	}()

	select {
	case <-ing.started:
	case code := <-codeCh:
		t.Fatalf("up exited with code %d before the initial ingest started", code)
	case <-time.After(15 * time.Second):
		cancel()
		<-codeCh
		t.Fatal("initial ingest never started")
	}

	cancel()

	var code int
	select {
	case code = <-codeCh:
	case <-time.After(15 * time.Second):
		t.Fatal("up did not exit after context cancellation")
	}
	if code != exitSuccess {
		t.Fatalf("cancel during the initial ingest returned code %d, want %d", code, exitSuccess)
	}
	if !app.serverGracefulStop {
		t.Fatal("serverGracefulStop not set after a cancel during the initial ingest")
	}
	if got := resolveProcessExitCode(context.Canceled, code, app.serverGracefulStop); got != exitSuccess {
		t.Fatalf("graceful SIGTERM exit code = %d, want %d", got, exitSuccess)
	}
}

// TestUp_IngestFailureWithoutCancelExitsThree pins the other half of #1091: a
// genuine ingestion failure while nothing cancelled the run still exits 3.
func TestUp_IngestFailureWithoutCancelExitsThree(t *testing.T) {
	setupGracefulUpEnv(t)

	app := NewAppWithIOAndHooks(io.Discard, io.Discard, RuntimeHooks{
		NewIngestor: func(config.Config, model.Store) (model.Ingestor, error) { return failingIngestor{}, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codeCh := make(chan int, 1)
	go func() {
		codeCh <- app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
	}()

	var code int
	select {
	case code = <-codeCh:
	case <-time.After(15 * time.Second):
		cancel()
		<-codeCh
		t.Fatal("up did not exit after the ingest failed")
	}
	if code != exitIngestionFatal {
		t.Fatalf("ingest failure without cancel returned code %d, want %d", code, exitIngestionFatal)
	}
}

// errOnlyCtx is a context whose Err reports a cancel but whose Done channel
// never fires. It forces runEventLoop to take the ingest-error branch, so the
// test does not depend on which ready select case the runtime picks.
type errOnlyCtx struct {
	context.Context
	err error
}

// Done returns nil, a channel that never becomes ready.
func (errOnlyCtx) Done() <-chan struct{} { return nil }

// Err returns the configured error.
func (c errOnlyCtx) Err() error { return c.err }

// runEventLoopWithIngestErr drives runEventLoop with one ingest error on the
// ingest channel and no other events. It returns the exit code.
func runEventLoopWithIngestErr(t *testing.T, runCtx context.Context, ingestErr error) int {
	t.Helper()
	ingestErrCh := make(chan error, 1)
	ingestErrCh <- ingestErr
	cfg := config.Config{StateDir: t.TempDir()}
	app := NewAppWithIO(io.Discard, io.Discard)
	return app.runEventLoop(runCtx, func() {}, &cfg, nil,
		appstate.NewIndexingState(appstate.ModeIncremental),
		newNDJSONEmitter(io.Discard, false),
		nil, ingestErrCh, nil, nil, io.Discard)
}

// TestRunEventLoop_IngestErrorClassification pins the event-loop rule of
// #1091 deterministically: a context error from the ingest after the run
// context ended is a graceful stop (exit 0). Any other ingest error, or a
// context error while the run context is still live, exits 3.
func TestRunEventLoop_IngestErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		ctxErr    error
		ingestErr error
		want      int
	}{
		{"canceled after stop", context.Canceled, fmt.Errorf("scan: %w", context.Canceled), exitSuccess},
		{"deadline after stop", context.Canceled, fmt.Errorf("scan: %w", context.DeadlineExceeded), exitSuccess},
		{"real error after stop", context.Canceled, errors.New("disk on fire"), exitIngestionFatal},
		{"real error while live", nil, errors.New("disk on fire"), exitIngestionFatal},
		{"context error while live", nil, context.Canceled, exitIngestionFatal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := errOnlyCtx{Context: context.Background(), err: tc.ctxErr}
			if got := runEventLoopWithIngestErr(t, ctx, tc.ingestErr); got != tc.want {
				t.Fatalf("runEventLoop exit = %d, want %d", got, tc.want)
			}
		})
	}
}
