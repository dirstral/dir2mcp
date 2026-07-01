package cli

import (
	"archive/tar"
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
)

// supportLogTailBytes caps how much of server.log is included in the
// bundle. Keeps bundles shareable over chat and email while still
// capturing a meaningful window of recent activity.
const supportLogTailBytes int64 = 2 * 1024 * 1024 // 2 MB

// runSupportBundle implements `dir2mcp support-bundle`: it gathers a
// small set of files (effective config snapshot, server log tail,
// status JSON, list-files JSON, version, OS info, routing decisions)
// into a single tar.gz suitable for sharing with a maintainer when
// diagnosing install/indexing problems. No secret values are included
// — the effective config snapshot only carries `secret_sources:`
// metadata (where each credential came from), not the credentials
// themselves; server.log and the client bridge logs are run through the
// secret redactor; and list-files.json redacts corpus paths/titles/error
// messages unless the operator opts in with --include-content.
func (a *App) runSupportBundle(ctx context.Context, global globalOptions, args []string) int {
	fs := flag.NewFlagSet("support-bundle", flag.ContinueOnError)
	// Route flag-parse output through writeCLIError so JSON callers get
	// a single structured error object on stdout/stderr instead of the
	// flag package's prose mixed with our JSON envelope.
	fs.SetOutput(io.Discard)
	output := fs.String("output", "", "destination tar.gz path (default: ./dir2mcp-support-<timestamp>.tar.gz)")
	includeContent := fs.Bool("include-content", false,
		"include corpus paths/titles/extraction error messages in list-files.json (may disclose corpus content — review the bundle before sharing)")
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
		for _, w := range warnings {
			_, _ = fmt.Fprintf(a.stderr, "warning: %v\n", w)
		}
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

	cfgBytes, cfgErr := readFileBest(config.EffectiveSnapshotPath(cfg.StateDir))
	add("config.snapshot.yaml", cfgBytes, cfgErr)

	// server.log is redirected daemon stdout/stderr and can carry query text,
	// answer snippets, and bearer tokens. Redact it exactly like the client
	// bridge logs before it enters the shareable bundle.
	logBytes, logErr := readLogTail(serverLogPath(cfg.StateDir), supportLogTailBytes)
	if logErr == nil && len(logBytes) > 0 {
		logBytes = []byte(redactBundleSecrets(string(logBytes)))
	}
	add("server.log", logBytes, logErr)

	statusBytes, statusErr := marshalStatusJSON(ctx, a, cfg)
	add("status.json", statusBytes, statusErr)

	listBytes, listErr := marshalListFilesJSON(ctx, a, cfg, includeContent)
	add("list-files.json", listBytes, listErr)
	if listErr == nil && includeContent {
		warnings = append(warnings, errors.New(
			"list-files.json: --include-content embedded corpus paths/titles/error messages; review the bundle before sharing it"))
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
// the bundle still records "we tried and found nothing".
func marshalStatusJSON(ctx context.Context, a *App, cfg config.Config) ([]byte, error) {
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
	return json.MarshalIndent(map[string]interface{}{
		"source":    source,
		"state_dir": cfg.StateDir,
		"snapshot":  snapshot,
	}, "", "  ")
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
	if ext := filepath.Ext(relPath); ext != "" {
		return "[redacted]" + ext
	}
	return "[redacted]"
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

// writeSupportBundle writes files as a single gzip-compressed tarball
// at destPath. Directories in destPath must already exist. Mode 0o600
// keeps the bundle owner-readable only — its contents include the
// server log and diagnostic metadata that should not be exposed to
// other local users by an unfavourable umask.
func writeSupportBundle(destPath string, files []supportFile) error {
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	now := time.Now()
	for _, file := range files {
		hdr := &tar.Header{
			Name:    file.Name,
			Mode:    0o644,
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
	return nil
}

func supportEntryNames(files []supportFile) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	return names
}

// bundleSecretRedactors mask credential material that can appear in client
// bridge logs and connection URLs (bearer tokens, Authorization headers, and
// token-style query parameters) before they are written into the shared bundle.
var bundleSecretRedactors = []*regexp.Regexp{
	// Authorization header: header name + (optional "Bearer ") + value.
	regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*)(?:bearer\s+)?[^\s"',}]+`),
	// Standalone "Bearer <token>".
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`),
	// token-style query parameters (?token=..., &access_token=..., &api_key=...).
	regexp.MustCompile(`(?i)([?&](?:token|access_token|api_key|apikey|key)=)[^&\s"']+`),
}

// redactBundleSecrets masks credential material in text destined for the support
// bundle. It is intentionally conservative (pattern-based) — the bundle is meant
// to be shareable with a maintainer, so leaking a bearer token would defeat the
// purpose.
func redactBundleSecrets(s string) string {
	out := s
	out = bundleSecretRedactors[0].ReplaceAllString(out, "${1}[REDACTED]")
	out = bundleSecretRedactors[1].ReplaceAllString(out, "Bearer [REDACTED]")
	out = bundleSecretRedactors[2].ReplaceAllString(out, "${1}[REDACTED]")
	return out
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
