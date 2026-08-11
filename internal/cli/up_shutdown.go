package cli

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Shutdown order for `dir2mcp up` (issues #688, #689).
//
// The deferred calls in runUp run in reverse order of registration. They
// implement these four steps:
//
//  1. STOP ACCEPTING NEW WORK. Cancel the run context. The MCP transport stops
//     accepting new connections, and the ingest, embed, corpus-writer and watch
//     workers see the cancellation.
//  2. DRAIN IN-FLIGHT WORK. Wait for the MCP transport, the initial ingest and
//     the other background workers to return. A handler that already started
//     keeps its index and its store until it finishes.
//  3. STOP PERSISTENCE. Cancel the periodic autosave, wait for it, then write
//     the final snapshot.
//  4. CLOSE RESOURCES. Close the indexes and the store.
//
// Every wait in steps 2 and 3 has a deadline. A shutdown that can hang forever
// is worse than one that gives up: the process stays alive, the supervisor
// kills it with SIGKILL, and the operator gets no message.

const (
	// mcpDrainGrace is how long the MCP transport waits for in-flight requests
	// after cancellation. It is the pre-existing http.Server.Shutdown window.
	mcpDrainGrace = 5 * time.Second

	// backgroundDrainBudget bounds step 2. It is mcpDrainGrace plus one second
	// of slack, because the transport is the slowest member of the drain group
	// and the other workers only need to observe the cancelled context.
	backgroundDrainBudget = mcpDrainGrace + time.Second

	// persistenceStopBudget bounds step 3: the wait for a running autosave plus
	// the final forced save. It is the pre-existing 3-second window.
	persistenceStopBudget = 3 * time.Second

	// serverShutdownBudget is the worst-case wall time of a graceful stop.
	//
	// It must stay below the grace period of every process manager that stops
	// the daemon, or the wait is theatre: the manager sends SIGKILL and the
	// drain never finishes. The three managers are
	//
	//   - `dir2mcp down`, which escalates to SIGKILL after
	//     daemonShutdownGrace. That constant is derived from this budget, so
	//     the two cannot drift apart.
	//   - launchd, which sends SIGKILL after the plist ExitTimeOut.
	//   - systemd, which sends SIGKILL after the unit TimeoutStopSec.
	//
	// The launchd and the systemd values are written into the generated unit
	// files as serviceStopTimeoutSeconds, so they do not depend on a platform
	// default that a distribution can lower.
	serverShutdownBudget = backgroundDrainBudget + persistenceStopBudget

	// serviceStopTimeoutSeconds is the stop grace period written into the
	// generated launchd plist and systemd unit. It must exceed
	// serverShutdownBudget with room for process teardown.
	serviceStopTimeoutSeconds = 20
)

// drainBackgroundWorkers performs steps 1 and 2 of the shutdown order.
//
// It cancels the run context and then waits for every registered background
// goroutine: the MCP transport, the initial ingest, the embed workers, the
// corpus writer and the watch worker. The wait stops after
// backgroundDrainBudget.
//
// On expiry the function warns and returns. It does not wait longer. The
// process is about to exit, and the goroutines that remain hold read-only work
// (a request handler, a scan) rather than a snapshot writer, so the risk is a
// truncated answer to one client, not a damaged file. The snapshot writer is
// fenced separately, by stopPersistenceWithLog and closeIndexesAfterPersistence.
func drainBackgroundWorkers(cancel context.CancelFunc, bgWG *sync.WaitGroup, stderr io.Writer) {
	cancel()

	done := make(chan struct{})
	go func() {
		bgWG.Wait()
		close(done)
	}()

	timer := time.NewTimer(backgroundDrainBudget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		writef(stderr, "shutdown warning: background work did not stop within %s; the server stops waiting for it\n",
			backgroundDrainBudget)
	}
}

// indexCloseFence decides whether runUp may close the vector indexes.
//
// The persistence manager owns the indexes while it saves them. If a save is
// still in flight when shutdown gives up waiting, a Close here races that
// writer and can leave a truncated snapshot on disk (issue #689). The fence
// then blocks the Close and the operating system reclaims the handles at
// process exit instead.
type indexCloseFence struct {
	blocked bool
	reason  error
}

// block marks the indexes as unsafe to close and records why.
func (f *indexCloseFence) block(reason error) {
	f.blocked = true
	f.reason = reason
}

// closeIndexesAfterPersistence performs the index half of step 4. It runs after
// stopPersistenceWithLog, which is what sets the fence.
func (f *indexCloseFence) closeIndexesAfterPersistence(stderr io.Writer, indexes ...model.Index) {
	if f.blocked {
		writef(stderr, "shutdown warning: index files stay open because a save is still in flight: %v\n", f.reason)
		return
	}
	for _, ix := range indexes {
		if ix == nil {
			continue
		}
		_ = ix.Close()
	}
}

// stopPersistenceWithLog performs step 3. It stops the periodic autosave, waits
// for it, and writes the final snapshot, all within persistenceStopBudget.
//
// When the wait expires the autosave still owns the indexes, so the function
// raises the fence and the caller keeps them open.
//
// stderr must be the synchronized sink, not the raw writer: a drain that gave
// up leaves background goroutines writing to that same sink (issue #419).
func stopPersistenceWithLog(persistence *index.PersistenceManager, fence *indexCloseFence, stderr io.Writer) {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), persistenceStopBudget)
	defer stopCancel()

	stopErr := persistence.StopAndSave(stopCtx)
	if stopErr == nil {
		return
	}
	if errors.Is(stopErr, index.ErrSaveInFlight) && fence != nil {
		fence.block(stopErr)
	}
	if errors.Is(stopErr, context.Canceled) {
		return
	}
	writef(stderr, "final index save warning: %v\n", stopErr)
}

// startTransportWorker launches the MCP transport on the shutdown drain group
// (issue #688).
//
// Membership of the group is what makes the shutdown order hold: runUp waits
// for Serve to return, and Serve returns only after http.Server.Shutdown drains
// the active handlers. Without it, runUp could close the index and the store
// under a running ask/search/open_file handler, and the client would get an
// internal error in place of its answer.
func startTransportWorker(runCtx context.Context, transport *mcp.SDKTransport, handler mcp.Handler, bgWG *sync.WaitGroup) <-chan error {
	transport.SetShutdownGrace(mcpDrainGrace)
	serverErrCh := make(chan error, 1)
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		serverErrCh <- transport.Serve(runCtx, handler)
	}()
	return serverErrCh
}
