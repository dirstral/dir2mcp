package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// reindexProgressInterval is how often the reindex command prints a
// status line while the ingestor is running. Picked small enough that
// users see motion on slow-OCR corpora but large enough that the
// stderr stream stays readable.
const reindexProgressInterval = 5 * time.Second

func (a *App) runReindex(ctx context.Context, global globalOptions, args []string) int {
	if len(args) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("reindex command does not accept arguments: %s", strings.Join(args, " ")))
		return exitConfigInvalid
	}

	cfg, code := a.loadReindexConfig(global)
	if code != exitSuccess {
		return code
	}
	// Refuse before prompting/destroying anything: a reindex under a live
	// daemon corrupts the shared index/state (issue #418).
	if code := a.refuseReindexIfDaemonRunning(global, cfg); code != exitSuccess {
		return code
	}

	if !a.confirmDestructive(global, "Re-index all documents?", "Discards the current index and rebuilds it from scratch (may re-run OCR/embeddings).") {
		writeln(a.stderr, "reindex aborted")
		return exitSuccess
	}

	st, code := a.prepareReindexStore(ctx, global, cfg)
	if code != exitSuccess {
		return code
	}
	defer a.closeStoreWithLog(st)

	ing, err := a.newIngestor(cfg, st)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("initialize ingestor: %v", err))
		return exitConfigInvalid
	}

	progressCtx, stopProgress := context.WithCancel(ctx)
	defer stopProgress()
	progressDone := startReindexProgress(progressCtx, a.stderr, st, global, reindexProgressInterval)

	err = ing.Reindex(ctx)
	stopProgress()
	<-progressDone
	return a.finishReindex(ctx, global, st, err)
}

// loadReindexConfig pulls the layered config and normalises the state
// directory. Extracted from runReindex so the parent function stays
// under the cyclomatic-complexity budget.
func (a *App) loadReindexConfig(global globalOptions) (config.Config, int) {
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return cfg, exitConfigInvalid
	}
	baseDir := strings.TrimSpace(cfg.StateDir)
	if baseDir == "" {
		baseDir = ".dir2mcp"
	}
	cfg.StateDir = baseDir
	return cfg, exitSuccess
}

// refuseReindexIfDaemonRunning blocks a reindex when a live `dir2mcp up`
// daemon owns the same state dir. A reindex clears content hashes and
// unlinks the on-disk vector index (see prepareReindexStore); doing that
// under a running daemon means two concurrent sqlite writers and index
// files removed out from under a process that still holds them open —
// index/state corruption, with the daemon continuing to serve a freed,
// stale index (issue #418). We reuse the exact pid-file + liveness check
// `up` uses to detect an already-running instance (readPIDFile +
// processIsAlive) rather than reimplementing pid logic, and refuse with
// an actionable error instead of racing the daemon.
//
// Returns exitSuccess (reindex may proceed) when there is no pid file,
// the pid file is unreadable/malformed, or it names a dead process — none
// of which we can positively identify as a live daemon.
func (a *App) refuseReindexIfDaemonRunning(global globalOptions, cfg config.Config) int {
	pid, err := readPIDFile(pidFilePath(cfg.StateDir))
	if err != nil {
		// No pid file (the common case) or a malformed one: nothing we can
		// confirm is a live daemon, so let the reindex proceed. `down`
		// handles cleanup of a malformed pid file.
		return exitSuccess
	}
	if !processIsAlive(pid) {
		// Stale pid file from a daemon that crashed/exited without cleanup —
		// safe to reindex.
		return exitSuccess
	}
	writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
		fmt.Sprintf("dir2mcp is running for %s (pid %d); refusing to reindex under a live daemon", cfg.StateDir, pid),
		"Stop it with `dir2mcp down` first, then re-run `dir2mcp reindex`.",
	)
	return exitGeneric
}

// prepareReindexStore creates the state directory, opens the store,
// clears prior content hashes, and removes stale vector index files
// so the upcoming Reindex starts from a clean slate. Returns the
// initialised store on success; on any failure it writes a CLI error
// and returns the appropriate exit code.
func (a *App) prepareReindexStore(ctx context.Context, global globalOptions, cfg config.Config) (model.Store, int) {
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitRootInaccessible, fmt.Sprintf("create state dir: %v", err))
		return nil, exitRootInaccessible
	}
	st := a.storeForConfig(cfg)
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		_ = st.Close()
		writeCLIError(a.stderr, global.jsonOutput, exitIndexLoadFailure, fmt.Sprintf("initialize metadata store: %v", err))
		return nil, exitIndexLoadFailure
	}
	if code := clearContentHashesIfSupported(ctx, st, a.stderr, global.jsonOutput); code != exitSuccess {
		_ = st.Close()
		return nil, code
	}
	// Remove stale vector index files so a snapshot of any shape cannot survive
	// a reindex: the current HNSW v2 files, the legacy pre-#247 bare-map files,
	// and — when the disk backend is selected — its segment + identity sidecar
	// (issue #246).
	staleNames := index.StaleIndexFiles(cfg.IndexBackend)
	for _, name := range staleNames {
		indexPath := filepath.Join(cfg.StateDir, name)
		if err := os.Remove(indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = st.Close()
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("remove stale index file %s: %v", indexPath, err))
			return nil, exitGeneric
		}
	}
	return st, exitSuccess
}

// finishReindex handles the post-Reindex flow: dispatch on the
// ingestor's return error, then (on success) emit the final progress
// summary so users see the final counters without having to re-query
// status.
func (a *App) finishReindex(ctx context.Context, global globalOptions, st model.Store, err error) int {
	if errors.Is(err, model.ErrNotImplemented) {
		if !global.quiet {
			writeln(a.stdout, "reindex is not available yet: ingestion pipeline not implemented")
		}
		return exitSuccess
	}
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("reindex failed: %v", err))
		return exitGeneric
	}
	if !global.quiet && !global.jsonOutput {
		printReindexFinalSummary(a.stderr, ctx, st)
	}
	return exitSuccess
}

// startReindexProgress spawns a goroutine that prints periodic
// progress lines to out while the ingestor is running, then returns a
// channel that closes when the goroutine exits. The poller stops as
// soon as ctx is cancelled (the caller does this once Reindex
// returns). Silent under --quiet and under --json so machine-readable
// callers and quiet-script users see a clean output stream.
func startReindexProgress(ctx context.Context, out io.Writer, st model.Store, global globalOptions, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if global.quiet || global.jsonOutput {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		// One immediate tick so users see a "starting" line within
		// a second instead of waiting a full interval before the
		// first progress update.
		printReindexProgressLine(out, ctx, st)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				printReindexProgressLine(out, ctx, st)
			}
		}
	}()
	return done
}

// printReindexProgressLine emits one [reindex] status line to out.
// CorpusStats is only available from the SQLite-backed store; for any
// other backend the function is silent so the reindex command stays
// usable with future store implementations.
func printReindexProgressLine(out io.Writer, ctx context.Context, st model.Store) {
	stats, ok := corpusStatsForReindex(ctx, st)
	if !ok {
		return
	}
	_, _ = fmt.Fprintf(out, "[reindex] scanned=%d indexed=%d skipped=%d errors=%d chunks=%d embedded=%d\n",
		stats.Scanned, stats.Indexed, stats.Skipped, stats.Errors, stats.ChunksTotal, stats.EmbeddedOK)
}

// printReindexFinalSummary writes a one-line completion summary so the
// user knows the run finished and what the final counts are. Same
// CorpusStats source as the progress poller; silent when stats are
// unavailable.
func printReindexFinalSummary(out io.Writer, ctx context.Context, st model.Store) {
	stats, ok := corpusStatsForReindex(ctx, st)
	if !ok {
		return
	}
	_, _ = fmt.Fprintf(out, "[reindex] done: scanned=%d indexed=%d skipped=%d errors=%d chunks=%d embedded=%d\n",
		stats.Scanned, stats.Indexed, stats.Skipped, stats.Errors, stats.ChunksTotal, stats.EmbeddedOK)
}

// corpusStatsForReindex pulls CorpusStats from the SQLite-backed
// store, swallowing the type-assertion miss and any transient query
// error so the progress poller never crashes a healthy reindex run.
// Returns ok=false when stats are unavailable (caller should skip
// printing rather than emit a misleading zero-line).
func corpusStatsForReindex(ctx context.Context, st model.Store) (model.CorpusStats, bool) {
	type corpusStatser interface {
		CorpusStats(ctx context.Context) (model.CorpusStats, error)
	}
	cs, ok := st.(corpusStatser)
	if !ok {
		return model.CorpusStats{}, false
	}
	stats, err := cs.CorpusStats(ctx)
	if err != nil {
		return model.CorpusStats{}, false
	}
	return stats, true
}
