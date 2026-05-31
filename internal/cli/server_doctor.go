package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
	"github.com/dirstral/dir2mcp/internal/store"
)

// doctorCheck is one entry in the daemon-side `dir2mcp doctor` report.
// Status is one of "ok", "warn", or "error"; Detail is rendered
// verbatim in both the text and JSON output and should be a single
// human-readable line explaining the result or, on failure, the
// remediation hint.
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

const (
	doctorStatusOK    = "ok"
	doctorStatusWarn  = "warn"
	doctorStatusError = "error"
)

// runServerDoctor produces a daemon-side preflight report covering
// config loadability, state-dir writability, provider resolution for
// each capability the server uses, extractor availability, and recent
// indexing-failure aggregation. It does NOT contact remote providers
// — keeping the command fast and side-effect-free was deliberately
// preferred over deeper "ping" checks that would slow it down and
// could fail for transient unrelated reasons.
func (a *App) runServerDoctor(ctx context.Context, global globalOptions, args []string) int {
	if len(args) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("dir2mcp doctor (server-side) does not accept arguments: %s", strings.Join(args, " ")))
		return exitConfigInvalid
	}

	cfg, cfgErr := loadConfigWithGlobalOptions(global)
	checks := []doctorCheck{configCheck(cfgErr)}
	if cfgErr != nil {
		return a.renderDoctorReport(global, checks)
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(".", ".dir2mcp")
	}

	checks = append(checks,
		stateDirCheck(cfg.StateDir),
		providerCheck(cfg, "embed", provider.CapEmbed, true),
		providerCheck(cfg, "chat", provider.CapChat, false),
		extractorCheck(cfg),
		extractionCoverageCheck(ctx, a, cfg),
		indexingFailureCheck(ctx, a, cfg),
	)
	return a.renderDoctorReport(global, checks)
}

// configCheck reports whether the layered config loaded cleanly.
// A load failure short-circuits the rest of the doctor run since
// every subsequent check depends on it.
func configCheck(err error) doctorCheck {
	if err != nil {
		return doctorCheck{Name: "config", Status: doctorStatusError, Detail: err.Error()}
	}
	return doctorCheck{Name: "config", Status: doctorStatusOK, Detail: "loaded"}
}

// stateDirCheck verifies the configured state directory exists and is
// writable. It deliberately uses a real probe (touch a temp file)
// instead of a stat-only check because permission bits don't always
// reflect the effective writability of a path (mounts, ACLs).
func stateDirCheck(stateDir string) doctorCheck {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return doctorCheck{Name: "state_dir", Status: doctorStatusError, Detail: fmt.Sprintf("mkdir %s: %v", stateDir, err)}
	}
	probe, err := os.CreateTemp(stateDir, "doctor-probe-*")
	if err != nil {
		return doctorCheck{Name: "state_dir", Status: doctorStatusError, Detail: fmt.Sprintf("write %s: %v", stateDir, err)}
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return doctorCheck{Name: "state_dir", Status: doctorStatusOK, Detail: stateDir}
}

// providerCheck resolves cap through the provider model and, when
// resolved, asks providerfactory to build the matching adapter so the
// check fails fast on a configured-but-unusable profile. required
// distinguishes capabilities that must be present (embed) from those
// that degrade gracefully when absent (chat).
func providerCheck(cfg config.Config, label string, cap provider.Capability, required bool) doctorCheck {
	prof, err := cfg.Providers().Resolve(cap)
	if err != nil {
		status := doctorStatusError
		if !required {
			status = doctorStatusWarn
		}
		var ce *provider.ConfigError
		if errors.As(err, &ce) {
			return doctorCheck{Name: "provider." + label, Status: status, Detail: ce.Error()}
		}
		return doctorCheck{Name: "provider." + label, Status: status, Detail: err.Error()}
	}
	if cap == provider.CapEmbed {
		if _, ferr := providerfactory.Embedder(prof); ferr != nil {
			return doctorCheck{Name: "provider." + label, Status: doctorStatusError,
				Detail: fmt.Sprintf("profile %q unusable: %v", prof.Name, ferr)}
		}
	}
	return doctorCheck{Name: "provider." + label, Status: doctorStatusOK, Detail: prof.Name}
}

// extractorCheck delegates to ingest.DescribeDocumentExtractor so the
// doctor, the up banner, and the support-bundle all agree on the
// routing story for OCR. Disabled extractors and fallback selections
// surface as warns (not errors) — operators may intentionally turn
// OCR off, and the Mistral fallback is functional even if not the
// preferred choice.
func extractorCheck(cfg config.Config) doctorCheck {
	d := ingest.DescribeDocumentExtractor(cfg)
	if d.Name == "" {
		return doctorCheck{Name: "extractor", Status: doctorStatusWarn, Detail: d.Reason}
	}
	detail := d.Name
	if d.Reason != "" {
		detail = fmt.Sprintf("%s (%s)", d.Name, d.Reason)
	}
	status := doctorStatusOK
	if d.Source == "fallback" {
		status = doctorStatusWarn
	}
	return doctorCheck{Name: "extractor", Status: status, Detail: detail}
}

// extractionCoverageCheck surfaces the two silent failures behind a corpus
// that returns "no relevant context" on every ask: (A) documents that need an
// extractor (pdf/image/document) exist, but no extractor is available, so they
// produce no searchable text; and (B) chunks exist but none are embedded, so
// search matches nothing. Both are reported with the concrete document/chunk
// counts and the remedial command, since the user-facing symptom (empty
// answers) gives no hint of the cause.
//
// Like indexingFailureCheck, it reads CorpusStats from the SQLite store and
// degrades gracefully when there is no index yet or the store is not
// SQLite-backed.
func extractionCoverageCheck(ctx context.Context, a *App, cfg config.Config) doctorCheck {
	const name = "extraction_coverage"
	metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
	if _, err := os.Stat(metaPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doctorCheck{Name: name, Status: doctorStatusOK, Detail: "no index yet"}
		}
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: fmt.Sprintf("stat %s: %v", metaPath, err)}
	}
	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	sqliteStore, ok := st.(*store.SQLiteStore)
	if !ok {
		return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: "store is not SQLite-backed; coverage aggregation unavailable"}
	}
	if err := sqliteStore.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: fmt.Sprintf("initialize store: %v", err)}
	}
	stats, err := sqliteStore.CorpusStats(ctx)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: err.Error()}
	}

	// Count documents whose type requires an extractor to become searchable.
	var extractable int64
	for docType, n := range stats.DocCounts {
		if ingest.ShouldGenerateExtractedMarkdown(docType) {
			extractable += n
		}
	}

	// (A) Extractable documents exist but no extractor will run: those
	// documents produce no representation, no chunks, and never appear in any
	// answer. This is a hard configuration dead-end, so it is an error.
	if extractable > 0 {
		if d := ingest.DescribeDocumentExtractor(cfg); d.Name == "" {
			detail := fmt.Sprintf(
				"%d document(s) need extraction (pdf/image/document) but no extractor is available: %s. "+
					"They produce no searchable text. Install docling or set MISTRAL_API_KEY, then run `dir2mcp reindex`.",
				extractable, d.Reason)
			return doctorCheck{Name: name, Status: doctorStatusError, Detail: detail}
		}
	}

	// (B) Chunks were created but none are embedded: extraction succeeded yet
	// search still matches nothing. Warn (it can also be a transient mid-index
	// state) with the remedial action.
	if stats.ChunksTotal > 0 && stats.EmbeddedOK == 0 {
		detail := fmt.Sprintf(
			"%d chunk(s) indexed but 0 embedded; no embedding provider has run. "+
				"Set a provider credential (e.g. MISTRAL_API_KEY / OPENAI_API_KEY) and run `dir2mcp reindex`.",
			stats.ChunksTotal)
		return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: detail}
	}

	return doctorCheck{Name: name, Status: doctorStatusOK, Detail: fmt.Sprintf(
		"%d extractable doc(s); %d/%d chunks embedded", extractable, stats.EmbeddedOK, stats.ChunksTotal)}
}

// indexingFailureCheck reads the store-level FailureSummary (set by
// CorpusStats) and reports the dominant failure category, if any. The
// goal is to surface "67 of 89 errors are rate_limit" at preflight
// time so the operator knows where to look without having to grep
// server.log. Missing state and zero-failure corpora both pass.
//
// The check requires the SQLite-backed store because CorpusStats is
// not on the model.Store interface; with a non-sqlite store the check
// degrades to a warn rather than failing the whole doctor run.
func indexingFailureCheck(ctx context.Context, a *App, cfg config.Config) doctorCheck {
	metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
	if _, err := os.Stat(metaPath); err != nil {
		// Only ENOENT is the legitimate "no index yet" path. A
		// permission or I/O failure here would otherwise be reported
		// as healthy, hiding the actual blocker.
		if errors.Is(err, os.ErrNotExist) {
			return doctorCheck{Name: "indexing_failures", Status: doctorStatusOK, Detail: "no index yet"}
		}
		return doctorCheck{Name: "indexing_failures", Status: doctorStatusError,
			Detail: fmt.Sprintf("stat %s: %v", metaPath, err)}
	}
	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	sqliteStore, ok := st.(*store.SQLiteStore)
	if !ok {
		return doctorCheck{Name: "indexing_failures", Status: doctorStatusWarn,
			Detail: "store is not SQLite-backed; failure aggregation unavailable"}
	}
	if err := sqliteStore.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		return doctorCheck{Name: "indexing_failures", Status: doctorStatusError, Detail: fmt.Sprintf("initialize store: %v", err)}
	}
	stats, err := sqliteStore.CorpusStats(ctx)
	if err != nil {
		return doctorCheck{Name: "indexing_failures", Status: doctorStatusError, Detail: err.Error()}
	}
	return renderIndexingFailureCheck(stats)
}

// renderIndexingFailureCheck combines document-level and chunk-level
// failure counters into one doctor row. Document-level errors
// (CorpusStats.Errors) come from the extraction/representation stage
// — they happen *before* chunks are ever created, so a corpus that
// fails entirely at extraction has 0 chunk-level failures and
// previously slipped through this check as "0 failures" even though
// every PDF in the corpus had failed. Both counts are surfaced so
// neither stage can hide.
func renderIndexingFailureCheck(stats model.CorpusStats) doctorCheck {
	docErrors := stats.Errors
	var chunkCategories map[string]int64
	if stats.FailureSummary != nil {
		chunkCategories = stats.FailureSummary.Categories
	}
	if docErrors == 0 && len(chunkCategories) == 0 {
		return doctorCheck{Name: "indexing_failures", Status: doctorStatusOK, Detail: "0 failures"}
	}
	parts := []string{}
	if docErrors > 0 {
		parts = append(parts, fmt.Sprintf("%d document(s) failed extraction", docErrors))
	}
	if len(chunkCategories) > 0 {
		parts = append(parts, "chunk failures: "+summarizeFailureCategories(chunkCategories))
	}
	return doctorCheck{Name: "indexing_failures", Status: doctorStatusWarn, Detail: strings.Join(parts, "; ")}
}

// summarizeFailureCategories renders the failure-category map as a
// compact "category=count, ..." string sorted by count descending so
// the most-frequent failure mode appears first.
func summarizeFailureCategories(categories map[string]int64) string {
	type entry struct {
		category string
		count    int64
	}
	entries := make([]entry, 0, len(categories))
	for k, v := range categories {
		entries = append(entries, entry{category: k, count: v})
	}
	// Insertion sort for stability + simplicity over a tiny N. Tied
	// counts break by category name ascending so the output is
	// byte-identical across runs (Go map iteration would otherwise
	// randomize the seed order before the sort, leaving ties
	// non-deterministic).
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0; j-- {
			if entries[j-1].count > entries[j].count {
				break
			}
			if entries[j-1].count == entries[j].count && entries[j-1].category < entries[j].category {
				break
			}
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("%s=%d", e.category, e.count))
	}
	return strings.Join(parts, ", ")
}

// renderDoctorReport emits checks as either JSON (one object with an
// "ok" rollup boolean + the checks array) or as a human-readable text
// block. Exit code is 0 unless any check is at error severity; warn
// does not fail the command so operators can keep using doctor in
// scripts that only care about hard failures.
func (a *App) renderDoctorReport(global globalOptions, checks []doctorCheck) int {
	exitCode := exitSuccess
	for _, c := range checks {
		if c.Status == doctorStatusError {
			exitCode = exitGeneric
			break
		}
	}

	if global.jsonOutput {
		_ = emitJSON(a.stdout, map[string]interface{}{
			"ok":     exitCode == exitSuccess,
			"checks": checks,
		})
		return exitCode
	}

	if !global.quiet {
		printDoctorChecks(a.stdout, checks)
	}
	return exitCode
}

// printDoctorChecks emits checks as one line per check, with a
// leading status tag so the output is easy to scan. Kept dependency-
// free of the styles package so it works identically under --quiet
// suppression and inside support-bundle captures.
func printDoctorChecks(out io.Writer, checks []doctorCheck) {
	for _, c := range checks {
		_, _ = fmt.Fprintf(out, "%-20s %-5s  %s\n", c.Name, c.Status, c.Detail)
	}
}
