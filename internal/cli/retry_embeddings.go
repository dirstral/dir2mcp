package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// reindexOptions holds the parsed `dir2mcp reindex` flags.
type reindexOptions struct {
	// embeddingsOnly selects the embed-step retry instead of a full rebuild:
	// failed chunks are moved back to pending WITHOUT re-running extraction.
	embeddingsOnly bool
	// errorCategories narrows the retry to specific store.ErrorCategory values.
	// Empty selects the default retryable set (store.RequeueableErrorCategories).
	errorCategories []string
}

// parseReindexOptions parses the reindex flag set. Positional arguments are
// still rejected by the caller with the historical message, so scripts that
// depend on that contract are unaffected.
func parseReindexOptions(args []string) (reindexOptions, []string, error) {
	opts := reindexOptions{}
	var categories string
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.embeddingsOnly, "embeddings-only", false,
		"retry chunks that failed to embed (reset them to pending) without re-running extraction")
	fs.StringVar(&categories, "error-category", "",
		"comma-separated error categories to retry (default: "+strings.Join(store.RequeueableErrorCategories(), ",")+")")
	if err := fs.Parse(args); err != nil {
		return reindexOptions{}, nil, err
	}

	parsed, err := parseErrorCategoryList(categories)
	if err != nil {
		return reindexOptions{}, nil, err
	}
	if len(parsed) > 0 && !opts.embeddingsOnly {
		// A full rebuild re-creates every chunk, so it has no notion of retrying
		// a subset by failure category. Silently ignoring the filter would let an
		// operator believe they had scoped a run that in fact reprocesses the
		// whole corpus.
		return reindexOptions{}, nil, errors.New("--error-category is only valid with --embeddings-only")
	}
	opts.errorCategories = parsed
	return opts, fs.Args(), nil
}

// parseErrorCategoryList splits and validates a comma-separated
// --error-category value. An unrecognized name is rejected with the real
// vocabulary rather than accepted into a retry that would match zero rows and
// report a confusing "0 chunks requeued".
func parseErrorCategoryList(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	out := []string{}
	for _, field := range strings.Split(raw, ",") {
		name := strings.TrimSpace(field)
		if name == "" {
			continue
		}
		if !store.IsKnownErrorCategory(name) {
			return nil, fmt.Errorf("unknown error category %q (known: %s)", name, strings.Join(store.KnownErrorCategories(), ", "))
		}
		out = append(out, strings.ToLower(name))
	}
	if len(out) == 0 {
		return nil, errors.New("--error-category was empty")
	}
	return out, nil
}

// failedChunkRequeuer is the optional store capability the embed-step retry
// needs: move chunks parked in embedding_status='error' back to pending. Kept
// as a local interface (the contentHashBackuper pattern) so a non-sqlite store
// degrades to a clear "unsupported" error instead of a nil-pointer panic.
type failedChunkRequeuer interface {
	RequeueFailedChunks(ctx context.Context, categories []string) (int64, error)
}

// runReindexEmbeddingsOnly retries the embed step for chunks that a provider
// rejected, without re-running extraction (issue #783).
//
// This is the recovery path for a credential- or provider-shaped failure. A key
// that is valid at startup and then revoked, rotated, billing-suspended, or
// rate-limited into a non-retryable class strands every chunk it touches in
// embedding_status='error', and nothing moved those chunks back: the only
// statement that resets a chunk to pending is the ingest-time chunk upsert. So
// the only supported recovery was re-ingesting the affected documents, which
// redoes extraction (the expensive half: OCR, transcription, a recognition
// cascade over a video) purely to redo the embed step (seconds). The §2.5
// startup probe added for issue #399 prevents the failure from beginning; it
// cannot help a daemon that is already up when the credential dies.
//
// Deliberately NOT guarded by refuseReindexIfDaemonRunning, unlike a full
// rebuild. That guard exists because a rebuild clears content hashes and
// unlinks index files under a process that holds them open (#418). This path
// does neither: it is one UPDATE over the chunks table, which WAL plus the
// store's busy_timeout already serialize, and a chunk in 'error' never had a
// vector written for it, so there is nothing in the live index to reconcile.
// Running it under a live daemon is in fact the fastest recovery there is: the
// daemon's embed worker polls NextPending on a ticker and drains the requeued
// chunks without a restart.
//
// It also does not prompt: it is not destructive (a chunk that fails again
// simply returns to 'error'), and prompting would break exactly the scripted,
// non-interactive recovery this exists to enable.
func (a *App) runReindexEmbeddingsOnly(ctx context.Context, global globalOptions, opts reindexOptions) int {
	cfg, code := a.loadReindexConfig(global)
	if code != exitSuccess {
		return code
	}

	st := a.storeForConfig(cfg)
	defer a.closeStoreWithLog(st)
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writeStoreInitError(a.stderr, global.jsonOutput, exitIndexLoadFailure, err, fmt.Sprintf("initialize metadata store: %v", err))
		return exitIndexLoadFailure
	}
	requeuer, ok := interface{}(st).(failedChunkRequeuer)
	if !ok {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, "configured store does not support retrying failed embeddings")
		return exitGeneric
	}

	categories := opts.errorCategories
	if len(categories) == 0 {
		categories = store.RequeueableErrorCategories()
	}
	// Read the failure breakdown BEFORE the retry: afterwards the requeued rows
	// are gone from it, and the per-category counts are what make the result
	// legible ("346 auth", not just "346").
	failed := failedCategoryCounts(ctx, st)

	requeued, err := requeuer.RequeueFailedChunks(ctx, categories)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("retry failed embeddings: %v", err))
		return exitGeneric
	}

	return a.reportEmbeddingsOnlyRetry(global, cfg, categories, failed, requeued)
}

// failedCategoryCounts returns the currently-failed chunk counts by error
// category, or nil when the store cannot supply them. Best-effort: the retry
// itself does not depend on this, it only makes the report specific.
func failedCategoryCounts(ctx context.Context, st model.Store) map[string]int64 {
	stats, ok := corpusStatsForReindex(ctx, st)
	if !ok || stats.FailureSummary == nil {
		return nil
	}
	return stats.FailureSummary.Categories
}

// reportEmbeddingsOnlyRetry renders the retry result in the CLI's JSON and
// human styles, including what was left alone and what happens next.
func (a *App) reportEmbeddingsOnlyRetry(global globalOptions, cfg config.Config, categories []string, failed map[string]int64, requeued int64) int {
	daemonRunning := daemonIsLive(cfg.StateDir)
	retried, terminal := splitFailedCounts(failed, categories)

	if global.jsonOutput {
		payload := map[string]interface{}{
			"state_dir":       cfg.StateDir,
			"embeddings_only": true,
			"categories":      categories,
			"requeued":        requeued,
			"daemon_running":  daemonRunning,
		}
		if len(retried) > 0 {
			// Observed immediately BEFORE the retry, so it explains the total
			// rather than restating it; the two can differ by chunks a live
			// daemon failed in between.
			payload["matched_by_category"] = retried
		}
		if len(terminal) > 0 {
			payload["not_retried_by_category"] = terminal
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode reindex json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}
	if global.quiet {
		return exitSuccess
	}

	writef(a.stderr, "[reindex] embeddings-only: requeued %d chunk(s) for embedding [categories=%s]\n",
		requeued, strings.Join(categories, ","))
	if breakdown := formatCategoryCounts(terminal); breakdown != "" {
		writef(a.stderr, "[reindex] not retried (terminal; re-ingest to change the input): %s\n", breakdown)
	}
	switch {
	case requeued == 0:
		writeln(a.stderr, "[reindex] nothing to retry: no failed chunks in those categories")
	case daemonRunning:
		writeln(a.stderr, "[reindex] a daemon is running here; its embed worker will pick these up on its next cycle")
	default:
		writeln(a.stderr, "[reindex] run `dir2mcp up` to embed them")
	}
	return exitSuccess
}

// splitFailedCounts partitions the currently-failed counts into the categories
// this run retried and those it left alone, so the report can say why the
// untouched ones stayed put instead of silently under-reporting.
func splitFailedCounts(failed map[string]int64, categories []string) (retried, terminal map[string]int64) {
	if len(failed) == 0 {
		return nil, nil
	}
	selected := make(map[string]struct{}, len(categories))
	for _, c := range categories {
		selected[c] = struct{}{}
	}
	retried = map[string]int64{}
	terminal = map[string]int64{}
	for category, n := range failed {
		if _, ok := selected[category]; ok {
			retried[category] = n
			continue
		}
		terminal[category] = n
	}
	return retried, terminal
}

// formatCategoryCounts renders a stable "category=N category=N" line, or ""
// when there is nothing to report.
func formatCategoryCounts(counts map[string]int64) string {
	if len(counts) == 0 {
		return ""
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return strings.Join(parts, " ")
}
