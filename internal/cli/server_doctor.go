package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
	"github.com/dirstral/dir2mcp/internal/statefs"
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
// each capability the server uses, extractor availability, daemon
// liveness/port drift, a stuck-pending-embedding backlog, and recent
// indexing-failure aggregation.
//
// By default it does NOT contact remote providers — keeping the command
// fast and side-effect-free was deliberately preferred over "ping"
// checks that could fail for transient unrelated reasons. The opt-in
// --deep flag activates a single cheap probe embed so a present-but-
// invalid/expired credential fails loudly instead of passing as ok (a
// constructed-but-never-called client cannot detect bad creds — #434).
func (a *App) runServerDoctor(ctx context.Context, global globalOptions, args []string) int {
	deep, code := parseDoctorFlags(a, global, args)
	if code != exitSuccess {
		return code
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
		providerCheck(ctx, cfg, "embed", provider.CapEmbed, true, deep),
		providerCheck(ctx, cfg, "chat", provider.CapChat, false, false),
		egressCheck(cfg),
		extractorCheck(cfg),
		extractionCoverageCheck(ctx, a, cfg),
		indexingFailureCheck(ctx, a, cfg),
		daemonLivenessCheck(cfg),
		stuckPendingCheck(ctx, a, cfg),
	)
	return a.renderDoctorReport(global, checks)
}

// parseDoctorFlags parses the server-side doctor flags. Only --deep is
// accepted; positional arguments are rejected (a client name would have
// been routed to the client-side doctor before reaching here).
func parseDoctorFlags(a *App, global globalOptions, args []string) (deep bool, code int) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	deepFlag := fs.Bool("deep", false, "actively probe providers (a cheap embed call) instead of only constructing clients")
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid doctor flags: %v", err))
		return false, exitConfigInvalid
	}
	if fs.NArg() > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("dir2mcp doctor (server-side) does not accept arguments: %s", strings.Join(fs.Args(), " ")))
		return false, exitConfigInvalid
	}
	return *deepFlag, exitSuccess
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
	if err := statefs.MkdirAllHardened(stateDir); err != nil {
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
//
// When deep is set (only for the embed capability, via `doctor --deep`)
// the built embedder is exercised with a single cheap probe embed so a
// present-but-invalid/expired credential surfaces as an error instead
// of passing as ok — constructing a client never touches the network,
// so it cannot detect bad creds (#434). The probe error is a provider
// error (status code + code, never the key/URL), so it is safe to show.
func providerCheck(ctx context.Context, cfg config.Config, label string, cap provider.Capability, required, deep bool) doctorCheck {
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
		emb, ferr := providerfactory.Embedder(prof)
		if ferr != nil {
			return doctorCheck{Name: "provider." + label, Status: doctorStatusError,
				Detail: fmt.Sprintf("profile %q unusable: %v", prof.Name, ferr)}
		}
		if deep {
			if perr := probeEmbedder(ctx, emb, prof); perr != nil {
				return doctorCheck{Name: "provider." + label, Status: doctorStatusError,
					Detail: fmt.Sprintf("profile %q failed live probe (credential invalid/expired or endpoint unreachable): %v", prof.Name, perr)}
			}
			return doctorCheck{Name: "provider." + label, Status: doctorStatusOK, Detail: prof.Name + " (probe ok)"}
		}
	}
	return doctorCheck{Name: "provider." + label, Status: doctorStatusOK, Detail: prof.Name}
}

// probeEmbedder issues one bounded, throwaway query-role embed so the
// doctor can distinguish a working credential from a present-but-bad
// one. The input is a fixed non-sensitive string and the result is
// discarded; only the error matters. A short timeout keeps `doctor
// --deep` responsive even when the endpoint hangs.
func probeEmbedder(ctx context.Context, emb model.Embedder, prof provider.Profile) error {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := emb.Embed(pctx, prof.EmbedTextModel, model.EmbedQuery, []string{"dir2mcp doctor probe"})
	return err
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
	// Restrict to index-eligible rows (deleted=0 AND status='ok') so skipped
	// or already-errored documents don't inflate the "needs extraction" count
	// or trip a false error — those are surfaced by indexing_failures instead.
	okCounts, err := sqliteStore.ActiveDocCountsByStatus(ctx, "ok")
	if err != nil {
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: err.Error()}
	}
	var extractable int64
	for docType, n := range okCounts {
		if ingest.ShouldGenerateExtractedMarkdown(docType) {
			extractable += n
		}
	}

	// (A/A') Report either a hard dead-end (index-eligible extractable documents
	// exist but no extractor at all) or partial coverage (an active extractor that
	// cannot read some present formats). The coverage half runs even when no
	// status="ok" extractable row exists, because the uncovered documents are by
	// construction NOT status="ok" (§7.7, spec 0.60.1). Factored out to keep this
	// function's branching under the complexity budget.
	if check, reported := extractionAvailabilityCheck(ctx, sqliteStore, cfg, name, extractable); reported {
		return check
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

// extractionAvailabilityCheck reports the extractor-coverage verdict. It returns
// (check, true) when it has a verdict to surface — either:
//
//	(A)  index-eligible (status="ok") extractable documents exist but no extractor
//	     is available at all → a hard configuration dead-end (error); or
//	(A') an extractor is active but cannot read some formats present in the durable
//	     record → those documents are recorded as skipped (lenient) or error
//	     (strict), never as indexed (§7.4.B.2), so partial coverage is a warning
//	     that NAMES the exact uncovered extensions (within §7.7's "surface the
//	     reason" mandate) so the remedy is actionable — "install docling / add a
//	     provider for .odt, .tiff" (#395) — instead of an opaque failure count.
//
// (A') counts extractable documents of EVERY status via the shared
// computeExtractionCoverage verdict (the same one the `up` banner prints). It
// used to count status="ok" rows only, which went blind the moment #584 recorded
// the gap honestly as status="skipped": measured on main, a store holding an .odt
// durably skipped and a .tiff errored produced "ok (1 extractable doc(s); 0/0
// chunks embedded)" and named neither format.
//
// It returns (_, false) when nothing needs surfacing, letting the caller fall
// through to the embedding checks.
func extractionAvailabilityCheck(ctx context.Context, sqliteStore *store.SQLiteStore, cfg config.Config, name string, okExtractable int64) (doctorCheck, bool) {
	decision := ingest.DescribeDocumentExtractorContext(ctx, cfg)
	if decision.Name == "" && okExtractable > 0 {
		detail := fmt.Sprintf(
			"%d document(s) need extraction (pdf/image/document) but no extractor is available: %s. "+
				"They produce no searchable text. Enable an extractor (set ingest.extractor to auto/docling/mistral, "+
				"not off), then make one available (install docling or set MISTRAL_API_KEY), and run `dir2mcp reindex`.",
			okExtractable, decision.Reason)
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: detail}, true
	}
	// The coverage half runs even with NO primary engine: on a lean install whose
	// only extractable documents are already durably skipped, nothing is
	// status="ok", so (A) above does not fire, yet the corpus still holds formats
	// nothing reads. The verdict then lists every present extractable format.
	cov, err := computeExtractionCoverage(ctx, sqliteStore, cfg, decision)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: err.Error()}, true
	}
	if len(cov.Uncovered) == 0 {
		return doctorCheck{}, false
	}
	engine := cov.Engine
	if engine == "" {
		engine = "none"
	}
	return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: fmt.Sprintf(
		"%d document(s) in %d format(s) are uncovered by the active extractor (%s): %s. "+
			"They produce no searchable text (recorded as skipped or error, never as indexed). %s",
		cov.Docs, len(cov.Uncovered), engine, strings.Join(cov.Uncovered, ", "), cov.Remedy)}, true
}

// extractorIsStructured maps a resolved extractor name (ExtractorDecision.Name)
// to the structured/flat distinction the ingest capability table uses: the
// docling family (docling / docling-serve) emits a DoclingDocument and reads the
// broader structured set; mistral-ocr is the flat path. It mirrors the
// structuredExtractor type-assertion Service.extractorCanReadExt performs at
// runtime, so the doctor's coverage verdict matches what indexing will actually
// route.
func extractorIsStructured(name string) bool {
	switch strings.TrimSpace(name) {
	case "docling", "docling-serve":
		return true
	default:
		return false
	}
}

// uncoveredExtractableExtensions returns the sorted, distinct extensions present
// in extCounts that no active extraction engine can read, plus the total document
// count they account for. It consults the SAME per-format router the indexing path
// uses (ingest.ExtractionCovered → selectExtractionRoute), so the doctor names
// exactly the formats that will be skipped with an unsupported-format diagnostic
// (#394/#395) — including the pandoc (T2, #393) tier the coarse structured/flat
// boolean cannot express. `structured`/`flatOCR`/`pandoc` are the active engines
// derived from the extractor decision; `policy` is ingest.extractor. Extension-less
// assets (bucketed under "") are ignored — they carry no format to name.
func uncoveredExtractableExtensions(extCounts map[string]int64, policy string, structured, flatOCR, pandoc bool) (exts []string, docs int64) {
	for ext, n := range extCounts {
		if ext == "" {
			continue
		}
		if ingest.ExtractionCovered(policy, structured, flatOCR, pandoc, ext) {
			continue
		}
		exts = append(exts, ext)
		docs += n
	}
	sort.Strings(exts)
	return exts, docs
}

// uncoveredExtractionRemedy tailors the remediation hint to the engines NOT yet
// active. It names only engines that would actually help: docling for the
// OpenXML-Office/tiff/bmp formats and pandoc (T2, #393) for the born-digital
// OpenDocument/RTF/EPUB family. Formats no installable engine can read (e.g.
// .gif/.svg/.odp/.ods/legacy .doc) get a pre-conversion hint. `structured`/`pandoc`
// report which of those engines is already active, so an already-active engine is
// never suggested.
func uncoveredExtractionRemedy(uncovered []string, structured, pandoc bool) string {
	// The §7.7 coverage report MUST, for each uncovered class, name the engine to
	// add AND the ingest.on_unsupported knob that governs whether the gap is a
	// warning (lenient, the default) or a per-document error (strict).
	const onUnsupportedHint = " Or set ingest.on_unsupported: strict to fail instead of skip these documents."

	doclingWouldCover, pandocWouldCover := false, false
	for _, ext := range uncovered {
		if !structured && ingest.ExtractorSupportsExt(true, ext) {
			doclingWouldCover = true
		}
		if !pandoc && ingest.PandocSupportsExt(ext) {
			pandocWouldCover = true
		}
	}

	var fixes []string
	if doclingWouldCover {
		fixes = append(fixes, "install docling (or set ingest.extractor=docling) for the Office/tiff/bmp formats")
	}
	if pandocWouldCover {
		fixes = append(fixes, "install pandoc (#393) for the born-digital OpenDocument/RTF/EPUB formats")
	}
	if len(fixes) == 0 {
		// Every engine that could help is already active (or none reads these):
		// no installable extractor covers them.
		return "These formats are read by no available extractor; pre-convert them to a supported format." + onUnsupportedHint
	}
	return "To cover them: " + strings.Join(fixes, "; ") + "; or pre-convert to a supported format." + onUnsupportedHint
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
	var lastFailureUnix int64
	if stats.FailureSummary != nil {
		chunkCategories = stats.FailureSummary.Categories
		lastFailureUnix = stats.FailureSummary.LastFailureUnix
	}
	if docErrors == 0 && len(chunkCategories) == 0 {
		return doctorCheck{Name: "indexing_failures", Status: doctorStatusOK, Detail: "0 failures"}
	}
	parts := []string{}
	if docErrors > 0 {
		parts = append(parts, fmt.Sprintf("%d document(s) failed extraction", docErrors))
	}
	if len(chunkCategories) > 0 {
		chunkPart := "chunk failures: " + summarizeFailureCategories(chunkCategories)
		// These counts are chunks CURRENTLY in a failed state, which may have
		// been recorded by a long-past run. Say when the newest of them happened
		// so the reader does not take a stale set for a live one (#783).
		if lastFailureUnix > 0 {
			chunkPart += fmt.Sprintf(" (latest %s)", time.Unix(lastFailureUnix, 0).UTC().Format(time.RFC3339))
		}
		parts = append(parts, chunkPart)
		if hint := retryableFailureHint(chunkCategories); hint != "" {
			parts = append(parts, hint)
		}
	}
	return doctorCheck{Name: "indexing_failures", Status: doctorStatusWarn, Detail: strings.Join(parts, "; ")}
}

// retryableFailureHint names the recovery command when some of the failed
// chunks are in a category a plain embed retry can clear (#783). Without it the
// doctor reports the stranded chunks but leaves the operator to guess that the
// only supported fix used to be a full re-ingest.
func retryableFailureHint(categories map[string]int64) string {
	var retryable int64
	for category, n := range categories {
		if store.IsRequeueableCategory(category) {
			retryable += n
		}
	}
	if retryable == 0 {
		return ""
	}
	return fmt.Sprintf("%d retryable after fixing the provider: run `dir2mcp reindex --embeddings-only`", retryable)
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

// daemonProcess reports the pid recorded for this state dir and whether
// that process is currently alive. A missing/unreadable pid file yields
// (0, false). Used by both the liveness and stuck-pending checks.
func daemonProcess(stateDir string) (pid int, alive bool) {
	p, err := readPIDFile(pidFilePath(stateDir))
	if err != nil {
		return 0, false
	}
	return p, processIsAlive(p)
}

// daemonLivenessCheck reports whether a daemon is registered/alive for
// this state dir and whether its recorded listen port is still
// resolvable. It catches the port-drift class (#434): a daemon killed
// with SIGKILL or stopped by the service manager can leave a stale pid
// file or a connection.json pointing at a dead port, so clients hold a
// URL that no longer serves. All checks are local file reads — no
// network — so the row is cheap and always safe to run.
func daemonLivenessCheck(cfg config.Config) doctorCheck {
	const name = "daemon_liveness"
	pid, err := readPIDFile(pidFilePath(cfg.StateDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doctorCheck{Name: name, Status: doctorStatusOK, Detail: "no daemon registered (not running)"}
		}
		return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: fmt.Sprintf("pid file unreadable: %v", err)}
	}
	if !processIsAlive(pid) {
		return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: fmt.Sprintf(
			"stale pid file: pid %d is not running; a prior daemon exited without cleanup — run `dir2mcp down` to clear it", pid)}
	}
	port := readPreviousListenPort(cfg.StateDir)
	if port == "" {
		return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: fmt.Sprintf(
			"daemon running (pid %d) but connection.json is missing/unreadable; clients may hold a stale URL", pid)}
	}
	return doctorCheck{Name: name, Status: doctorStatusOK, Detail: fmt.Sprintf("daemon running (pid %d) on port %s", pid, port)}
}

// stuckPendingCheck surfaces a pending-embedding backlog that nothing is
// draining. The existing extraction_coverage row only fires at
// EmbeddedOK==0; a corpus with 40/100 chunks embedded and no running
// daemon previously passed as ok even though the remaining 60 were stuck
// forever (#434). This warns when chunks remain pending AND no daemon is
// alive to embed them; while a daemon is running, a backlog is normal
// mid-index and stays ok.
func stuckPendingCheck(ctx context.Context, a *App, cfg config.Config) doctorCheck {
	const name = "pending_backlog"
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
		return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: "store is not SQLite-backed; pending aggregation unavailable"}
	}
	if err := sqliteStore.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: fmt.Sprintf("initialize store: %v", err)}
	}
	stats, err := sqliteStore.CorpusStats(ctx)
	if err != nil {
		return doctorCheck{Name: name, Status: doctorStatusError, Detail: err.Error()}
	}
	if stats.EmbeddedPending == 0 {
		return doctorCheck{Name: name, Status: doctorStatusOK, Detail: "0 chunks pending embedding"}
	}
	if _, alive := daemonProcess(cfg.StateDir); alive {
		return doctorCheck{Name: name, Status: doctorStatusOK, Detail: fmt.Sprintf(
			"%d chunk(s) pending; a daemon is running and draining them", stats.EmbeddedPending)}
	}
	return doctorCheck{Name: name, Status: doctorStatusWarn, Detail: fmt.Sprintf(
		"%d chunk(s) stuck pending embedding and no daemon is running to drain them; start it with `dir2mcp up` (or run `dir2mcp reindex`)", stats.EmbeddedPending)}
}

// egressCheck reports, per content-carrying capability (embed, chat, ocr,
// stt), whether the resolved provider's effective endpoint is a public /
// third-party host — meaning corpus content leaves this machine — or a
// local/loopback/LAN endpoint (no egress). It exists so an operator with a
// data-residency / on-prem requirement can VERIFY "nothing leaves the host"
// from the resolved configuration instead of inferring it from the absence of
// cloud base_urls (#493).
//
// The check is purely informational and never fails or warns: sending corpus
// content to a cloud provider is the intended default for most users, not a
// misconfiguration, so it stays at "ok" and puts the verdict in Detail. All
// work is local config resolution and host classification — no network is
// touched, so the row is cheap and always safe to run.
//
// A capability that does not resolve (not configured) is skipped: an
// unconfigured provider sends nothing. When a resolved profile has no explicit
// base_url, the effective host is the provider kind's built-in default (e.g.
// kind: mistral -> api.mistral.ai), so a happy-path cloud setup is correctly
// reported as egress even though its base_url is blank.
func egressCheck(cfg config.Config) doctorCheck {
	const name = "egress"
	res := cfg.Providers()
	// Ordered so the detail reads embed, chat, ocr, stt regardless of map
	// iteration; each pair is a corpus-content-carrying capability.
	caps := []struct {
		label string
		cap   provider.Capability
	}{
		{"embed", provider.CapEmbed},
		{"chat", provider.CapChat},
		{"ocr", provider.CapOCR},
		{"stt", provider.CapSTT},
	}

	// Group public destinations by host so the same provider serving several
	// capabilities renders once (e.g. "api.mistral.ai (embed, chat, ocr)").
	byHost := map[string][]string{}
	hostOrder := []string{}
	resolvedAny := false
	for _, c := range caps {
		prof, err := res.Resolve(c.cap)
		if err != nil {
			continue // capability not configured -> nothing egresses for it
		}
		resolvedAny = true
		host := effectiveProviderHost(prof)
		if host == "" || hostIsLocal(host) {
			continue // loopback / LAN / self-hosted, or no known endpoint
		}
		if _, seen := byHost[host]; !seen {
			hostOrder = append(hostOrder, host)
		}
		byHost[host] = append(byHost[host], c.label)
	}

	if !resolvedAny {
		return doctorCheck{Name: name, Status: doctorStatusOK, Detail: "no content providers resolved"}
	}
	if len(hostOrder) == 0 {
		return doctorCheck{Name: name, Status: doctorStatusOK,
			Detail: "no third-party egress: all resolved providers target local/loopback or private/LAN endpoints"}
	}
	sort.Strings(hostOrder)
	parts := make([]string, 0, len(hostOrder))
	for _, h := range hostOrder {
		parts = append(parts, fmt.Sprintf("%s (%s)", h, strings.Join(byHost[h], ", ")))
	}
	return doctorCheck{Name: name, Status: doctorStatusOK, Detail: fmt.Sprintf(
		"corpus content egresses to third-party host(s): %s. For an on-prem/no-egress setup, see the README 'Fully local / no-egress' recipe.",
		strings.Join(parts, "; "))}
}

// kindDefaultHost maps a provider kind to the cloud host its client contacts
// when a profile sets no explicit base_url. It mirrors the defaultBaseURL
// constants in the per-provider clients (internal/{mistral,openai,anthropic,
// gemini,cohere,elevenlabs}). Self-hosted-only kinds (whisper/omniembed/colbert
// and the credential-less `local` openai profile) have no cloud default and are
// intentionally absent, so a blank base_url on those resolves to no host.
var kindDefaultHost = map[provider.Kind]string{
	provider.KindMistral:    "api.mistral.ai",
	provider.KindOpenAI:     "api.openai.com",
	provider.KindAnthropic:  "api.anthropic.com",
	provider.KindGemini:     "generativelanguage.googleapis.com",
	provider.KindCohere:     "api.cohere.com",
	provider.KindElevenLabs: "api.elevenlabs.io",
}

// effectiveProviderHost returns the hostname the resolved profile will actually
// contact: the host of its explicit base_url when set, else the kind's built-in
// cloud default. Returns "" when neither is known (a self-hosted kind whose
// base_url was left unset — misconfigured, surfaced by other checks, not egress
// to a known third party).
func effectiveProviderHost(prof provider.Profile) string {
	if raw := strings.TrimSpace(prof.BaseURL); raw != "" {
		if h := hostFromBaseURL(raw); h != "" {
			return h
		}
		return ""
	}
	return kindDefaultHost[prof.Kind]
}

// hostFromBaseURL extracts the lowercased hostname (no port) from a provider
// base_url. It tolerates a scheme-less value (e.g. "gpu-vps:9001") by parsing
// it as an authority.
func hostFromBaseURL(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	// Scheme-less: reparse with a scheme so host/port split correctly.
	if u2, err2 := url.Parse("http://" + raw); err2 == nil && u2.Host != "" {
		return strings.ToLower(u2.Hostname())
	}
	return ""
}

// hostIsLocal reports whether host denotes a loopback, private/LAN, or
// otherwise non-public endpoint — i.e. one that does not send corpus content
// off the machine/network. A bare single-label hostname (no dot, e.g.
// "gpu-vps") is treated as LAN. Public FQDNs and public IPs return false.
func hostIsLocal(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return true
	}
	switch host {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	for _, suffix := range []string{".local", ".localhost", ".internal", ".lan", ".intranet"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	// A single-label hostname (no dot) is not a public FQDN; treat as LAN.
	return !strings.Contains(host, ".")
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
