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

	st, staging, code := a.prepareReindexStore(ctx, global, cfg)
	if code != exitSuccess {
		return code
	}

	ing, err := a.newIngestor(cfg, st)
	if err != nil {
		// The rebuild will not run, so restore the previous index we moved
		// aside rather than leaving the corpus without one (issue #418).
		a.closeStoreWithLog(st)
		staging.rollback(a.stderr)
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("initialize ingestor: %v", err))
		return exitConfigInvalid
	}

	progressCtx, stopProgress := context.WithCancel(ctx)
	defer stopProgress()
	progressDone := startReindexProgress(progressCtx, a.stderr, st, global, reindexProgressInterval)

	reindexErr := ing.Reindex(ctx)
	stopProgress()
	<-progressDone

	// Print the success summary while the store is still open (it sources
	// CorpusStats), then close the store so the rebuild's on-disk index is
	// final before we either keep it (commit) or restore the previous one
	// (rollback). Closing first prevents a persist-on-close from clobbering a
	// rolled-back index.
	if reindexErr == nil && !global.quiet && !global.jsonOutput {
		printReindexFinalSummary(a.stderr, ctx, st)
	}
	a.closeStoreWithLog(st)
	if reindexErr == nil {
		staging.commit()
	} else {
		// Any non-success — a real error, ctx cancellation from Ctrl-C, or an
		// unimplemented pipeline — means the rebuild is not durable, so put the
		// previous index back (issue #418).
		staging.rollback(a.stderr)
	}
	return a.finishReindex(global, reindexErr)
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
// stale index (issue #418). We reuse the same pid-file ownership check
// `up`/`down` use (classifyPIDFile) rather than reimplementing pid logic,
// and refuse with an actionable error instead of racing the daemon.
//
// Returns exitSuccess (reindex may proceed) when there is no pid file, the
// pid file is unreadable/malformed, it names a dead process, or it names a
// recycled pid (alive, but not our daemon) — none of which we can positively
// identify as a live daemon holding the index open.
func (a *App) refuseReindexIfDaemonRunning(global globalOptions, cfg config.Config) int {
	pid, ownership := classifyPIDFile(pidFilePath(cfg.StateDir))
	if ownership != pidLive {
		return exitSuccess
	}
	writeCLIError(a.stderr, global.jsonOutput, exitGeneric,
		fmt.Sprintf("dir2mcp is running for %s (pid %d); refusing to reindex under a live daemon", cfg.StateDir, pid),
		"Stop it with `dir2mcp down` first, then re-run `dir2mcp reindex`.",
	)
	return exitGeneric
}

// reindexBackupSuffix is appended to a stale index basename when a reindex
// moves it aside (rename, not delete) so the previous, working index can be
// restored if the rebuild is interrupted or fails (issue #418).
const reindexBackupSuffix = ".reindex-old"

// reindexStaging tracks the on-disk vector index files a reindex moved aside
// instead of deleting up front. commit discards those backups after a durable
// rebuild; rollback restores them over any partial rebuild so an interrupted
// reindex (Ctrl-C on a long OCR/embed run) leaves the corpus's previous index
// in place rather than empty/half-indexed (issue #418). A nil-safe zero value
// (no state dir, no backups) makes commit/rollback no-ops.
type reindexStaging struct {
	stateDir string
	backups  []string // basenames moved to name+reindexBackupSuffix
}

// backup renames stateDir/name aside to name+reindexBackupSuffix. A missing
// source is a no-op (nothing to preserve). Any pre-existing backup from an
// earlier interrupted run is cleared first so the rename can land.
func (s *reindexStaging) backup(name string) error {
	src := filepath.Join(s.stateDir, name)
	dst := src + reindexBackupSuffix
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear stale backup %s: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("move aside %s: %w", src, err)
	}
	s.backups = append(s.backups, name)
	return nil
}

// commit deletes the moved-aside backups after a durable rebuild.
func (s *reindexStaging) commit() {
	for _, name := range s.backups {
		_ = os.Remove(filepath.Join(s.stateDir, name) + reindexBackupSuffix)
	}
	s.backups = nil
}

// rollback restores the moved-aside index files over any partially-written
// rebuild so an interrupted or failed reindex leaves the previous, working
// index intact. Best-effort: warnings are logged but never fail teardown.
func (s *reindexStaging) rollback(stderr io.Writer) {
	for _, name := range s.backups {
		dst := filepath.Join(s.stateDir, name)
		src := dst + reindexBackupSuffix
		if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			writef(stderr, "warning: reindex rollback: remove partial %s: %v\n", dst, err)
		}
		if err := os.Rename(src, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			writef(stderr, "warning: reindex rollback: restore %s: %v\n", dst, err)
		}
	}
	s.backups = nil
}

// prepareReindexStore creates the state directory, opens the store, clears
// prior content hashes, and moves stale vector index files aside (rename, not
// delete) so the upcoming Reindex starts from a clean slate WITHOUT destroying
// the previous index up front. Returns the initialised store and the staging
// handle (for commit/rollback) on success; on any failure it restores anything
// already moved aside, writes a CLI error, and returns the exit code.
func (a *App) prepareReindexStore(ctx context.Context, global globalOptions, cfg config.Config) (model.Store, *reindexStaging, int) {
	staging := &reindexStaging{stateDir: cfg.StateDir}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitRootInaccessible, fmt.Sprintf("create state dir: %v", err))
		return nil, nil, exitRootInaccessible
	}
	st := a.storeForConfig(cfg)
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		_ = st.Close()
		writeCLIError(a.stderr, global.jsonOutput, exitIndexLoadFailure, fmt.Sprintf("initialize metadata store: %v", err))
		return nil, nil, exitIndexLoadFailure
	}
	if code := clearContentHashesIfSupported(ctx, st, a.stderr, global.jsonOutput); code != exitSuccess {
		_ = st.Close()
		return nil, nil, code
	}
	// Move stale vector index files aside so a snapshot of any shape cannot
	// survive a reindex — the current HNSW v2 files, the legacy pre-#247
	// bare-map files, and (for the disk backend) its segment + identity sidecar
	// (issue #246) — while keeping them recoverable until the rebuild is durable
	// (issue #418).
	for _, name := range index.StaleIndexFiles(cfg.IndexBackend) {
		if err := staging.backup(name); err != nil {
			staging.rollback(a.stderr)
			_ = st.Close()
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("stage index file %s: %v", name, err))
			return nil, nil, exitGeneric
		}
	}
	return st, staging, exitSuccess
}

// finishReindex maps the ingestor's return error to the user-facing exit
// code and message. The success summary and the store's lifetime are handled
// by the caller (runReindex) so the store can be closed before the
// commit/rollback of the staged index.
func (a *App) finishReindex(global globalOptions, err error) int {
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
