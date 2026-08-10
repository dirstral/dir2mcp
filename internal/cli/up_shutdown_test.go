package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// closeCountingIndex is a model.Index that only records its Close calls.
type closeCountingIndex struct {
	closes int
}

func (i *closeCountingIndex) Upsert(context.Context, []float32, model.IndexPayload) error { return nil }
func (i *closeCountingIndex) Delete(context.Context, []uint64) error                      { return nil }
func (i *closeCountingIndex) Search(context.Context, []float32, int, model.Filter) ([]model.IndexHit, error) {
	return nil, nil
}
func (i *closeCountingIndex) Identity(context.Context) (string, error) { return "", nil }
func (i *closeCountingIndex) Reset(context.Context, string) error      { return nil }
func (i *closeCountingIndex) Close() error                             { i.closes++; return nil }

// TestShutdownBudgetFitsEveryProcessManager pins the reason the drain is real
// work and not theatre (issue #688).
//
// A wait longer than the grace period of the process manager never finishes:
// the manager sends SIGKILL first. `dir2mcp down` is the tightest of the three
// managers, so its grace period must exceed the budget, and the generated
// launchd and systemd stop timeouts must exceed it too.
func TestShutdownBudgetFitsEveryProcessManager(t *testing.T) {
	if backgroundDrainBudget <= mcpDrainGrace {
		t.Fatalf("the drain budget (%s) must exceed the transport grace (%s), or the transport can never finish",
			backgroundDrainBudget, mcpDrainGrace)
	}
	if serverShutdownBudget != backgroundDrainBudget+persistenceStopBudget {
		t.Fatalf("the shutdown budget (%s) must cover both phases (%s + %s)",
			serverShutdownBudget, backgroundDrainBudget, persistenceStopBudget)
	}
	if daemonShutdownGrace <= serverShutdownBudget {
		t.Fatalf("`down` escalates to SIGKILL after %s, which is not more than the %s the server needs to stop; the drain would be cut short",
			daemonShutdownGrace, serverShutdownBudget)
	}
	serviceTimeout := time.Duration(serviceStopTimeoutSeconds) * time.Second
	if serviceTimeout <= daemonShutdownGrace {
		t.Fatalf("the service stop timeout (%s) must exceed the `down` grace period (%s)",
			serviceTimeout, daemonShutdownGrace)
	}
}

// TestGeneratedServiceUnitsStateTheStopTimeout proves the launchd plist and the
// systemd unit carry an explicit stop timeout. Both platforms have a default,
// but a default can be lowered below the shutdown budget, which would make the
// drain theatre on that host.
func TestGeneratedServiceUnitsStateTheStopTimeout(t *testing.T) {
	spec := serviceSpec{
		Label:      "com.dirstral.test",
		BinaryPath: "/usr/local/bin/dir2mcp",
		WorkingDir: "/corpus",
		LogPath:    "/corpus/.dir2mcp/server.log",
	}

	plist := renderLaunchdPlist(spec)
	wantPlist := fmt.Sprintf("<key>ExitTimeOut</key>\n  <integer>%d</integer>", serviceStopTimeoutSeconds)
	if !strings.Contains(plist, wantPlist) {
		t.Fatalf("launchd plist missing %q:\n%s", wantPlist, plist)
	}

	unit := renderSystemdUnit(spec)
	wantUnit := fmt.Sprintf("TimeoutStopSec=%d", serviceStopTimeoutSeconds)
	if !strings.Contains(unit, wantUnit) {
		t.Fatalf("systemd unit missing %q:\n%s", wantUnit, unit)
	}
}

// TestDrainWaitsForInFlightMCPRequest proves the transport is part of the
// shutdown drain (issue #688): a request that is already inside a handler keeps
// the drain open, so runUp cannot reach the index and store Close calls while a
// client is still waiting for its answer.
//
// The request goes to a non-MCP path so the test does not need the MCP
// handshake. The drain mechanism is the same either way: both paths run on the
// one http.Server whose Shutdown waits for active handlers.
func TestDrainWaitsForInFlightMCPRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	cfg := config.Config{MCPPath: "/mcp"}
	transport := mcp.NewSDKTransport(mcp.NewServer(cfg, nil), ln, "", "")

	inHandler := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inHandler)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	var bgWG sync.WaitGroup
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverErrCh := startTransportWorker(runCtx, transport, handler, &bgWG)

	respCh := make(chan error, 1)
	go func() {
		resp, reqErr := http.Get("http://" + ln.Addr().String() + "/slow")
		if reqErr != nil {
			respCh <- reqErr
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		respCh <- nil
	}()

	select {
	case <-inHandler:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("the request never reached the handler")
	}

	drained := make(chan struct{})
	go func() {
		drainBackgroundWorkers(cancel, &bgWG, io.Discard)
		close(drained)
	}()

	select {
	case <-drained:
		close(release)
		t.Fatal("the drain returned while an MCP request was still in the handler; runUp would close the index and the store underneath it")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the drain did not return after the handler finished")
	}

	if reqErr := <-respCh; reqErr != nil {
		t.Fatalf("the in-flight request was cut short instead of served: %v", reqErr)
	}
	if serveErr := <-serverErrCh; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		t.Fatalf("transport Serve: %v", serveErr)
	}
}

// TestDrainStopsWaitingAtTheBudget proves the drain gives up instead of hanging.
// A shutdown that can wait forever is worse than one that stops: the process
// stays alive until the supervisor sends SIGKILL, and the operator gets no
// message about it.
func TestDrainStopsWaitingAtTheBudget(t *testing.T) {
	var bgWG sync.WaitGroup
	stuck := make(chan struct{})
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		<-stuck
	}()
	defer close(stuck)

	_, cancel := context.WithCancel(context.Background())
	var stderr bytes.Buffer

	done := make(chan struct{})
	go func() {
		drainBackgroundWorkers(cancel, &bgWG, &stderr)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(backgroundDrainBudget + 10*time.Second):
		t.Fatal("the drain never gave up; a graceful stop can hang the process")
	}
	if got := stderr.String(); !strings.Contains(got, "background work did not stop") {
		t.Fatalf("the forced end of the drain was silent; stderr=%q", got)
	}
}

// TestIndexCloseFenceBlocksCloseWhileSaving proves the CLI honours the
// persistence ownership contract (issue #689): when a save is still in flight,
// the indexes stay open and the operator is told why.
func TestIndexCloseFenceBlocksCloseWhileSaving(t *testing.T) {
	ix := &closeCountingIndex{}

	open := &indexCloseFence{}
	var openLog bytes.Buffer
	open.closeIndexesAfterPersistence(&openLog, ix)
	if ix.closes != 1 {
		t.Fatalf("a quiescent shutdown must close the index; closes=%d", ix.closes)
	}

	blocked := &indexCloseFence{}
	blocked.block(errors.New("save still running"))
	var blockedLog bytes.Buffer
	blocked.closeIndexesAfterPersistence(&blockedLog, ix)
	if ix.closes != 1 {
		t.Fatalf("the index was closed under a running save; closes=%d", ix.closes)
	}
	if got := blockedLog.String(); !strings.Contains(got, "save is still in flight") {
		t.Fatalf("the skipped close was silent; stderr=%q", got)
	}
}
