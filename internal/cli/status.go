package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

func (a *App) runStatus(ctx context.Context, global globalOptions, args []string) int {
	if len(args) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("status command does not accept arguments: %s", strings.Join(args, " ")))
		return exitConfigInvalid
	}

	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return exitConfigInvalid
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(".", ".dir2mcp")
	}

	cached, cacheErr := readCorpusSnapshot(filepath.Join(cfg.StateDir, "corpus.json"))
	metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
	_, metaErr := os.Stat(metaPath)
	if cacheErr != nil && metaErr != nil {
		if errors.Is(metaErr, os.ErrNotExist) {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("no state found in %s; run: dir2mcp up", cfg.StateDir))
			return exitGeneric
		}
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read state: %v", metaErr))
		return exitGeneric
	}

	snapshot := cached
	source := "corpus_json"
	if metaErr == nil {
		// Read the store whenever it is there, even with a readable corpus.json
		// in hand. corpus.json is a cache the daemon stops refreshing the moment
		// indexing reports stopped, while the embed worker keeps draining the
		// queue for minutes afterwards; the MCP dir2mcp_stats tool queries the
		// store on every call. Trusting the file is exactly how #1005 happened:
		// `status` printed "embedded=0 pending=487" over a corpus the store,
		// the stats tool and `ask` all agreed was fully embedded.
		computed, code, ok := a.computeStatusSnapshot(ctx, global, cfg, cacheErr == nil)
		if ok {
			if cacheErr == nil {
				carryRunLiveness(&computed, cached)
			}
			snapshot = computed
			source = "computed"
		} else if cacheErr != nil {
			// Nothing cached to fall back on, so the store failure is fatal.
			// computeStatusSnapshot already reported why.
			return code
		}
		// Otherwise the cached snapshot stands, and the downgrade is already on
		// stderr, so the operator knows the counters may have moved on.
	}

	// Reconcile a daemon-written "running" snapshot against process liveness.
	// corpus.json is written by the daemon, so a daemon that recorded
	// running=true and then crashed would otherwise leave status reporting
	// indexing as "running" forever. If no live daemon owns this state dir, the
	// snapshot is stale: report indexing as stopped instead (#418).
	staleRunning := false
	if snapshot.Indexing.Running && !daemonIsLive(cfg.StateDir) {
		snapshot.Indexing.Running = false
		staleRunning = true
	}

	// Load recent per-document ingest failures (rel_path + reason) so status can
	// show *which* files failed and why, not just errors=N (#414 part 2). This is
	// a live store read, best-effort: any miss leaves the block unrendered.
	recentFailures := a.loadRecentFailuresForStatus(ctx, cfg, statusRecentFailuresLimit)

	return a.renderStatusOutput(global, cfg.StateDir, snapshot, source, staleRunning, recentFailures)
}

// computeStatusSnapshot builds a corpus snapshot straight from the metadata
// store, which is the source of truth the dir2mcp_stats MCP tool reads too
// (both assemble their counter block with model.ResolveIndexingCounters, so the
// two surfaces cannot drift: #1005).
//
// haveCached says whether the caller holds a usable corpus.json. Without one a
// failure here is fatal, so the reason is written to the caller's error surface
// and the returned exit code is the one to exit with. With one the failure only
// costs freshness, so it is noted on stderr and the caller keeps the cache.
func (a *App) computeStatusSnapshot(ctx context.Context, global globalOptions, cfg config.Config, haveCached bool) (corpusSnapshot, int, bool) {
	snapshot, storeInitFailed, err := a.storeStatusSnapshot(ctx, cfg, a.stderr)
	if err == nil {
		return snapshot, exitSuccess, true
	}
	if haveCached {
		// Say it out loud. A silent downgrade hands the operator counters that
		// look current and are not, which is the #1005 failure in miniature.
		writef(a.stderr, "status: reporting cached counters; %v\n", err)
		return corpusSnapshot{}, exitSuccess, false
	}
	if storeInitFailed {
		writeStoreInitError(a.stderr, global.jsonOutput, exitIndexLoadFailure, err, err.Error())
		return corpusSnapshot{}, exitIndexLoadFailure, false
	}
	writeCLIError(a.stderr, global.jsonOutput, exitGeneric, err.Error())
	return corpusSnapshot{}, exitGeneric, false
}

// carryRunLiveness copies the run-liveness fields from the daemon's corpus.json
// cache onto a store-derived snapshot. mode, running and watch_overflows
// describe the daemon's CURRENT run, and no store query can answer them, so a
// refreshed snapshot that dropped them would report a live indexing run as
// stopped: #418 in reverse. The counters are NOT copied, which is the whole
// point of refreshing them (#1005).
func carryRunLiveness(refreshed *corpusSnapshot, cached corpusSnapshot) {
	refreshed.Indexing.Mode = cached.Indexing.Mode
	refreshed.Indexing.Running = cached.Indexing.Running
	refreshed.Indexing.WatchOverflows = cached.Indexing.WatchOverflows
}

// storeStatusSnapshot builds a corpus snapshot from the metadata store. The
// bool reports whether the failure was the store handshake itself, which
// carries its own operator hint (writeStoreInitError).
//
// warn takes the snapshot builder's advisory output (unexpected document
// statuses). `status` sends it to stderr; the support bundle discards it,
// because a bundle must not print to the operator's terminal.
func (a *App) storeStatusSnapshot(ctx context.Context, cfg config.Config, warn io.Writer) (corpusSnapshot, bool, error) {
	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		return corpusSnapshot{}, true, fmt.Errorf("initialize metadata store: %w", err)
	}
	// status --json must emit a single JSON object, not an NDJSON stream.
	// Keep the emitter disabled so computed-snapshot warnings go to warn.
	emitter := newNDJSONEmitter(a.stdout, false)
	snapshot, err := buildCorpusSnapshot(ctx, st, nil, warn, emitter)
	if err != nil {
		return corpusSnapshot{}, false, fmt.Errorf("build status snapshot: %w", err)
	}
	return snapshot, false, nil
}

// statusRecentFailuresLimit bounds how many recent failures `status` renders —
// enough to triage without flooding the terminal on a badly-broken corpus.
const statusRecentFailuresLimit = 10

// loadRecentFailuresForStatus reads the most recent status='error' documents
// from the store, best-effort. It opens a short-lived read handle (WAL allows a
// concurrent reader alongside a live daemon) and returns nil on any miss — an
// absent store, an unsupported backend, or a transient query error — so status
// never fails because the diagnostic side query did.
func (a *App) loadRecentFailuresForStatus(ctx context.Context, cfg config.Config, limit int) []model.Document {
	metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
	if _, err := os.Stat(metaPath); err != nil {
		return nil
	}
	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		return nil
	}
	type recentFailuresLister interface {
		RecentFailures(ctx context.Context, limit int) ([]model.Document, error)
	}
	rf, ok := st.(recentFailuresLister)
	if !ok {
		return nil
	}
	docs, err := rf.RecentFailures(ctx, limit)
	if err != nil {
		return nil
	}
	return docs
}

// daemonIsLive reports whether a live daemon that is actually OURS currently
// owns the given state dir. It consults the pid file written by `dir2mcp up`
// and verifies both liveness and identity (classifyPIDFile). Returns false
// when the pid file is absent, malformed, names a dead process, or names a
// recycled pid (alive, but an unrelated process the OS reassigned after a
// crash) — so a crashed daemon's stale "running" snapshot is reported as
// stopped rather than lingering green forever (issue #418).
func daemonIsLive(stateDir string) bool {
	_, ownership := classifyPIDFile(pidFilePath(stateDir))
	return ownership == pidLive
}

func (a *App) renderStatusOutput(global globalOptions, stateDir string, snapshot corpusSnapshot, source string, staleRunning bool, recentFailures []model.Document) int {
	if global.jsonOutput {
		payload := map[string]interface{}{
			"source":    source,
			"state_dir": stateDir,
			"snapshot":  snapshot,
		}
		if rf := recentFailuresJSON(recentFailures); len(rf) > 0 {
			// Additive, optional field: the per-document failure list (rel_path,
			// doc_type, redacted reason) surfaced alongside errors=N so --json
			// consumers see *what* failed, not just a count (#414). Omitted when
			// there are no failures.
			payload["recent_failures"] = rf
		}
		if staleRunning {
			// Additive, optional field: signals that the snapshot recorded
			// indexing as running but no live daemon was found, so running was
			// reconciled to false. Absent in the common case (#418).
			payload["stale_running"] = true
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writeCLIError(a.stderr, true, exitGeneric, fmt.Sprintf("encode status json: %v", err))
			return exitGeneric
		}
		return exitSuccess
	}

	if global.quiet {
		return exitSuccess
	}
	s := a.sty(false)
	writeln(a.stdout)
	writeln(a.stdout, s.kv("State", stateDir))
	writeln(a.stdout, s.kv("Source", source))
	writeln(a.stdout, s.kv("Timestamp", snapshot.Timestamp))
	writeln(a.stdout)

	runningLabel := s.dim("stopped")
	switch {
	case snapshot.Indexing.Running:
		runningLabel = s.Green.Render("running")
	case staleRunning:
		runningLabel = s.dim("stopped") + "  " + s.dim("(stale snapshot; daemon not running)")
	}

	writef(a.stdout, "  %s  %s  %s\n", s.sectionHeader("Indexing"), s.dim("mode="+snapshot.Indexing.Mode), runningLabel)
	writef(a.stdout, "    %s  %s  %s  %s\n",
		s.stat("scanned", snapshot.Indexing.Scanned),
		s.stat("indexed", snapshot.Indexing.Indexed),
		s.stat("skipped", snapshot.Indexing.Skipped),
		s.stat("deleted", snapshot.Indexing.Deleted),
	)
	writef(a.stdout, "    %s  %s  %s  %s  %s",
		s.stat("reps", snapshot.Indexing.Representations),
		s.stat("chunks", snapshot.Indexing.ChunksTotal),
		s.stat("embedded", snapshot.Indexing.EmbeddedOK),
		s.stat("pending", snapshot.Indexing.EmbeddedPending),
		s.stat("unknown", snapshot.Indexing.Unknown),
	)
	if snapshot.Indexing.Errors > 0 {
		writef(a.stdout, "  %s", s.Red.Render(fmt.Sprintf("errors=%d", snapshot.Indexing.Errors)))
	} else {
		writef(a.stdout, "  %s", s.stat("errors", snapshot.Indexing.Errors))
	}
	writeln(a.stdout)
	writeln(a.stdout)

	a.renderCoverageBlock(s, snapshot)
	a.renderRecentFailuresBlock(s, recentFailures)

	writef(a.stdout, "  %s  %s  %s\n",
		s.sectionHeader("Documents"),
		s.stat("total", snapshot.TotalDocs),
		s.stat("code_ratio", fmt.Sprintf("%.4f", snapshot.CodeRatio)),
	)
	if len(snapshot.DocCounts) > 0 {
		keys := make([]string, 0, len(snapshot.DocCounts))
		for key := range snapshot.DocCounts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writef(a.stdout, "    %s\n", s.stat(key, snapshot.DocCounts[key]))
		}
	}
	writeln(a.stdout)
	return exitSuccess
}

// renderCoverageBlock prints an honest "Coverage / not-indexed" section that
// breaks the skipped total down by reason (#414): "3 files unsupported_format,
// 1 secret_excluded, …". No-op when nothing was skipped so healthy corpora keep
// the terminal clean. Reasons are printed in a stable sorted order.
func (a *App) renderCoverageBlock(s styles, snapshot corpusSnapshot) {
	summary := snapshot.Indexing.SkipSummary
	if summary == nil || len(summary.Categories) == 0 {
		return
	}
	reasons := make([]string, 0, len(summary.Categories))
	for reason := range summary.Categories {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	writef(a.stdout, "  %s  %s\n",
		s.sectionHeader("Coverage"),
		s.dim("not indexed — what was skipped & why"),
	)
	for _, reason := range reasons {
		writef(a.stdout, "    %s\n", s.stat(reason, summary.Categories[reason]))
		if hint := skipReasonHint(reason); hint != "" {
			writef(a.stdout, "      %s\n", s.dim(hint))
		}
	}
	writeln(a.stdout)
}

// skipReasonHint returns the remediation guidance for a skip reason — the
// "here's what to install or configure" half of the honest-coverage report
// (#414). It is empty for reasons that are working as intended (an archive
// container, an ignored binary, an ignore-rule match): printing an action for
// those would train operators to ignore the block.
//
// An unrecognized reason (a newer server, per the additive skip_reasons enum)
// also returns empty rather than guessing.
func skipReasonHint(reason string) string {
	switch reason {
	case model.SkipReasonUnsupportedFormat:
		return "no active extraction engine reads these formats — install a richer extractor (e.g. the docling-enabled build) or set ingest.extractor"
	case model.SkipReasonSizeCap:
		return "larger than ingest.max_file_mb — raise the cap to include them"
	case model.SkipReasonPathExcluded:
		return "matched ingest.path_excludes — drop the pattern to include them"
	case model.SkipReasonSecretExcluded:
		return "matched a secret pattern and was withheld on purpose — review ingest.secret_patterns if this was unintended"
	case model.SkipReasonLanguageUncovered:
		return "source language is outside the STT model's declared stt_languages coverage — route it via media.stt.language_providers to a model that covers it, or set media.stt.on_uncovered_language=warn to transcribe anyway"
	case model.SkipReasonTranscriptPartial:
		return "the windowed decode covered less of the recording than media.stt.min_coverage requires: check the STT endpoint for the window failures, then re-index, or set media.stt.on_partial_transcript=warn to index the partial transcript anyway"
	default:
		return ""
	}
}

// renderRecentFailuresBlock prints up to statusRecentFailuresLimit recent
// per-document ingest failures with rel_path and a redacted reason (#414 part
// 2), so `status` surfaces *which* files failed rather than only errors=N.
// No-op when there are none.
func (a *App) renderRecentFailuresBlock(s styles, recentFailures []model.Document) {
	if len(recentFailures) == 0 {
		return
	}
	writef(a.stdout, "  %s  %s\n",
		s.sectionHeader("Recent failures"),
		s.dim(fmt.Sprintf("%d shown", len(recentFailures))),
	)
	for _, d := range recentFailures {
		msg := ingest.RedactCredentialsForDisplay(strings.TrimSpace(d.ErrorMessage))
		if msg == "" {
			writef(a.stdout, "    %s\n", s.Red.Render(d.RelPath))
			continue
		}
		writef(a.stdout, "    %s  %s\n", s.Red.Render(d.RelPath), s.dim(msg))
	}
	writeln(a.stdout)
}

// recentFailuresJSON projects the recent-failure documents into the additive
// `recent_failures` array emitted by `status --json`: rel_path, doc_type,
// mtime_unix, and a redacted error_message. Returns nil for an empty input so
// the caller omits the field entirely on a healthy corpus.
func recentFailuresJSON(recentFailures []model.Document) []map[string]interface{} {
	if len(recentFailures) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(recentFailures))
	for _, d := range recentFailures {
		out = append(out, map[string]interface{}{
			"rel_path":      d.RelPath,
			"doc_type":      d.DocType,
			"mtime_unix":    d.MTimeUnix,
			"error_message": ingest.RedactCredentialsForDisplay(d.ErrorMessage),
		})
	}
	return out
}
