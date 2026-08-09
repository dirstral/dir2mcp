package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/buildinfo"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/statefs"
)

// supportLogTailBytes caps how much of server.log is included in the
// bundle. Keeps bundles shareable over chat and email while still
// capturing a meaningful window of recent activity.
const supportLogTailBytes int64 = 2 * 1024 * 1024 // 2 MB

// runSupportBundle implements `dir2mcp support-bundle`: it gathers a
// small set of files (effective config snapshot, server log tail,
// status JSON, list-files JSON, version, OS info, routing decisions)
// into a single tar.gz suitable for sharing with a maintainer when
// diagnosing install/indexing problems.
//
// Two privacy tiers govern what lands in it (see support_bundle_redact.go):
//
//   - Credentials are removed from EVERY artifact in EVERY mode. The effective
//     config snapshot carries only `secret_sources:` metadata (where each
//     credential came from), never the credentials; server.log, the client
//     bridge logs, the snapshot and connection URLs all run through
//     redactBundleSecrets, which strips bearer tokens, Authorization headers,
//     URL userinfo, and the value of every URL query/fragment parameter.
//   - Values naming the operator's machine or corpus — corpus paths and titles,
//     extraction error messages, and the snapshot's paths, endpoints, hosts,
//     prompts and hand-written lists — are removed unless the operator opts in
//     with --include-content. That flag never re-enables credential disclosure.
func (a *App) runSupportBundle(ctx context.Context, global globalOptions, args []string) int {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	// Route flag-parse output through writeCLIError so JSON callers get
	// a single structured error object on stdout/stderr instead of the
	// flag package's prose mixed with our JSON envelope.
	fs.SetOutput(io.Discard)
	output := fs.String("output", "", "destination tar.gz path (default: ./dir2mcp-support-<timestamp>.tar.gz)")
	includeContent := fs.Bool("include-content", false,
		"include corpus paths/titles/extraction error messages in list-files.json and status.json, and local paths/endpoints/prompts in config.snapshot.yaml (may disclose corpus content and local layout — review the bundle before sharing; credentials stay redacted either way)")
	if err := fs.Parse(args); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("support-bundle: %v", err))
		return exitConfigInvalid
	}
	if rest := fs.Args(); len(rest) > 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("support-bundle does not accept positional arguments: %s", strings.Join(rest, " ")))
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

	destPath := strings.TrimSpace(*output)
	if destPath == "" {
		destPath = filepath.Join(".", fmt.Sprintf("dir2mcp-support-%s.tar.gz", time.Now().UTC().Format("20060102-150405")))
	}

	files, warnings := collectSupportArtifacts(ctx, a, cfg, *includeContent)

	if err := writeSupportBundle(destPath, files); err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("write support bundle: %v", err))
		return exitGeneric
	}

	if global.jsonOutput {
		_ = emitJSON(a.stdout, map[string]interface{}{
			"output":   destPath,
			"entries":  supportEntryNames(files),
			"warnings": warningStrings(warnings),
		})
		return exitSuccess
	}

	if !global.quiet {
		_, _ = fmt.Fprintf(a.stdout, "Wrote support bundle to %s\n", destPath)
	}
	// Warnings — including the --include-content privacy-consent notice — are
	// diagnostics on stderr. --quiet suppresses non-error output, but a consent
	// warning must never be silently dropped, so it survives quiet mode.
	for _, w := range warnings {
		_, _ = fmt.Fprintf(a.stderr, "warning: %v\n", w)
	}
	return exitSuccess
}

// supportFile is one entry in the tar.gz: a logical name (relative path
// inside the archive) and the bytes to write. Empty Bytes is allowed —
// the entry is still written, which is useful for marking "this artifact
// would have been collected but the source was missing".
type supportFile struct {
	Name  string
	Bytes []byte
}

// collectSupportArtifacts assembles every artifact for the bundle.
// Missing or unreadable sources are recorded as warnings rather than
// fatal errors so the bundle is always produced even when the daemon
// is down or the index is empty.
func collectSupportArtifacts(ctx context.Context, a *App, cfg config.Config, includeContent bool) ([]supportFile, []error) {
	var files []supportFile
	var warnings []error

	add := func(name string, body []byte, err error) {
		if err != nil {
			warnings = append(warnings, fmt.Errorf("%s: %w", name, err))
			return
		}
		files = append(files, supportFile{Name: name, Bytes: body})
	}

	add("version.txt", []byte(buildinfo.Display()+"\n"), nil)
	add("os.txt", []byte(fmt.Sprintf("GOOS=%s\nGOARCH=%s\n", runtime.GOOS, runtime.GOARCH)), nil)

	// The snapshot used to be copied verbatim, which put absolute corpus/state
	// paths and any URL-embedded credential into the default bundle (#720).
	// redactConfigSnapshot applies both privacy tiers; see
	// support_bundle_redact.go.
	cfgBytes, cfgErr := readFileBest(config.EffectiveSnapshotPath(cfg.StateDir))
	if cfgErr == nil {
		cfgBytes = redactConfigSnapshot(cfgBytes, includeContent)
	}
	add("config.snapshot.yaml", cfgBytes, cfgErr)

	// server.log is redirected daemon stdout/stderr and can carry query text,
	// answer snippets, and bearer tokens. Redact it exactly like the client
	// bridge logs before it enters the shareable bundle.
	logBytes, logErr := readLogTail(serverLogPath(cfg.StateDir), supportLogTailBytes)
	if logErr == nil && len(logBytes) > 0 {
		logBytes = []byte(redactBundleSecrets(string(logBytes)))
	}
	add("server.log", logBytes, logErr)

	statusBytes, statusErr := marshalStatusJSON(ctx, a, cfg, includeContent)
	add("status.json", statusBytes, statusErr)

	listBytes, listErr := marshalListFilesJSON(ctx, a, cfg, includeContent)
	add("list-files.json", listBytes, listErr)
	if includeContent {
		warnings = append(warnings, errors.New(
			"list-files.json/status.json/config.snapshot.yaml: --include-content may include corpus paths/titles/error messages and local paths, endpoints and prompts; review the bundle before sharing it"))
	}

	routingBytes, routingErr := marshalRoutingJSON(cfg)
	add("routing.json", routingBytes, routingErr)

	// Daemon liveness: whether the server the client talks to is even up, and
	// where (token redacted). An empty server.log next to an unreachable daemon
	// points at the transport/bridge layer rather than dir2mcp itself.
	daemonBytes, daemonErr := marshalDaemonLivenessJSON(cfg)
	add("daemon.json", daemonBytes, daemonErr)

	// Host MCP-client (Claude Desktop) logs, best-effort. The client launches a
	// `bunx mcp-remote` stdio→HTTP bridge, and transport-level "Failed to call
	// tool" errors surface there — not in dir2mcp's own server.log. Absent logs
	// are silently skipped (this is a diagnostic nicety, not a required artifact).
	files = append(files, collectClientMCPLogs()...)

	return files, warnings
}

// readFileBest reads a file if it exists, returning (bytes, nil) on
// success, ([]byte(nil), nil) when the path is missing (no warning —
// the support bundle is expected to run on partial/clean state dirs),
// or ([]byte(nil), err) on a real read failure.
func readFileBest(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return b, nil
}

// readLogTail returns the last maxBytes of path, or the whole file if
// it is smaller. Missing log file is silent (returns nil bytes); other
// errors propagate as a warning.
func readLogTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := stat.Size()
	if size <= maxBytes {
		return io.ReadAll(f)
	}
	if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// marshalStatusJSON computes the same payload `dir2mcp status --json`
// would emit. Falls back to a placeholder when no state is present so
// the bundle still records "we tried and found nothing". The snapshot's
// FailureSummary.Samples carry raw {rel_path, message} pairs that can echo
// corpus content, so they are redacted the same way list-files.json is
// unless the operator opts in with --include-content.
func marshalStatusJSON(ctx context.Context, a *App, cfg config.Config, includeContent bool) ([]byte, error) {
	snapshotPath := filepath.Join(cfg.StateDir, "corpus.json")
	snapshot, err := readCorpusSnapshot(snapshotPath)
	source := "corpus_json"
	if err != nil {
		metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
		if _, statErr := os.Stat(metaPath); statErr != nil {
			return json.MarshalIndent(map[string]interface{}{
				"available": false,
				"reason":    "no state found",
			}, "", "  ")
		}
		st := a.storeForConfig(cfg)
		defer func() { _ = st.Close() }()
		if initErr := st.Init(ctx); initErr != nil && !errors.Is(initErr, model.ErrNotImplemented) {
			return nil, fmt.Errorf("initialize store: %w", initErr)
		}
		emitter := newNDJSONEmitter(io.Discard, false)
		snapshot, err = buildCorpusSnapshot(ctx, st, nil, io.Discard, emitter)
		if err != nil {
			return nil, fmt.Errorf("build snapshot: %w", err)
		}
		source = "computed"
	}
	snapshot.Indexing.FailureSummary = redactFailureSamples(snapshot.Indexing.FailureSummary, includeContent)
	return json.MarshalIndent(map[string]interface{}{
		"source": source,
		// state_dir is an absolute local path, so it is the same disclosure
		// #720 reports for the snapshot's state_dir and is gated identically.
		// Redacting it only in config.snapshot.yaml would have achieved
		// nothing: the reader would simply have read it out of here instead.
		"state_dir": redactContentField(cfg.StateDir, includeContent),
		"snapshot":  snapshot,
	}, "", "  ")
}

// redactFailureSamples returns a copy of the failure summary whose sample
// fields are handled exactly like list-files.json: rel_path becomes an
// extension-only placeholder and the free-text message is blanked (set to the
// empty string, not omitted) by default, while --include-content restores the
// raw values run through the secret
// redactor. The Categories aggregate is a fixed error-category enum with no
// corpus content, so it is preserved either way. A copy is returned so the
// underlying snapshot (which may be shared with the store) is never mutated.
func redactFailureSamples(fs *model.FailureSummary, includeContent bool) *model.FailureSummary {
	if fs == nil {
		return nil
	}
	// LastFailureUnix is a timestamp, not corpus content, and it is the field
	// that tells a maintainer reading the bundle whether the failures it reports
	// are current or were stranded long before the bundle was taken (#783), so
	// it is preserved in both modes.
	out := &model.FailureSummary{LastFailureUnix: fs.LastFailureUnix}
	if fs.Categories != nil {
		out.Categories = make(map[string]int64, len(fs.Categories))
		for k, v := range fs.Categories {
			out.Categories[k] = v
		}
	}
	if len(fs.Samples) > 0 {
		out.Samples = make([]model.FailureSample, len(fs.Samples))
		for i, s := range fs.Samples {
			out.Samples[i] = model.FailureSample{
				RelPath:    listFileRelPath(s.RelPath, includeContent),
				Category:   s.Category,
				Message:    redactContentField(s.Message, includeContent),
				FailedUnix: s.FailedUnix,
			}
		}
	}
	return out
}

// supportBundleFileRow is the per-document shape emitted in the bundle's
// list-files.json. It deliberately uses snake_case keys (matching the
// MCP tool conventions) and adds error_message — a maintainer-facing
// field that is intentionally NOT part of the spec-bound MCP list_files
// tool output (SPEC §15.5 uses additionalProperties:false, and the
// support bundle is a diagnostic artifact outside that contract).
type supportBundleFileRow struct {
	RelPath      string `json:"rel_path"`
	DocType      string `json:"doc_type"`
	SourceType   string `json:"source_type,omitempty"`
	Title        string `json:"title,omitempty"`
	SizeBytes    int64  `json:"size_bytes"`
	MTimeUnix    int64  `json:"mtime_unix"`
	Status       string `json:"status"`
	Deleted      bool   `json:"deleted"`
	ErrorMessage string `json:"error_message,omitempty"`
	// HasError preserves the per-document failure signal in the default
	// (content-excluded) bundle, where the free-text error_message — which can
	// echo file content — is dropped. Present only when content is excluded.
	HasError bool `json:"has_error,omitempty"`
}

// marshalListFilesJSON dumps the per-document list. By default the
// content-bearing fields — rel_path, title and the free-text extraction
// error_message — are redacted so the bundle never discloses the full corpus
// inventory or content fragments without the operator's explicit consent; the
// per-document doc_type/status/size skeleton (including a has_error flag) is
// still emitted so a maintainer can triage extraction failures. Passing
// includeContent (the `--include-content` flag) restores the raw fields, still
// run through the secret redactor so credentials never leak either way. Reads
// directly from the sqlite store so it works without the daemon running.
func marshalListFilesJSON(ctx context.Context, a *App, cfg config.Config, includeContent bool) ([]byte, error) {
	metaPath := filepath.Join(cfg.StateDir, "meta.sqlite")
	if _, err := os.Stat(metaPath); err != nil {
		return json.MarshalIndent(map[string]interface{}{
			"available": false,
			"reason":    "no meta.sqlite",
		}, "", "  ")
	}
	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		return nil, fmt.Errorf("initialize store: %w", err)
	}
	docs, total, err := st.ListFiles(ctx, "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	rows := make([]supportBundleFileRow, 0, len(docs))
	for _, d := range docs {
		rows = append(rows, supportBundleFileRow{
			RelPath:      listFileRelPath(d.RelPath, includeContent),
			DocType:      d.DocType,
			SourceType:   d.SourceType,
			Title:        redactContentField(d.Title, includeContent),
			SizeBytes:    d.SizeBytes,
			MTimeUnix:    d.MTimeUnix,
			Status:       d.Status,
			Deleted:      d.Deleted,
			ErrorMessage: redactContentField(d.ErrorMessage, includeContent),
			HasError:     !includeContent && d.ErrorMessage != "",
		})
	}
	return json.MarshalIndent(map[string]interface{}{
		"content_included": includeContent,
		"files":            rows,
		"total":            total,
	}, "", "  ")
}

// listFileRelPath returns the document path for the bundle. With includeContent
// it is the raw path run through the secret redactor; otherwise the path is
// replaced with a placeholder that keeps only the file extension (needed to
// diagnose extractor/OCR routing) and discloses no directory names or basenames.
func listFileRelPath(relPath string, includeContent bool) string {
	if includeContent {
		return redactBundleSecrets(relPath)
	}
	if ext := redactedExtension(relPath); ext != "" {
		return "[redacted]" + ext
	}
	return "[redacted]"
}

// compoundExtensions are multi-part suffixes whose full form (not just the last
// segment filepath.Ext returns) can drive extractor/OCR routing. Preserving the
// whole suffix keeps the routing signal — e.g. .tar.gz instead of a misleading
// .gz — while still disclosing only the extension, never the basename.
var compoundExtensions = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"}

// redactedExtension returns the routing-relevant extension of relPath: a known
// compound suffix when present (case-insensitive match, original casing kept),
// otherwise the single trailing extension from filepath.Ext.
func redactedExtension(relPath string) string {
	lower := strings.ToLower(relPath)
	for _, ce := range compoundExtensions {
		if strings.HasSuffix(lower, ce) {
			return relPath[len(relPath)-len(ce):]
		}
	}
	return filepath.Ext(relPath)
}

// redactContentField returns a corpus-content string (title, error message) for
// the bundle: the secret-redacted value when the operator opted into content,
// and the empty string otherwise so the field is omitted entirely.
func redactContentField(value string, includeContent bool) string {
	if includeContent {
		return redactBundleSecrets(value)
	}
	return ""
}

// marshalRoutingJSON dumps the provider/extractor routing decisions
// (same data the `up` banner Models section shows). This is the
// fastest way for a maintainer to see whether the wrong OCR backend
// was selected without needing to ask the user to re-run `up`.
func marshalRoutingJSON(cfg config.Config) ([]byte, error) {
	return json.MarshalIndent(map[string]interface{}{
		"decisions": routingDecisions(cfg),
	}, "", "  ")
}

// writeSupportBundle writes files as a single gzip-compressed tarball at
// destPath. Directories in destPath must already exist.
//
// The archive is owner-only. It used to be written with
// os.OpenFile(..., O_TRUNC, 0o600), but an open mode is only applied when the
// file is CREATED: overwriting an existing 0644 bundle truncated it in place and
// left it world-readable, so the stated privacy boundary held only for the first
// write to a given path (#719). Routing through atomicWriteFile — the same
// temp+sync+chmod+rename helper the rest of the CLI uses — fixes that
// structurally: rename installs a fresh inode carrying the temp file's mode, so
// the destination's PRIOR mode cannot survive and no chmod race exists.
//
// (statefs's "tighten only if wider" rule deliberately does not apply here.
// That rule protects a pre-existing file an operator may have made stricter;
// this path owns the temp file outright and wants exactly statefs.FileMode.)
//
// Atomicity is the second half of the fix: a failure mid-write now leaves a
// previously valid bundle untouched instead of destroying it. The artifacts are
// already fully in memory, so buffering the tarball costs nothing beyond the
// (log-tail-capped) bundle size.
func writeSupportBundle(destPath string, files []supportFile) error {
	var buf bytes.Buffer
	if err := writeSupportTarGz(&buf, files); err != nil {
		return err
	}
	return atomicWriteFile(destPath, buf.Bytes(), statefs.FileMode)
}

// writeSupportTarGz streams files into w as a gzip-compressed tarball.
//
// Entries are 0o600 rather than 0o644: extracting a deliberately owner-only
// archive must not re-create the exposure the archive mode exists to prevent.
//
// Both writers are closed explicitly and their errors returned. They used to be
// deferred with the error discarded, which meant a failed gzip/tar flush
// produced a silently truncated bundle.
func writeSupportTarGz(w io.Writer, files []supportFile) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	now := time.Now()
	for _, file := range files {
		hdr := &tar.Header{
			Name:    file.Name,
			Mode:    0o600,
			Size:    int64(len(file.Bytes)),
			ModTime: now,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(file.Bytes); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finalize tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finalize gzip: %w", err)
	}
	return nil
}

func supportEntryNames(files []supportFile) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

// bundleSecretRedactors mask header-shaped credential material (bearer tokens
// and Authorization headers) before it is written into the shared bundle. URLs
// are handled structurally instead — see redactURLCredentials.
var bundleSecretRedactors = []*regexp.Regexp{
	// Authorization header: header name + (optional "Bearer ") + value.
	regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*)(?:bearer\s+)?[^\s"',}]+`),
	// Standalone "Bearer <token>".
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`),
}

// bundleURLPattern finds absolute-URL-shaped substrings in free text so
// redactURLCredentials can rewrite each one. The excluded characters stop a
// match at whitespace or a quote, which is where a URL ends in a log line or a
// YAML scalar.
var bundleURLPattern = regexp.MustCompile("(?i)\\b[a-z][a-z0-9+.-]*://[^\\s\"'`<>]+")

// redactURLCredentials removes credential material from every URL in s:
// the userinfo component, and the VALUE of every query and fragment parameter.
//
// # Why every parameter, and not a list of credential-ish names
//
// The original rule redacted a fixed set of parameter names
// (token/access_token/api_key/apikey/key). That is a deny-list, and it fails
// OPEN: `?password=` and `?client_secret=` sailed straight through it, and so
// would whatever name the next integration invents. Widening the vocabulary
// only moves the boundary — it never closes it.
//
// So the value of EVERY parameter goes, with no vocabulary at all. Two
// measurements say the cost is close to zero:
//
//   - A real effective-config snapshot contains no query parameters whatsoever.
//     Persisted URLs are endpoints, and provider.NormalizeEmbedBaseURL already
//     strips `RawQuery` before an endpoint is ever recorded.
//   - dir2mcp does not build query-parameter URLs of its own, so server.log and
//     the client bridge logs have essentially none to lose either.
//
// This also matches what the repo already does everywhere else it hands a URL
// to something that logs or persists it: avutil.redactInput and
// provider.NormalizeEmbedBaseURL both clear User, RawQuery and Fragment
// wholesale rather than inspecting names. This is the same decision, one step
// less destructive: parameter NAMES survive, because a name is not a credential
// and "there was a password parameter here" is exactly the kind of thing a
// maintainer reading a bundle needs to see.
//
// The whole userinfo goes, not just the password: for an S3-compatible endpoint
// the *username* is the access key ID, which is credential material in its own
// right.
//
// Rewriting is textual surgery on the matched substring rather than a
// url.Parse/String round-trip, so a log line is never silently re-encoded or
// normalized on its way into the bundle.
func redactURLCredentials(s string) string {
	return bundleURLPattern.ReplaceAllStringFunc(s, func(raw string) string {
		// Trailing sentence punctuation after a URL in prose is not part of it,
		// so it is trimmed off and re-appended. That is only safe when nothing
		// at the end of the URL is being redacted: for a URL with parameters the
		// trailing run would be the tail of the last parameter's VALUE, and
		// re-appending it would leak the final characters of a credential. So a
		// URL carrying a query or fragment keeps the punctuation inside the
		// redacted value instead (cosmetic loss, no leak).
		url, suffix := raw, ""
		if !strings.ContainsAny(raw, "?#") {
			url = strings.TrimRight(raw, ".,;:!)]}")
			suffix = raw[len(url):]
		}
		rest, fragment, hasFragment := strings.Cut(url, "#")
		rest, query, hasQuery := strings.Cut(rest, "?")

		out := redactURLUserinfo(rest)
		if hasQuery {
			out += "?" + redactParamValues(query)
		}
		if hasFragment {
			out += "#" + redactParamValues(fragment)
		}
		return out + suffix
	})
}

// redactURLUserinfo replaces the `user:pass@` component of scheme://authority.
// An `@` later in the path is not userinfo and is left alone.
func redactURLUserinfo(schemeAndPath string) string {
	sep := strings.Index(schemeAndPath, "://")
	if sep < 0 {
		return schemeAndPath
	}
	head := schemeAndPath[:sep+3]
	tail := schemeAndPath[sep+3:]
	authority := tail
	if slash := strings.Index(tail, "/"); slash >= 0 {
		authority = tail[:slash]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return schemeAndPath
	}
	return head + "[REDACTED]" + tail[at:]
}

// redactParamValues blanks the value of every `name=value` pair in a query or
// fragment, keeping the names. A component with no `=` (a plain `#anchor`)
// carries no value and is returned untouched.
func redactParamValues(component string) string {
	pairs := strings.Split(component, "&")
	for i, pair := range pairs {
		name, _, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		pairs[i] = name + "=[REDACTED]"
	}
	return strings.Join(pairs, "&")
}

// redactBundleSecrets masks credential material in text destined for the support
// bundle. The bundle is meant to be shareable with a maintainer, so leaking a
// bearer token would defeat the purpose.
//
// This is the bundle's TIER-1 filter: it removes credentials, and it runs over
// every artifact in every mode. --include-content never disables it, because
// consenting to disclose your own corpus is not consenting to disclose a
// password.
func redactBundleSecrets(s string) string {
	out := s
	out = bundleSecretRedactors[0].ReplaceAllString(out, "${1}[REDACTED]")
	out = bundleSecretRedactors[1].ReplaceAllString(out, "Bearer [REDACTED]")
	return redactURLCredentials(out)
}

// claudeMCPLogDirs returns the OS-specific directories where Claude Desktop
// writes its MCP server/bridge logs. Returns nil when the location cannot be
// resolved (the caller then collects nothing — best-effort).
func claudeMCPLogDirs() []string {
	switch runtime.GOOS {
	case "windows":
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			return []string{filepath.Join(appData, "Claude", "logs")}
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return []string{filepath.Join(home, "Library", "Logs", "Claude")}
		}
	default: // linux and other unixes
		if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
			return []string{filepath.Join(home, ".config", "Claude", "logs")}
		}
	}
	return nil
}

// collectClientMCPLogs gathers Claude Desktop's MCP logs (mcp*.log) best-effort,
// tail-capped and secret-redacted, under client-logs/ in the bundle. Missing or
// unreadable logs are silently skipped — this is a diagnostic aid, not a
// required artifact, so it never produces warnings or errors.
func collectClientMCPLogs() []supportFile {
	var out []supportFile
	for _, dir := range claudeMCPLogDirs() {
		matches, err := filepath.Glob(filepath.Join(dir, "mcp*.log"))
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, match := range matches {
			data, readErr := readLogTail(match, supportLogTailBytes)
			if readErr != nil || len(data) == 0 {
				continue
			}
			out = append(out, supportFile{
				Name:  "client-logs/" + filepath.Base(match),
				Bytes: []byte(redactBundleSecrets(string(data))),
			})
		}
	}
	return out
}

// marshalDaemonLivenessJSON records whether the dir2mcp daemon the client
// connects to is configured and reachable. This disambiguates "the daemon is
// down / the bridge can't reach it" (empty server.log, unreachable) from "the
// daemon is up but a handler failed" (populated server.log). The connection URL
// is redacted and only header *names* are recorded — never their values.
func marshalDaemonLivenessJSON(cfg config.Config) ([]byte, error) {
	info := map[string]interface{}{
		"connection_present": false,
		"reachable":          false,
	}

	raw, err := readFileBest(connectionFilePath(cfg.StateDir))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return json.MarshalIndent(info, "", "  ")
	}
	info["connection_present"] = true

	var conn connectionPayload
	if jerr := json.Unmarshal(raw, &conn); jerr != nil {
		info["parse_error"] = "connection.json present but could not be parsed"
		return json.MarshalIndent(info, "", "  ")
	}
	info["transport"] = conn.Transport
	info["public"] = conn.Public
	info["token_source"] = conn.TokenSource
	info["url"] = redactBundleSecrets(conn.URL)
	if len(conn.Headers) > 0 {
		keys := make([]string, 0, len(conn.Headers))
		for k := range conn.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		info["header_keys"] = keys
	}

	if u, perr := url.Parse(strings.TrimSpace(conn.URL)); perr == nil && u.Hostname() != "" {
		host := u.Host
		if u.Port() == "" {
			port := "80"
			if u.Scheme == "https" {
				port = "443"
			}
			host = net.JoinHostPort(u.Hostname(), port)
		}
		if c, derr := net.DialTimeout("tcp", host, 1500*time.Millisecond); derr == nil {
			_ = c.Close()
			info["reachable"] = true
		} else {
			info["reachable_error"] = derr.Error()
		}
	}

	return json.MarshalIndent(info, "", "  ")
}

func warningStrings(errs []error) []string {
	if len(errs) == 0 {
		return nil
	}
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}
