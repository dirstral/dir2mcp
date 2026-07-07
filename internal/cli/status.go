package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
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

	snapshotPath := filepath.Join(cfg.StateDir, "corpus.json")
	snapshot, err := readCorpusSnapshot(snapshotPath)
	source := "corpus_json"
	if err != nil {
		metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
		if _, statErr := os.Stat(metaPath); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("no state found in %s; run: dir2mcp up", cfg.StateDir))
				return exitGeneric
			}
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("read state: %v", statErr))
			return exitGeneric
		}

		st := a.storeForConfig(cfg)
		defer func() { _ = st.Close() }()
		if initErr := st.Init(ctx); initErr != nil && !errors.Is(initErr, model.ErrNotImplemented) {
			writeCLIError(a.stderr, global.jsonOutput, exitIndexLoadFailure, fmt.Sprintf("initialize metadata store: %v", initErr))
			return exitIndexLoadFailure
		}
		// status --json must emit a single JSON object, not an NDJSON stream.
		// Keep the emitter disabled so computed-snapshot warnings go to stderr.
		emitter := newNDJSONEmitter(a.stdout, false)
		snapshot, err = buildCorpusSnapshot(ctx, st, nil, a.stderr, emitter)
		if err != nil {
			writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("build status snapshot: %v", err))
			return exitGeneric
		}
		source = "computed"
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

// statusErrorMessageRedactors is a compact safety net of high-confidence
// credential patterns scrubbed from a failure message before it is printed by
// `status`, mirroring the MCP stats surface (internal/mcp redactStatsErrorMessage).
var statusErrorMessageRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(sk|pk|rk)[-_](live|test|prod)?[-_]?[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`(?i)\b(AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-.]{10,}\.[A-Za-z0-9_\-]{5,}`),
	regexp.MustCompile(`(?i)(authorization|api[_-]?key|token|secret|password|passwd)\s*[:=]\s*\S+`),
}

// redactStatusErrorMessage scrubs high-confidence credentials from a failure
// message before display. Returns msg unchanged when nothing matches.
func redactStatusErrorMessage(msg string) string {
	for _, rx := range statusErrorMessageRedactors {
		msg = rx.ReplaceAllString(msg, "[REDACTED]")
	}
	return msg
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
	}
	writeln(a.stdout)
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
		msg := redactStatusErrorMessage(strings.TrimSpace(d.ErrorMessage))
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
			"error_message": redactStatusErrorMessage(d.ErrorMessage),
		})
	}
	return out
}
