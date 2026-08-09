package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/buildinfo"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/statefs"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/usage"
)

const (
	exitSuccess        = iota // 0
	exitGeneric               // 1
	exitConfigInvalid         // 2
	exitIngestionFatal        // 3
	exitServerBindFailure
	exitAuthOrPayment
	exitSignalInterrupt
)

const (
	// Compatibility aliases retained for existing call sites.
	exitRootInaccessible = exitConfigInvalid
	exitIndexLoadFailure = exitGeneric
)

const (
	authTokenEnvVar = "DIR2MCP_AUTH_TOKEN"
	// environment variable for the x402 facilitator bearer token; CLI honors
	// this value (but it is *not* persisted to disk) in addition to flags.
	x402FacilitatorTokenEnvVar = "DIR2MCP_X402_FACILITATOR_TOKEN"
	connectionFileName         = "connection.json"
	secretTokenName            = "secret.token"
)

var commands = map[string]struct{}{
	"up":             {},
	"down":           {},
	"status":         {},
	"ask":            {},
	"search":         {},
	"open-file":      {},
	"list-files":     {},
	"reindex":        {},
	"embed-worker":   {},
	"export":         {},
	"bridge":         {},
	"config":         {},
	"install":        {},
	"uninstall":      {},
	"doctor":         {},
	"print-config":   {},
	"support-bundle": {},
	"service":        {},
	"version":        {},
}

type App struct {
	stdout io.Writer
	stderr io.Writer

	newIngestor  func(config.Config, model.Store) (model.Ingestor, error)
	newStore     func(config.Config) model.Store
	newRetriever func(config.Config, model.Store) model.Retriever
	// newIndex replaces the local index.backend dispatch for one kind ("text"
	// or "code"), returning the index and the path it persists to. Nil in
	// production; tests set it via RuntimeHooks.NewIndex.
	newIndex func(config.Config, string) (model.Index, string)

	cachedStyles map[bool]*styles

	// serverGracefulStop is set once the long-running server (up
	// --foreground / a supervised service) has begun serving, so a
	// subsequent signal-triggered shutdown is recognized as a normal,
	// successful termination rather than an interrupted command. See
	// resolveProcessExitCode (#434).
	serverGracefulStop bool
}

type indexingStateAware interface {
	SetIndexingState(state *appstate.IndexingState)
}

type documentDeleteNotifier interface {
	SetOnDocumentsDeleted(fn func(relPaths []string))
}

type documentErrorNotifier interface {
	SetOnDocumentError(fn func(relPath, docType, message string))
}

type documentSkipNotifier interface {
	SetOnDocumentSkip(fn func(relPath, docType, reason string))
}

type contentHashResetter interface {
	ClearDocumentContentHashes(ctx context.Context) error
}

// contentHashBackuper lets a reindex snapshot the content-hash gate before
// clearing it and restore it if the rebuild is interrupted or fails, so an
// aborted reindex does not force a full-corpus reprocess on the next sync
// (issue #418). Optional capability: stores without it degrade gracefully (the
// clear simply is not unwound).
type contentHashBackuper interface {
	BackupContentHashes(ctx context.Context) error
	RestoreContentHashes(ctx context.Context) error
	DiscardContentHashBackup(ctx context.Context) error
}

type embeddedChunkLister interface {
	ListEmbeddedChunkMetadata(ctx context.Context, indexKind string, limit int, afterChunkID int64) ([]model.ChunkTask, error)
}

type activeDocCountStore interface {
	ActiveDocCounts(ctx context.Context) (map[string]int64, int64, error)
}

type corpusStatsStore interface {
	CorpusStats(ctx context.Context) (model.CorpusStats, error)
}

type RuntimeHooks struct {
	NewIngestor  func(config.Config, model.Store) (model.Ingestor, error)
	NewStore     func(config.Config) model.Store
	NewRetriever func(config.Config, model.Store) model.Retriever
	// NewIndex replaces the local index.backend dispatch, returning the index
	// for one kind ("text" or "code") and the path it persists to. It exists so
	// tests can inject an instrumented index (the concurrent startup load of
	// issue #429 F6 is otherwise unobservable from outside the package, and
	// AGENTS.md keeps new tests under tests/). When set it also bypasses the
	// networked qdrant/pgvector branches. It is called once per kind, from two
	// goroutines at once, so an implementation must be safe for concurrent use.
	NewIndex func(config.Config, string) (model.Index, string)
}

type globalOptions struct {
	jsonOutput     bool
	nonInteractive bool
	rootDir        string
	configPath     string
	stateDir       string
	quiet          bool
}

type upOptions struct {
	globalOptions
	readOnly      bool
	public        bool
	forceInsecure bool
	// foreground keeps the up process attached to the terminal — the
	// existing pre-daemonization behavior. Implied by --json so NDJSON
	// event streams keep flowing to the calling process.
	foreground bool
	// daemon forces daemon mode even when stdout is not a TTY. Without
	// it, non-TTY callers (tests, CI scripts) get the foreground path so
	// startup errors land on stderr inline instead of inside the daemon
	// log file.
	daemon             bool
	x402Mode           string
	x402FacilitatorURL string
	// token values may come from a flag, environment variable, or file path
	x402FacilitatorToken     string
	x402FacilitatorTokenFile string
	// original direct token flag presence; true when the user supplied
	// --x402-facilitator-token and it was non-empty before any precedence
	// logic cleared it in favor of a file path.
	x402FacilitatorTokenDirectSet bool
	x402ResourceBaseURL           string
	x402Network                   string
	x402Price                     string
	x402Scheme                    string
	x402Asset                     string
	x402PayTo                     string
	x402ToolsCallEnabled          bool
	x402ToolsCallEnabledIsSet     bool
	auth                          string
	listen                        string
	mcpPath                       string
	tlsCert                       string
	tlsKey                        string
	allowedOrigins                string
}

type optionalBoolFlag struct {
	value bool
	set   bool
}

// String renders the flag's current boolean value, returning "false" for a
// nil receiver.
func (o *optionalBoolFlag) String() string {
	if o == nil {
		return "false"
	}
	return strconv.FormatBool(o.value)
}

// Set parses s as a boolean, records the value, and marks the flag as
// explicitly set.
func (o *optionalBoolFlag) Set(s string) error {
	parsed, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	o.value = parsed
	o.set = true
	return nil
}

// IsBoolFlag reports that this flag may be used without an explicit value,
// allowing the bare --flag form.
func (o *optionalBoolFlag) IsBoolFlag() bool {
	return true
}

type askOptions struct {
	question   string
	k          int
	mode       string
	index      string
	pathPrefix string
	fileGlob   string
	docTypes   []string
}

type authMaterial struct {
	mode              string
	token             string
	tokenSource       string
	tokenFile         string
	authorizationHint string
}

type connectionSession struct {
	UsesMCPSessionID     bool   `json:"uses_mcp_session_id"`
	HeaderName           string `json:"header_name"`
	AssignedOnInitialize bool   `json:"assigned_on_initialize"`
}

type connectionPayload struct {
	// FormatVersion is the df-000 cross-version signal (SPEC §1.3/§4.3, #468); it
	// MUST be present so a reader can reject an incompatible connection.json shape
	// before connecting. First field to mirror the spec example.
	FormatVersion string            `json:"format_version"`
	Transport     string            `json:"transport"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	Session       connectionSession `json:"session"`
	Public        bool              `json:"public"`
	TokenSource   string            `json:"token_source"`
	TokenFile     string            `json:"token_file,omitempty"`
}

type ndjsonEvent struct {
	Timestamp string      `json:"ts"`
	Level     string      `json:"level"`
	Event     string      `json:"event"`
	Data      interface{} `json:"data"`
}

type ndjsonEmitter struct {
	enabled bool
	// mu serializes writes to out. A single emitter is the shared sink for
	// concurrent ask/search query_metrics events and the corpus writer;
	// without synchronization their lines (which can exceed PIPE_BUF) may
	// interleave and corrupt server.log.
	mu  sync.Mutex
	out io.Writer
}

type cliErrorPayload struct {
	Error    cliError `json:"error"`
	ExitCode int      `json:"exit_code"`
}

type cliError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Hints   []string `json:"hints,omitempty"`
}

// corpusSnapshot is a point-in-time summary of the indexed corpus written to
// corpus.json in the state directory. See corpusIndexing for field semantics,
// including the sentinel value used for unavailable counters.
type corpusSnapshot struct {
	Timestamp    string           `json:"ts"`
	Indexing     corpusIndexing   `json:"indexing"`
	DocCounts    map[string]int64 `json:"doc_counts"`
	TotalDocs    int64            `json:"total_docs"`
	CodeRatio    float64          `json:"code_ratio"`
	CacheableFor string           `json:"cacheable_for,omitempty"`
}

// corpusIndexing holds indexing progress counters. Representations,
// ChunksTotal, and EmbeddedOK carry -1 when not available — for example on
// the ListFiles-only fallback path where those metrics cannot be derived from
// the store. Consumers should treat -1 as "unknown", not as an error.
type corpusIndexing struct {
	Mode            string                `json:"mode"`
	Running         bool                  `json:"running"`
	Scanned         int64                 `json:"scanned"`
	Indexed         int64                 `json:"indexed"`
	Skipped         int64                 `json:"skipped"`
	Deleted         int64                 `json:"deleted"`
	Representations int64                 `json:"representations"`
	ChunksTotal     int64                 `json:"chunks_total"`
	EmbeddedOK      int64                 `json:"embedded_ok"`
	EmbeddedPending int64                 `json:"embedded_pending"`
	Errors          int64                 `json:"errors"`
	Unknown         int64                 `json:"unknown"`
	FailureSummary  *model.FailureSummary `json:"failure_summary,omitempty"`
	// SkipSummary groups never-indexed documents by skip_reason (honest
	// coverage, #414/#395). Travels straight through from CorpusStats; omitted
	// from JSON when nothing was skipped.
	SkipSummary *model.SkipSummary `json:"skip_summary,omitempty"`
	// WatchOverflows is the process-lifetime count of fsnotify kernel
	// event-buffer overflows (#591). It is a pointer so it is emitted (even as
	// 0) only when a watcher is running and omitted otherwise — absence means
	// "not applicable" (one-shot index), not zero (spec 0.33.0 / stats.json).
	WatchOverflows *int64 `json:"watch_overflows,omitempty"`
}

// NewApp constructs an App wired to os.Stdout and os.Stderr.
func NewApp() *App {
	return NewAppWithIO(os.Stdout, os.Stderr)
}

// NewAppWithIO constructs an App with the given output writers and the
// default ingestor/store constructors.
func NewAppWithIO(stdout, stderr io.Writer) *App {
	return &App{
		stdout: stdout,
		stderr: stderr,
		newIngestor: func(cfg config.Config, st model.Store) (model.Ingestor, error) {
			svc, err := ingest.NewService(cfg, st)
			if err != nil {
				return nil, err
			}
			if extractor := ingest.DocumentExtractorFromConfig(cfg); extractor != nil {
				svc.SetDocumentExtractor(extractor)
			}
			// #393: activate the capability-activated pandoc engine (T2) after the
			// primary extractor is wired. Under `auto` it runs alongside the primary;
			// under the `pandoc` pin it reuses the primary instance. Kept out of
			// NewService so it stays free of ambient-PATH probing.
			svc.ActivatePandocEngine(cfg)
			return svc, nil
		},
		// default store constructor uses sqlite in the configured state
		// directory.  tests can override via RuntimeHooks.NewStore.
		newStore: func(cfg config.Config) model.Store {
			return store.NewSQLiteStore(filepath.Join(cfg.StateDir, "meta.sqlite"))
		},
	}
}

// NewAppWithIOAndHooks constructs an App with the given writers, overriding
// the default ingestor/store/retriever constructors with any non-nil hooks
// (used by tests to inject fakes).
func NewAppWithIOAndHooks(stdout, stderr io.Writer, hooks RuntimeHooks) *App {
	app := NewAppWithIO(stdout, stderr)
	if hooks.NewIngestor != nil {
		app.newIngestor = hooks.NewIngestor
	}
	if hooks.NewStore != nil {
		app.newStore = hooks.NewStore
	}
	if hooks.NewRetriever != nil {
		app.newRetriever = hooks.NewRetriever
	}
	if hooks.NewIndex != nil {
		app.newIndex = hooks.NewIndex
	}
	return app
}

// writef writes a formatted string to out, discarding any write error.
func writef(out io.Writer, format string, args ...interface{}) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// writeln writes args followed by a newline to out, discarding any write
// error.
func writeln(out io.Writer, args ...interface{}) {
	_, _ = fmt.Fprintln(out, args...)
}

// sty returns the cached styles instance, creating one on first call.
// Pass jsonMode=true to disable colors even when stdout is a TTY.
func (a *App) sty(jsonMode bool) styles {
	if a.cachedStyles != nil {
		if cached, ok := a.cachedStyles[jsonMode]; ok && cached != nil {
			return *cached
		}
	}
	if a.cachedStyles == nil {
		a.cachedStyles = make(map[bool]*styles, 2)
	}
	s := newStyles(a.stdout, jsonMode)
	a.cachedStyles[jsonMode] = &s
	return s
}

// storeForConfig returns the metadata store for cfg, using the injected
// newStore hook when present and otherwise the default sqlite store in the
// state directory.
func (a *App) storeForConfig(cfg config.Config) model.Store {
	if a != nil && a.newStore != nil {
		return a.newStore(cfg)
	}
	return store.NewSQLiteStore(filepath.Join(cfg.StateDir, "meta.sqlite"))
}

// Run is the process entrypoint: it installs a signal-cancelled context,
// dispatches args, and maps a clean exit after interruption to the
// signal-interrupt exit code.
func (a *App) Run(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code := a.RunWithContext(ctx, args)
	return resolveProcessExitCode(ctx.Err(), code, a.serverGracefulStop)
}

// resolveProcessExitCode maps a clean (exitSuccess) return that coincided
// with a context cancellation to the interrupt exit code — EXCEPT when the
// cancellation gracefully stopped the long-running server (up --foreground
// or a supervised launchd/systemd service). A signal-triggered stop of the
// server is a normal, successful termination and must exit 0, so launchd
// (KeepAlive SuccessfulExit=false) and systemd (Restart=on-failure) do not
// mistake a user- or supervisor-requested `down` for a crash and respawn it
// (#434). A crash still returns its non-zero code untouched.
func resolveProcessExitCode(ctxErr error, code int, serverGracefulStop bool) int {
	if errors.Is(ctxErr, context.Canceled) && code == exitSuccess && !serverGracefulStop {
		return exitSignalInterrupt
	}
	return code
}

// RunWithContext parses global flags and dispatches the remaining command
// under ctx, printing usage when no command is given.
func (a *App) RunWithContext(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printUsage()
		return exitSuccess
	}
	jsonRequested := argsContainJSONFlag(args)

	globalOpts, remaining, err := parseGlobalOptions(args)
	if err != nil {
		writeCLIError(a.stderr, jsonRequested, exitConfigInvalid, err.Error())
		return exitConfigInvalid
	}
	if len(remaining) == 0 {
		a.printUsage()
		return exitSuccess
	}

	return a.runCommand(ctx, globalOpts, remaining, jsonRequested)
}

// runCommand validates the command name against the canonical command
// surface and dispatches it, handling the up/down/version paths directly and
// delegating the rest to runSimpleCommand.
func (a *App) runCommand(ctx context.Context, globalOpts globalOptions, remaining []string, jsonRequested bool) int {
	command := remaining[0]
	// Validate against the canonical command surface before dispatch. The
	// commands map is the single source of truth that the repo-split
	// boundary test (tests/security) AST-parses to verify the surface stays
	// intact; the explicit lookup here keeps it referenced so go vet / the
	// unused-variable lint stays happy after parseGlobalOptions stopped
	// consulting it directly.
	if _, known := commands[command]; !known {
		effectiveJSON := globalOpts.jsonOutput || jsonRequested
		writeCLIError(a.stderr, effectiveJSON, exitGeneric, fmt.Sprintf("unknown command: %s", command))
		if !effectiveJSON {
			a.printUsage()
		}
		return exitGeneric
	}
	if code, handled := a.runSimpleCommand(ctx, globalOpts, command, remaining[1:]); handled {
		return code
	}

	switch command {
	case "up":
		upOpts, parseErr := parseUpOptions(globalOpts, remaining[1:])
		if parseErr != nil {
			writeCLIError(a.stderr, globalOpts.jsonOutput || argsContainJSONFlag(remaining[1:]), exitConfigInvalid, fmt.Sprintf("invalid up flags: %v", parseErr))
			return exitConfigInvalid
		}
		return a.runUp(ctx, upOpts)
	case "down":
		return a.runDown(ctx, globalOpts, remaining[1:])
	case "version":
		// version takes no operands. Reject extra arguments so a typo or a
		// concatenated command does not look like a success (#706).
		if extra := remaining[1:]; len(extra) > 0 {
			writeCLIError(a.stderr, globalOpts.jsonOutput || jsonRequested, exitConfigInvalid,
				fmt.Sprintf("version command does not accept arguments: %s", strings.Join(extra, " ")))
			return exitConfigInvalid
		}
		if !globalOpts.quiet {
			writeln(a.stdout, "dir2mcp "+buildinfo.Display())
		}
		return exitSuccess
	default:
		effectiveJSON := globalOpts.jsonOutput || jsonRequested
		writeCLIError(a.stderr, effectiveJSON, exitGeneric, fmt.Sprintf("unknown command: %s", remaining[0]))
		if !effectiveJSON {
			a.printUsage()
		}
		return exitGeneric
	}
}

// runSimpleCommand dispatches the non-up/down commands (status, reindex,
// config, bridge, install, uninstall, doctor, print-config, support-bundle,
// service) plus the legacy shims, returning handled=false when command is
// none of these.
func (a *App) runSimpleCommand(ctx context.Context, globalOpts globalOptions, command string, args []string) (int, bool) {
	if code, handled := a.runLegacyShimCommand(ctx, globalOpts, command, args); handled {
		return code, true
	}

	switch command {
	case "status":
		return a.runStatus(ctx, globalOpts, args), true
	case "reindex":
		return a.runReindex(ctx, globalOpts, args), true
	case "embed-worker":
		return a.runEmbedWorker(ctx, globalOpts, args), true
	case "export":
		return a.runExport(ctx, globalOpts, args), true
	case "config":
		return a.runConfig(ctx, globalOpts, args), true
	case "bridge":
		return a.runBridge(ctx, globalOpts, args), true
	case "install":
		return a.runInstall(ctx, globalOpts, args), true
	case "uninstall":
		return a.runUninstall(ctx, globalOpts, args), true
	case "doctor":
		return a.runDoctor(ctx, globalOpts, args), true
	case "print-config":
		return a.runPrintConfig(ctx, globalOpts, args), true
	case "support-bundle":
		return a.runSupportBundle(ctx, globalOpts, args), true
	case "service":
		return a.runService(ctx, globalOpts, args), true
	default:
		return 0, false
	}
}

// runLegacyShimCommand dispatches the legacy ask/search/open-file/list-files
// compatibility shims, returning handled=false when command is none of them.
func (a *App) runLegacyShimCommand(ctx context.Context, globalOpts globalOptions, command string, args []string) (int, bool) {
	switch command {
	case "ask":
		return a.runAsk(ctx, globalOpts, args), true
	case "search":
		return a.runSearchRemote(ctx, globalOpts, args), true
	case "open-file":
		return a.runOpenFileRemote(ctx, globalOpts, args), true
	case "list-files":
		return a.runListFilesRemote(ctx, globalOpts, args), true
	default:
		return 0, false
	}
}

// printUsage writes the styled top-level help text (commands and flag
// groups) to stdout.
func (a *App) printUsage() {
	s := a.sty(false)
	o := a.stdout

	writeln(o, s.Brand.Render("dir2mcp")+" "+s.Dim.Render("· deploy a directory as an MCP knowledge server"))
	writeln(o)

	writeln(o, s.sectionHeader("Usage"))
	writeln(o, "  dir2mcp [global flags] <command> [command flags]")
	writeln(o)

	writeln(o, s.sectionHeader("Commands"))
	cmds := [][2]string{
		{"up", "start the MCP server and begin indexing (daemonizes by default)"},
		{"down", "stop the dir2mcp server running in this directory"},
		{"status", "show server health and corpus stats"},
		{"ask", "legacy compatibility shim; prefer dirstral-cli for client UX"},
		{"search", "legacy compatibility shim; prefer dirstral-cli for client UX"},
		{"open-file", "legacy compatibility shim; prefer dirstral-cli for client UX"},
		{"list-files", "legacy compatibility shim; prefer dirstral-cli for client UX"},
		{"reindex", "force a full re-index of all documents"},
		{"embed-worker", "run a standalone distributed embed worker (no MCP serving; requires Tier-C store + broker)"},
		{"export", "render a transcript as VTT/SRT/TTML subtitles (export --format vtt|srt|ttml <path>)"},
		{"bridge", "run helper adapters (for example ElevenLabs webhooks)"},
		{"config", "view or edit configuration"},
		{"install", "install dir2mcp into a client (e.g. dir2mcp install claude)"},
		{"uninstall", "remove dir2mcp from a client (e.g. dir2mcp uninstall claude)"},
		{"doctor", "run client integration diagnostics (e.g. dir2mcp doctor claude)"},
		{"print-config", "print the MCP-server JSON snippet for a client"},
		{"support-bundle", "collect logs + config + status into a shareable tar.gz"},
		{"service", "auto-start the daemon at login (macOS launchd): install|uninstall|status"},
		{"version", "print build version"},
	}
	for _, c := range cmds {
		writef(o, "  %-13s %s\n", s.Bold.Render(c[0]), s.Dim.Render(c[1]))
	}
	writeln(o)

	writeln(o, s.sectionHeader("Global Flags"))
	globals := [][2]string{
		{"--dir <path>", "root directory to serve (default: current dir)"},
		{"--config <path>", "config file path"},
		{"--state-dir <path>", "state / cache directory"},
		{"--json", "output machine-readable JSON"},
		{"--non-interactive", "disable prompts and progress output"},
		{"--quiet", "suppress non-error output"},
	}
	for _, f := range globals {
		writef(o, "  %-26s %s\n", s.Accent.Render(f[0]), s.Dim.Render(f[1]))
	}
	writeln(o)

	writeln(o, s.sectionHeader("Server Flags")+" "+s.Dim.Render("(up)"))
	serverFlags := [][2]string{
		{"--listen <addr>", "listen address (default: 127.0.0.1:0)"},
		{"--mcp-path <path>", "HTTP route for the MCP endpoint"},
		{"--public", "bind to all interfaces (requires auth or --force-insecure)"},
		{"--read-only", "disable write operations"},
		{"--auth <token>", "bearer token (or set DIR2MCP_AUTH_TOKEN)"},
		{"--tls-cert / --tls-key", "TLS certificate and key files"},
		{"--allowed-origins <csv>", "CORS allowed origins"},
	}
	for _, f := range serverFlags {
		writef(o, "  %-30s %s\n", s.Accent.Render(f[0]), s.Dim.Render(f[1]))
	}
	writeln(o)

	writeln(o, s.sectionHeader("Reindex Flags")+" "+s.Dim.Render("(reindex)"))
	reindexFlags := [][2]string{
		{"--embeddings-only", "retry chunks that failed to embed, without re-running extraction"},
		{"--error-category <csv>", "with --embeddings-only: which failure categories to retry"},
	}
	for _, f := range reindexFlags {
		writef(o, "  %-30s %s\n", s.Accent.Render(f[0]), s.Dim.Render(f[1]))
	}
	writeln(o)

	writeln(o, s.sectionHeader("Payment Flags")+" "+s.Dim.Render("(up, x402)"))
	x402Flags := [][2]string{
		{"--x402 <mode>", "payment gating: off | on | required"},
		{"--x402-facilitator-url", "x402 facilitator endpoint"},
	}
	for _, f := range x402Flags {
		writef(o, "  %-30s %s\n", s.Accent.Render(f[0]), s.Dim.Render(f[1]))
	}
}

// saveEnvLocalKey upserts keyName=value into the dotenv-style file at path,
// replacing any existing assignment and writing the file 0o600.
func saveEnvLocalKey(path, keyName, value string) error {
	var existing []byte
	if _, statErr := os.Stat(path); statErr == nil {
		var err error
		existing, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	prefix := keyName + "="
	exportPrefix := "export " + keyName + "="
	lines := strings.Split(string(existing), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) && !strings.HasPrefix(line, exportPrefix) {
			out = append(out, line)
		}
	}
	// Trim trailing blank lines, then append the key assignment.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	out = append(out, prefix+value)
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// resolveModelClients builds the embed + chat adapters for the
// retrieval service through the provider resolver (SPEC 8.1.3) +
// providerfactory. It is **best-effort by design**: unresolved
// embed/chat yields a nil client so the server can still boot (e.g.
// read-only serving an existing index, where the embedder may never be
// exercised). The hard requirement is enforced once, in the §2.5
// startup preflight (checkMistralAPIKey + requiresMistralAPIKey), which
// already encodes the read-only/ingest nuances; a nil embedder later
// surfaces as ErrMissingEmbedder at query time (matching the previous
// keyless-client behavior).
// Returns the embedder, generator, and the resolved embed text/code
// model names (from the chosen embed profile) so the retrieval service
// and embedding workers use the right model for the selected provider
// instead of the hardcoded mistral defaults. Empty names mean "let the
// adapter use its provider default".
func (a *App) resolveModelClients(cfg config.Config) (model.Embedder, model.Generator, string, string) {
	r := cfg.Providers()
	var embedder model.Embedder
	var textModel, codeModel string
	if ep, err := r.Resolve(provider.CapEmbed); err == nil {
		if e, ferr := providerfactory.Embedder(ep); ferr == nil {
			embedder = e
			// Resolve the CONCRETE embed models the adapter will send (kind
			// default when the profile leaves them blank) so the ingest worker
			// and the query-side retrieval embedder use byte-identical model ids
			// instead of each independently relying on the adapter default while
			// retrieval keeps a stale mistral-embed fallback (issue #440 F2).
			textModel, codeModel = providerfactory.EffectiveEmbedModels(ep)
		}
	}
	var gen model.Generator
	if cp, err := r.Resolve(provider.CapChat); err == nil {
		// The chat model comes from the resolved profile
		// (providers:/model: config).
		if g, ferr := providerfactory.Generator(cp); ferr == nil {
			gen = g
		}
	}
	return embedder, gen, textModel, codeModel
}

// resolveChatModel returns the resolved chat/generation model name, used only
// to label and price the generate stage in per-query metrics (issue #327).
// Returns "" when no chat provider resolves.
func (a *App) resolveChatModel(cfg config.Config) string {
	cp, err := cfg.Providers().Resolve(provider.CapChat)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cp.ChatModel)
}

// configureQueryMetrics wires per-query cost/latency observability onto the
// retrieval service (issue #327): a structured query_metrics emitter, the
// configured price table, and the resolved generation model label. Additive
// and always-on; it never alters tool results. Shared by `up` and `ask`.
func (a *App) configureQueryMetrics(ret *retrieval.Service, cfg config.Config, emit func(level, event string, data interface{})) {
	if emit == nil {
		return
	}
	ret.SetGenerationModel(a.resolveChatModel(cfg))
	ret.SetMetricsEmitter(emit, usage.NewPriceTable(cfg.CostPriceOverrides))
	// Opt-in energy/CO2e estimate (issue #328); off unless carbon.enabled.
	ret.SetCarbonModel(usage.NewCarbonModel(
		cfg.Carbon.Enabled,
		cfg.Carbon.EnergyOverrides,
		cfg.Carbon.GridGramsCO2ePerWh,
	))
}

// rerankSelectorProfiles maps a non-default rerank.provider selector onto
// the built-in provider profile that serves it. `cohere` (and the empty
// selector) take the auto-resolution path instead and are intentionally
// absent. `colbert` is the self-hosted late-interaction backend
// (dir2mcp#337); it is reached by an explicit selector, like the whisper
// STT path, so it never wins rerank auto-selection over hosted cohere.
var rerankSelectorProfiles = map[string]string{
	"colbert": "colbert",
}

// resolveRerankProfile resolves the rerank provider profile for the legacy
// flat rerank.provider selector (SPEC 8.1.3). An empty selector or `cohere`
// uses auto-resolution; a known self-hosted selector (e.g. `colbert`)
// resolves its named profile explicitly. ok=false means "no rerank backend"
// (an unknown selector, or no eligible profile) — the caller fails open.
func resolveRerankProfile(cfg config.Config, asked bool) (provider.Profile, error, bool) {
	r := cfg.Providers()
	sel := strings.ToLower(strings.TrimSpace(cfg.RerankProvider))
	switch sel {
	case "", "cohere":
		p, err := r.Resolve(provider.CapRerank)
		return p, err, true
	default:
		name, known := rerankSelectorProfiles[sel]
		if !known {
			return provider.Profile{}, nil, false
		}
		p, err := r.ResolveExplicit(provider.CapRerank, name, asked)
		return p, err, true
	}
}

// configureReranker wires the optional rerank stage onto the retrieval
// service through the resolver (SPEC 8.1.3). The backend is either the
// hosted Cohere cross-encoder or a self-hosted late-interaction (ColBERT)
// endpoint (dir2mcp#337), selected by rerank.provider. cfg.RerankEnabled
// is a tri-state override:
//
//	nil    -> auto: on iff a rerank provider resolves
//	*false -> force off even when one resolves
//	*true  -> require it; warn + fail-open if none resolves
//
// Fail-open optimization, not a hard dependency. Shared by `up`/`ask`.
func (a *App) configureReranker(ret *retrieval.Service, cfg config.Config) {
	explicit := cfg.RerankEnabled != nil
	if explicit && !*cfg.RerankEnabled {
		return // explicit opt-out wins
	}
	asked := explicit && *cfg.RerankEnabled

	rp, err, ok := resolveRerankProfile(cfg, asked)
	if !ok {
		// Unrecognised rerank.provider selector: preserve the prior
		// "unsupported provider disables rerank" behavior.
		if asked {
			writef(a.stderr, "warning: rerank.provider %q unsupported; reranking disabled\n", cfg.RerankProvider)
		}
		return
	}
	if err != nil {
		// No eligible rerank provider (or an invalid explicit binding).
		// Auto mode stays silent; only warn when explicitly asked.
		if asked {
			writef(a.stderr, "warning: rerank.enabled is set but no rerank provider resolved (%v); reranking disabled\n", err)
		}
		return
	}
	rk, err := providerfactory.Reranker(rp)
	if err != nil {
		if asked {
			writef(a.stderr, "warning: rerank provider %q unusable (%v); reranking disabled\n", rp.Name, err)
		}
		return
	}
	ret.SetReranker(rk, rp.RerankModel, cfg.RerankCandidatePool)
	ret.SetRerankEnabled(true)
}

// openHNSWForAsk constructs a persisted HNSW index, restores it from its v2
// snapshot, and reconciles its recorded embed identity with the configured one
// (issue #247). On any error it closes the index and returns the error.
func openHNSWForAsk(ctx context.Context, path, identity string) (*index.HNSWIndex, error) {
	ix := index.NewHNSWIndex(path)
	if err := ix.Load(ctx, path); err != nil &&
		!errors.Is(err, model.ErrNotImplemented) &&
		!errors.Is(err, os.ErrNotExist) {
		_ = ix.Close()
		return nil, err
	}
	if err := index.EnsureIdentity(ctx, ix, identity); err != nil {
		_ = ix.Close()
		return nil, err
	}
	return ix, nil
}

// buildRetrieverForAsk constructs a retrieval service for the ask path:
// it loads the text/code HNSW indexes, resolves the embed/chat clients,
// wires reranking, and preloads embedded chunk metadata. It returns the
// retriever plus a cleanup func that closes the loaded indexes. The injected
// newRetriever hook short-circuits this when present.
func (a *App) buildRetrieverForAsk(ctx context.Context, cfg config.Config, st model.Store) (model.Retriever, func(), error) {
	if a != nil && a.newRetriever != nil {
		return a.newRetriever(cfg, st), nil, nil
	}

	// v2 snapshot filenames + per-index embed-identity reconciliation (issue
	// #247); see internal/index for the format rationale.
	identity := cfg.Providers().EmbedIdentity()
	textIx, err := openHNSWForAsk(ctx, filepath.Join(cfg.StateDir, index.TextIndexFileName), identity)
	if err != nil {
		return nil, nil, fmt.Errorf("load text index: %w", err)
	}
	codeIx, err := openHNSWForAsk(ctx, filepath.Join(cfg.StateDir, index.CodeIndexFileName), identity)
	if err != nil {
		_ = textIx.Close()
		return nil, nil, fmt.Errorf("load code index: %w", err)
	}

	embedder, generator, etm, ecm := a.resolveModelClients(cfg)
	ret := retrieval.NewService(st, textIx, embedder, generator)
	ret.SetQueryEmbeddingModel(etm)
	ret.SetCodeEmbeddingModel(ecm)
	ret.SetCodeIndex(codeIx)
	ret.SetRootDir(cfg.RootDir)
	ret.SetStateDir(cfg.StateDir)
	// Plumb ingest's ACTIVE OCR/transcript derivation identities into open_file's
	// cache lookup so it keys the OCR/transcript cache the SAME identity-aware way
	// ingest's writer does (issue #488). The ask path is read-only and builds no
	// ingest Service, so the identities are computed from the same cfg the ingestor
	// would use (byte-identical to a Service's getters).
	ocrIdentity, transcriptIdentity := ingest.ActiveDerivationIdentities(cfg)
	ret.SetDerivationCacheIdentities(ocrIdentity, transcriptIdentity)
	ret.SetProtocolVersion(cfg.ProtocolVersion)
	ret.SetRAGSystemPrompt(cfg.RAGSystemPrompt)
	ret.SetMaxContextChars(cfg.RAGMaxContextChars)
	ret.SetOversampleFactor(cfg.RAGOversampleFactor)
	a.configureReranker(ret, cfg)
	ret.SetMinScore(cfg.RetrievalMinScore)
	ret.SetRecencyHalfLife(cfg.RetrievalRecencyHalfLife)

	// Per-query cost/latency observability (issue #327). The ask CLI writes its
	// result (often JSON) to stdout, so route the query_metrics event to stderr
	// to avoid contaminating that output; it never alters the result itself.
	askMetricsEmitter := newNDJSONEmitter(a.stderr, true)
	a.configureQueryMetrics(ret, cfg, askMetricsEmitter.Emit)
	ret.SetContextCompression(cfg.ContextCompressionEnabled, cfg.ContextCompressionTargetRatio)
	ret.SetAdaptiveRetrieval(cfg.RetrievalAdaptiveEnabled, cfg.RetrievalAdaptiveKMin, cfg.RetrievalAdaptiveKMax)
	ret.SetMMR(cfg.RetrievalMMREnabled, cfg.RetrievalMMRLambda)
	ret.SetHyDE(cfg.RetrievalHyDEEnabled, cfg.RetrievalHyDEMode)
	// Hierarchical (coarse-to-fine) retrieval (SPEC §9.7): gates only the expand
	// step; summary hits are never citable regardless of this flag.
	ret.SetHierarchical(cfg.RetrievalHierarchicalEnabled)
	a.configureCrossLingual(ret, cfg, st, generator)

	if metadataStore, ok := st.(embeddedChunkLister); ok {
		if _, err := preloadEmbeddedChunkMetadata(ctx, metadataStore, ret); err != nil && !errors.Is(err, model.ErrNotImplemented) {
			_ = textIx.Close()
			_ = codeIx.Close()
			return nil, nil, fmt.Errorf("preload embedded chunk metadata: %w", err)
		}
	}

	// Route open_file / OCR reads through the corpus filesystem for object-store
	// backends so the ask CLI can read S3-backed documents too (#432). A build
	// failure is non-fatal here: local/NFS corpora do not need it and search/ask
	// citations never touch the corpus FS, so ask still functions.
	if sourceIsRemote(cfg) {
		if corpusFS, fsErr := buildCorpusFS(ctx, cfg); fsErr == nil {
			ret.SetCorpusFS(corpusFS)
		} else {
			// Non-fatal (search/ask never touch the corpus FS), but emit a
			// structured warning so the operator knows open_file text/OCR reads
			// will fall through to the local path and fail on this S3 corpus
			// even though search/ask still work (#432).
			askMetricsEmitter.Emit("warning", "corpus_fs_unavailable", map[string]interface{}{
				"error": fsErr.Error(),
			})
		}
	}

	cleanup := func() {
		_ = textIx.Close()
		_ = codeIx.Close()
	}
	return ret, cleanup, nil
}

// parseAskOptions parses the ask subcommand flags and trailing question,
// applying defaults and validating k, mode, index, and the doc-types filter.
func parseAskOptions(args []string) (askOptions, error) {
	opts := askOptions{
		k:     mcp.DefaultSearchK,
		mode:  "answer",
		index: "auto",
	}
	var rawDocTypes string

	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(
		&opts.k,
		"k",
		opts.k,
		fmt.Sprintf("number of results (<=0 defaults to %d, max %d)", mcp.DefaultSearchK, mcp.MaxSearchK),
	)
	fs.StringVar(&opts.mode, "mode", opts.mode, "answer|search_only")
	fs.StringVar(&opts.index, "index", opts.index, "auto|text|code|both")
	fs.StringVar(&opts.pathPrefix, "path-prefix", "", "optional path prefix filter")
	fs.StringVar(&opts.fileGlob, "file-glob", "", "optional file glob filter")
	fs.StringVar(&rawDocTypes, "doc-types", "", "comma-separated doc type filter")
	if err := fs.Parse(args); err != nil {
		return askOptions{}, err
	}

	opts.question = strings.TrimSpace(strings.Join(fs.Args(), " "))
	if opts.question == "" {
		return askOptions{}, errors.New("ask command requires a question argument")
	}
	if opts.k <= 0 {
		opts.k = mcp.DefaultSearchK
	}
	if opts.k > mcp.MaxSearchK {
		return askOptions{}, fmt.Errorf("k must be <= %d", mcp.MaxSearchK)
	}

	opts.mode = strings.ToLower(strings.TrimSpace(opts.mode))
	switch opts.mode {
	case "answer", "search_only":
	default:
		return askOptions{}, errors.New("mode must be one of answer,search_only")
	}

	opts.index = strings.ToLower(strings.TrimSpace(opts.index))
	switch opts.index {
	case "auto", "text", "code", "both":
	default:
		return askOptions{}, errors.New("index must be one of auto,text,code,both")
	}

	if trimmed := strings.TrimSpace(rawDocTypes); trimmed != "" {
		parts := strings.Split(trimmed, ",")
		opts.docTypes = make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			opts.docTypes = append(opts.docTypes, part)
		}
	}

	return opts, nil
}

// readCorpusSnapshot reads and JSON-decodes the corpus snapshot at path.
func readCorpusSnapshot(path string) (corpusSnapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return corpusSnapshot{}, err
	}
	var snapshot corpusSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return corpusSnapshot{}, err
	}
	return snapshot, nil
}

// emitJSON encodes payload as JSON to out without HTML escaping.
func emitJSON(out io.Writer, payload interface{}) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(payload)
}

// isJSONFlagEnabled reports whether arg is a --json/-json flag (bare or with
// a truthy =value form).
func isJSONFlagEnabled(arg string) bool {
	if arg == "--json" || arg == "-json" {
		return true
	}

	var raw string
	switch {
	case strings.HasPrefix(arg, "--json="):
		raw = strings.TrimPrefix(arg, "--json=")
	case strings.HasPrefix(arg, "-json="):
		raw = strings.TrimPrefix(arg, "-json=")
	default:
		return false
	}

	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	return err == nil && enabled
}

// argsContainJSONFlag reports whether any arg enables JSON output, used to
// format early errors before full flag parsing.
func argsContainJSONFlag(args []string) bool {
	for _, arg := range args {
		if isJSONFlagEnabled(arg) {
			return true
		}
	}
	return false
}

// exitCodeLabel maps an exit code to its stable machine-readable error code
// string used in JSON error payloads.
func exitCodeLabel(exitCode int) string {
	switch exitCode {
	case exitConfigInvalid:
		return "CONFIG_INVALID"
	case exitIngestionFatal:
		return "INGESTION_FATAL"
	case exitServerBindFailure:
		return "SERVER_BIND_FAILURE"
	case exitAuthOrPayment:
		return "AUTH_OR_PAYMENT"
	case exitSignalInterrupt:
		return "INTERRUPTED"
	default:
		return "GENERIC_ERROR"
	}
}

// writeCLIError writes an error to stderr, as a structured JSON payload when
// jsonOutput is set (with a hand-rolled fallback if encoding fails) or as
// plain text with optional hint lines otherwise. The JSON `code` field is
// derived from the exit code via exitCodeLabel; callers needing a specific
// canonical §14 code (e.g. BIND_FAILED, TLS_CONFIG_INVALID) use
// writeCLIErrorWithCode instead.
func writeCLIError(stderr io.Writer, jsonOutput bool, exitCode int, message string, hints ...string) {
	writeCLIErrorWithCode(stderr, jsonOutput, exitCode, exitCodeLabel(exitCode), message, hints...)
}

// emitConfigWarnings prints the non-fatal config-load diagnostics collected in
// Config.Warnings to stderr.
//
// Nothing consumed that field before (#628): warnings about a deprecated
// DIR2MCP_SESSION_TIMEOUT, an unparseable duration, or an unrecognized config
// key were appended and then silently discarded, so an operator whose config was
// partly ignored had no way to find out. Diagnostics go to stderr so they never
// contaminate --json stdout, and --quiet suppresses them like other non-error
// output.
func (a *App) emitConfigWarnings(cfg config.Config, quiet bool) {
	if quiet {
		return
	}
	for _, w := range cfg.Warnings {
		_, _ = fmt.Fprintf(a.stderr, "warning: %v\n", w)
	}
}

// writeCLIErrorWithCode is writeCLIError with an explicit machine-readable
// `code` for the JSON payload, decoupling the emitted error code from the
// process exit code. It lets a failure keep its canonical §14 code (e.g. a bind
// failure carries exit code exitServerBindFailure but code "BIND_FAILED") rather
// than the coarser exit-code label. The plain-text path is unaffected.
// writeStoreInitError reports a metadata-store Init failure, preserving any
// canonical §14 code the store attached to the error.
//
// Every Init call site funnelled failures through writeCLIError, which stamps
// the JSON `code` field from the exit code (GENERIC_ERROR). An index-format
// mismatch therefore reached machine consumers as GENERIC_ERROR even though the
// store raises a typed *store.IndexVersionMismatchError whose Code() is
// INDEX_VERSION_MISMATCH; the canonical code survived only as a prefix inside
// the human-readable message, which is the "CLI prose only" gap of #422 V6.
// BIND_FAILED and TLS_CONFIG_INVALID are wired through writeCLIErrorWithCode
// for exactly this reason — the store path just has many more call sites, so it
// gets one helper rather than eleven copies of the same errors.As.
func writeStoreInitError(stderr io.Writer, jsonOutput bool, exitCode int, err error, message string) {
	var mismatch *store.IndexVersionMismatchError
	if errors.As(err, &mismatch) {
		// The remedy is already in mismatch.Error() ("reindex the corpus"), so the
		// hint names the command rather than restating the diagnosis.
		writeCLIErrorWithCode(stderr, jsonOutput, exitCode, mismatch.Code(), message,
			"run `dir2mcp reindex` with a compatible binary, or point --state-dir at a fresh directory")
		return
	}
	writeCLIError(stderr, jsonOutput, exitCode, message)
}

func writeCLIErrorWithCode(stderr io.Writer, jsonOutput bool, exitCode int, code, message string, hints ...string) {
	if jsonOutput {
		filteredHints := make([]string, 0, len(hints))
		for _, hint := range hints {
			trimmed := strings.TrimSpace(hint)
			if trimmed != "" {
				filteredHints = append(filteredHints, trimmed)
			}
		}
		payload := cliErrorPayload{
			Error: cliError{
				Code:    code,
				Message: strings.TrimSpace(message),
				Hints:   filteredHints,
			},
			ExitCode: exitCode,
		}
		if err := emitJSON(stderr, payload); err != nil {
			fallback := strings.TrimSpace(message)
			if fallback == "" {
				fallback = "failed to encode error payload"
			} else {
				fallback = fmt.Sprintf("%s (failed to encode error payload: %v)", fallback, err)
			}
			escapedMessage, marshalErr := json.Marshal(fallback)
			if marshalErr != nil {
				escapedMessage = []byte("\"failed to encode error payload\"")
			}
			writef(
				stderr,
				"{\"error\":{\"code\":%q,\"message\":%s},\"exit_code\":%d}\n",
				code,
				escapedMessage,
				exitCode,
			)
		}
		return
	}
	writef(stderr, "%s\n", strings.TrimSpace(message))
	for _, hint := range hints {
		trimmed := strings.TrimSpace(hint)
		if trimmed != "" {
			writef(stderr, "%s\n", trimmed)
		}
	}
}

// serializeHits converts search hits into JSON-ready maps for CLI output.
func serializeHits(hits []model.SearchHit) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(hits))
	for _, hit := range hits {
		out = append(out, map[string]interface{}{
			"chunk_id": hit.ChunkID,
			"rel_path": hit.RelPath,
			"doc_type": hit.DocType,
			"rep_type": hit.RepType,
			"score":    hit.Score,
			"snippet":  hit.Snippet,
			"span":     serializeSpan(hit.Span),
		})
	}
	return out
}

// serializeCitations converts citations into JSON-ready maps for CLI output.
func serializeCitations(citations []model.Citation) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(citations))
	for _, citation := range citations {
		out = append(out, map[string]interface{}{
			"chunk_id": citation.ChunkID,
			"rel_path": citation.RelPath,
			"span":     serializeSpan(citation.Span),
		})
	}
	return out
}

// serializeSpan converts a span into a JSON-ready map, shaped per its kind
// (page, time, or the default line range).
func serializeSpan(span model.Span) map[string]interface{} {
	switch strings.ToLower(strings.TrimSpace(span.Kind)) {
	case "page":
		return map[string]interface{}{
			"kind": "page",
			"page": span.Page,
		}
	case "time":
		return map[string]interface{}{
			"kind":     "time",
			"start_ms": span.StartMS,
			"end_ms":   span.EndMS,
		}
	default:
		return map[string]interface{}{
			"kind":       "lines",
			"start_line": span.StartLine,
			"end_line":   span.EndLine,
		}
	}
}

// formatSpan renders a span as a compact human-readable token (e.g.
// "page:3", "time:1000-2000", or "lines:10-20").
func formatSpan(span model.Span) string {
	switch strings.ToLower(strings.TrimSpace(span.Kind)) {
	case "page":
		return fmt.Sprintf("page:%d", span.Page)
	case "time":
		return fmt.Sprintf("time:%d-%d", span.StartMS, span.EndMS)
	default:
		return fmt.Sprintf("lines:%d-%d", span.StartLine, span.EndLine)
	}
}

// tryConsumeGlobalFlag checks if arg matches --flagName or --flagName=value,
// consumes the value from remaining, and sets *target. Returns ok=true when
// matched. Returns an error when the flag is present but the value is missing.
func tryConsumeGlobalFlag(arg string, remaining []string, flagName string, target *string) (newRemaining []string, ok bool, err error) {
	fullFlag := "--" + flagName
	if arg == fullFlag || strings.HasPrefix(arg, fullFlag+"=") {
		value, consumed, err := consumeGlobalFlagValue(fullFlag, remaining)
		if err != nil {
			return nil, false, err
		}
		*target = value
		return remaining[consumed:], true, nil
	}
	return remaining, false, nil
}

// tryConsumeGlobalFlagSet attempts --dir, --config, and --state-dir in turn.
func tryConsumeGlobalFlagSet(arg string, remaining []string, opts *globalOptions) (newRemaining []string, ok bool, err error) {
	if newRem, matched, err := tryConsumeGlobalFlag(arg, remaining, "dir", &opts.rootDir); matched || err != nil {
		return newRem, matched, err
	}
	if newRem, matched, err := tryConsumeGlobalFlag(arg, remaining, "config", &opts.configPath); matched || err != nil {
		return newRem, matched, err
	}
	if newRem, matched, err := tryConsumeGlobalFlag(arg, remaining, "state-dir", &opts.stateDir); matched || err != nil {
		return newRem, matched, err
	}
	return remaining, false, nil
}

// parseGlobalOptions consumes leading global flags from args, returning the
// parsed options and the remaining command tokens. Unknown flags are
// rejected only before the command position; everything after the first
// non-flag token (including a trailing "--") is left for the subcommand.
func parseGlobalOptions(args []string) (globalOptions, []string, error) {
	opts := globalOptions{}
	var remaining []string
	cursor := args
	seenCommand := false

	for len(cursor) > 0 {
		arg := cursor[0]
		if arg == "--" {
			remaining = append(remaining, cursor...)
			break
		}
		if newRem, ok, err := tryConsumeGlobalFlagSet(arg, cursor, &opts); ok || err != nil {
			if err != nil {
				return globalOptions{}, nil, err
			}
			cursor = newRem
			continue
		}
		if enabled, matched, err := parseJSONFlagValue(arg); matched {
			if err != nil {
				return globalOptions{}, nil, err
			}
			opts.jsonOutput = enabled
			cursor = cursor[1:]
			continue
		}
		if arg == "--non-interactive" {
			opts.nonInteractive = true
			cursor = cursor[1:]
			continue
		}
		if arg == "--quiet" {
			opts.quiet = true
			cursor = cursor[1:]
			continue
		}
		// Strict unknown-flag check applies only before the command position is
		// observed; subcommand FlagSets are responsible for their own flags
		// once the first non-flag token has been seen.
		if !seenCommand && strings.HasPrefix(arg, "-") {
			return globalOptions{}, nil, fmt.Errorf("unknown global flag: %s", arg)
		}
		if !seenCommand {
			seenCommand = true
		}
		remaining = append(remaining, arg)
		cursor = cursor[1:]
	}

	return opts, remaining, nil
}

// parseJSONFlagValue matches the --json/-json flag. matched reports whether
// arg is the flag at all; enabled is its boolean value, with err set for an
// unparseable =value form.
func parseJSONFlagValue(arg string) (enabled bool, matched bool, err error) {
	if arg == "--json" || arg == "-json" {
		return true, true, nil
	}
	if strings.HasPrefix(arg, "--json=") {
		raw := strings.TrimSpace(strings.TrimPrefix(arg, "--json="))
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return false, true, fmt.Errorf("invalid value for --json: %q", raw)
		}
		return parsed, true, nil
	}
	if strings.HasPrefix(arg, "-json=") {
		raw := strings.TrimSpace(strings.TrimPrefix(arg, "-json="))
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return false, true, fmt.Errorf("invalid value for -json: %q", raw)
		}
		return parsed, true, nil
	}
	return false, false, nil
}

// consumeGlobalFlagValue extracts the value for flagName from args,
// supporting both --flag=value and --flag value forms, and returns the
// number of tokens consumed.
func consumeGlobalFlagValue(flagName string, args []string) (string, int, error) {
	if len(args) == 0 {
		return "", 0, fmt.Errorf("missing value for %s", flagName)
	}
	if strings.HasPrefix(args[0], flagName+"=") {
		value := strings.TrimSpace(strings.TrimPrefix(args[0], flagName+"="))
		if value == "" {
			return "", 0, fmt.Errorf("missing value for %s", flagName)
		}
		return value, 1, nil
	}
	if len(args) < 2 {
		return "", 0, fmt.Errorf("missing value for %s", flagName)
	}
	value := strings.TrimSpace(args[1])
	if value == "" {
		return "", 0, fmt.Errorf("missing value for %s", flagName)
	}
	return value, 2, nil
}

// defaultConfigFileName is the config file `dir2mcp` discovers in the process
// working directory when no --config is supplied.
const defaultConfigFileName = ".dir2mcp.yaml"

// resolveConfigPath returns the configured config path, defaulting to
// ".dir2mcp.yaml" when none was supplied.
func resolveConfigPath(global globalOptions) string {
	if trimmed := strings.TrimSpace(global.configPath); trimmed != "" {
		return trimmed
	}
	return defaultConfigFileName
}

// applyGlobalPathOverrides overlays the --dir and --state-dir global flags
// onto cfg, deriving a default state directory under the new root when the
// state directory was left at its default.
func applyGlobalPathOverrides(cfg config.Config, global globalOptions) config.Config {
	if root := strings.TrimSpace(global.rootDir); root != "" {
		stateLooksDefault := strings.TrimSpace(cfg.StateDir) == "" ||
			filepath.Clean(cfg.StateDir) == filepath.Clean(config.Default().StateDir)
		cfg.RootDir = root
		if strings.TrimSpace(global.stateDir) == "" && stateLooksDefault {
			cfg.StateDir = filepath.Join(root, ".dir2mcp")
		}
	}
	if state := strings.TrimSpace(global.stateDir); state != "" {
		cfg.StateDir = state
	}
	return cfg
}

// loadConfigWithGlobalOptions loads the config file, applies the global path
// overrides, and writes the effective-config snapshot (a snapshot failure is
// recorded as a non-fatal warning).
func loadConfigWithGlobalOptions(global globalOptions) (config.Config, error) {
	cfg, err := config.Load(resolveConfigPath(global))
	if err != nil {
		return config.Config{}, err
	}
	cfg = applyGlobalPathOverrides(cfg, global)
	if snapErr := saveEffectiveConfigSnapshot(cfg, authMaterial{}, ""); snapErr != nil {
		cfg.Warnings = append(cfg.Warnings, fmt.Errorf("write config snapshot: %w", snapErr))
	}
	return cfg, nil
}

// loadConfigForDaemonParent resolves the config without writing the
// effective-config snapshot. The daemon parent only needs the state
// directory and auth material to spawn + monitor the child; the child's
// own loadConfigWithGlobalOptions call writes the snapshot once. This
// avoids the duplicate disk write the reviewer flagged on PR #174.
func loadConfigForDaemonParent(global globalOptions) (config.Config, error) {
	cfg, err := config.Load(resolveConfigPath(global))
	if err != nil {
		return config.Config{}, err
	}
	cfg = applyGlobalPathOverrides(cfg, global)
	return cfg, nil
}

// saveEffectiveConfigSnapshot writes the effective (post-merge) config
// snapshot, recording where each secret was sourced from
// (env/configured/…) without persisting the secret values themselves.
func saveEffectiveConfigSnapshot(cfg config.Config, auth authMaterial, x402TokenSource string) error {
	sources := config.SecretSourceMetadata{}
	if strings.TrimSpace(cfg.ElevenLabsAPIKey) != "" {
		sources.ElevenLabsAPIKey = "configured"
		if strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY")) != "" {
			sources.ElevenLabsAPIKey = "env"
		}
	}
	if strings.TrimSpace(cfg.X402.FacilitatorToken) != "" {
		source := strings.TrimSpace(x402TokenSource)
		if source == "" {
			source = "configured"
		}
		sources.X402FacilitatorToken = source
	}
	if strings.TrimSpace(auth.token) != "" {
		source := strings.TrimSpace(auth.tokenSource)
		if source == "" {
			source = "configured"
		}
		sources.AuthToken = source
	}
	_, err := config.SaveEffectiveSnapshot(cfg, sources)
	return err
}

// validateTLSFlags ensures the TLS cert and key flags are supplied together
// and that each points to an existing regular file.
func validateTLSFlags(certPath, keyPath string) error {
	certPath = strings.TrimSpace(certPath)
	keyPath = strings.TrimSpace(keyPath)
	if certPath == "" && keyPath == "" {
		return nil
	}
	if certPath == "" || keyPath == "" {
		return errors.New("--tls-cert and --tls-key must be provided together")
	}
	for _, p := range []string{certPath, keyPath} {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("tls file %q: %w", p, err)
		}
		if info.IsDir() {
			return fmt.Errorf("tls file %q is a directory", p)
		}
	}
	return nil
}

// parseUpOptions parses the up subcommand flags on top of the global
// options, applying the facilitator-token file-wins precedence and rejecting
// the --daemon/--foreground and --daemon/--json conflicts.
func parseUpOptions(global globalOptions, args []string) (upOptions, error) {
	opts := upOptions{globalOptions: global}
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.jsonOutput, "json", opts.jsonOutput, "emit NDJSON events")
	fs.BoolVar(&opts.nonInteractive, "non-interactive", opts.nonInteractive, "disable prompts")
	fs.BoolVar(&opts.readOnly, "read-only", false, "run in read-only mode")
	fs.BoolVar(&opts.public, "public", false, "bind to all interfaces for external access")
	fs.BoolVar(&opts.forceInsecure, "force-insecure", false, "allow public mode without auth (unsafe)")
	fs.BoolVar(&opts.foreground, "foreground", false, "run in the foreground (do not daemonize); implied by --json or non-TTY stdout")
	fs.BoolVar(&opts.foreground, "f", false, "alias for --foreground")
	fs.BoolVar(&opts.daemon, "daemon", false, "force daemon mode even when stdout is not a TTY (use this with `nohup` or in scripts that want fire-and-forget)")
	fs.StringVar(&opts.x402Mode, "x402", "", "x402 mode: off|on|required")
	fs.StringVar(&opts.x402FacilitatorURL, "x402-facilitator-url", "", "x402 facilitator base URL")
	fs.StringVar(&opts.x402FacilitatorToken, "x402-facilitator-token", "", "x402 facilitator bearer token (insecure; token may also be provided via env or file)")
	fs.StringVar(&opts.x402FacilitatorTokenFile, "x402-facilitator-token-file", "", "path to file containing x402 facilitator bearer token")
	fs.StringVar(&opts.x402ResourceBaseURL, "x402-resource-base-url", "", "x402 resource base URL")
	fs.StringVar(&opts.x402Network, "x402-network", "", "x402 network (CAIP-2)")
	fs.StringVar(&opts.x402Price, "x402-price", "", "x402 atomic price per call")
	fs.StringVar(&opts.x402Scheme, "x402-scheme", "", "x402 payment scheme")
	fs.StringVar(&opts.x402Asset, "x402-asset", "", "x402 asset identifier")
	fs.StringVar(&opts.x402PayTo, "x402-pay-to", "", "x402 pay-to address")
	toolsCallEnabledFlag := &optionalBoolFlag{}
	fs.Var(toolsCallEnabledFlag, "x402-tools-call-enabled", "enable x402 gating for tools/call")
	fs.StringVar(&opts.auth, "auth", "", "auth mode: auto|none|file:<path>")
	fs.StringVar(&opts.listen, "listen", "", "listen address")
	fs.StringVar(&opts.mcpPath, "mcp-path", "", "MCP route path")
	fs.StringVar(&opts.tlsCert, "tls-cert", "", "path to TLS certificate file (PEM)")
	fs.StringVar(&opts.tlsKey, "tls-key", "", "path to TLS private key file (PEM)")
	fs.StringVar(&opts.allowedOrigins, "allowed-origins", "", "comma-separated origins to append to the allowlist")
	if err := fs.Parse(args); err != nil {
		return upOptions{}, err
	}
	if toolsCallEnabledFlag.set {
		opts.x402ToolsCallEnabled = toolsCallEnabledFlag.value
		opts.x402ToolsCallEnabledIsSet = true
	}

	// if both forms of the facilitator token are supplied, the file wins.  the
	// CLI parsing layer clears the direct-token field when a file path is
	// present so that callers (including tests) can rely on mutual
	// exclusivity without re‑implementing precedence logic. preserve whether
	// the direct flag was originally set so we can warn later in the CLI flow.
	directSet := strings.TrimSpace(opts.x402FacilitatorToken) != ""
	if opts.x402FacilitatorTokenFile != "" && directSet {
		opts.x402FacilitatorTokenDirectSet = true
		opts.x402FacilitatorToken = ""
	}

	if fs.NArg() > 0 {
		return upOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	// --daemon and --foreground are mutually exclusive — one forces
	// daemon mode regardless of TTY, the other forces foreground.
	// --json is incompatible with daemon mode because the parent exits
	// before the NDJSON event stream completes; surface the conflict
	// instead of silently dropping --daemon at runtime.
	if opts.daemon && opts.foreground {
		return upOptions{}, fmt.Errorf("--daemon and --foreground are mutually exclusive")
	}
	if opts.daemon && opts.jsonOutput {
		return upOptions{}, fmt.Errorf("--daemon is incompatible with --json (NDJSON event stream requires the foreground process to remain attached)")
	}
	return opts, nil
}

// closeStoreWithLog closes the store, logging any close error to stderr.
func (a *App) closeStoreWithLog(st model.Store) {
	if closeErr := st.Close(); closeErr != nil {
		writef(a.stderr, "close store: %v\n", closeErr)
	}
}

// clearContentHashesIfSupported clears stored document content hashes when
// the store implements contentHashResetter (forcing re-ingestion), returning
// an exit code; stores without the capability are a no-op success.
func clearContentHashesIfSupported(ctx context.Context, st model.Store, stderr io.Writer, jsonOutput bool) int {
	resetter, ok := interface{}(st).(contentHashResetter)
	if !ok {
		return exitSuccess
	}
	if err := resetter.ClearDocumentContentHashes(ctx); err != nil {
		writeCLIError(stderr, jsonOutput, exitGeneric, fmt.Sprintf("clear content hashes: %v", err))
		return exitGeneric
	}
	return exitSuccess
}

// ensureRootAccessible verifies that root exists and is a directory.
func ensureRootAccessible(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory: %s", root)
	}
	return nil
}

// prepareAuthMaterial resolves the bearer-token auth material for the
// configured auth mode: "none" disables auth, "auto" delegates to
// prepareAutoAuthMaterial, and "file:<path>" loads a token from disk.
//
// The two token modes have deliberately different contracts. "auto" owns
// <state-dir>/secret.token: dir2mcp creates it, may rewrite it, and enforces
// the SPEC §4.2 owner-only requirement on it. "file:<path>" is
// operator-managed: the path is read and required to be non-empty, and
// nothing else. Its mode is not checked and never changed, because a
// deliberately group-readable token shared with other processes on the host
// is a legitimate setup there, and because that flag is the documented escape
// hatch (SPEC §17) for a token that must live outside the state directory.
//
// warn receives operator-facing security notices; they are written whatever
// the output mode, because a credential that may have been exposed is not
// status chatter that --quiet asked to be spared.
func prepareAuthMaterial(cfg config.Config, warn io.Writer) (authMaterial, error) {
	mode := strings.TrimSpace(cfg.AuthMode)
	if mode == "" {
		mode = "auto"
	}

	if strings.EqualFold(mode, "none") {
		return authMaterial{
			mode:        "none",
			tokenSource: "none",
		}, nil
	}

	if strings.EqualFold(mode, "auto") {
		return prepareAutoAuthMaterial(cfg, warn)
	}

	if len(mode) >= len("file:") && strings.EqualFold(mode[:len("file:")], "file:") {
		tokenPath := strings.TrimSpace(mode[len("file:"):])
		if tokenPath == "" {
			return authMaterial{}, errors.New("auth mode file: requires a token path")
		}

		token, err := readToken(tokenPath, false)
		if err != nil {
			return authMaterial{}, err
		}
		if token == "" {
			return authMaterial{}, errors.New("auth file token is empty")
		}

		absPath := tokenPath
		if abs, err := filepath.Abs(tokenPath); err == nil {
			absPath = abs
		}
		return authMaterial{
			mode:              "file",
			token:             token,
			tokenSource:       "file",
			tokenFile:         absPath,
			authorizationHint: "Bearer <token-from-file>",
		}, nil
	}

	return authMaterial{}, fmt.Errorf("unsupported auth mode: %s", mode)
}

// prepareAutoAuthMaterial resolves auth material in "auto" mode: it prefers
// the env-var token, otherwise reads the persisted secret.token, generating
// and persisting a new random token when none exists.
//
// The persisted token is hardened before it is read or rewritten, so an
// existing file is held to the same SPEC §4.2 owner-only requirement as one
// this run creates (#715).
func prepareAutoAuthMaterial(cfg config.Config, warn io.Writer) (authMaterial, error) {
	if token := strings.TrimSpace(os.Getenv(authTokenEnvVar)); token != "" {
		return authMaterial{
			mode:              "auto",
			token:             token,
			tokenSource:       "env",
			authorizationHint: "Bearer <token-from-env>",
		}, nil
	}
	tokenPath := filepath.Join(cfg.StateDir, secretTokenName)
	if err := hardenSecretToken(tokenPath, warn); err != nil {
		return authMaterial{}, err
	}
	token, err := readToken(tokenPath, true)
	if err != nil {
		return authMaterial{}, err
	}
	if token == "" {
		token, err = generateTokenHex()
		if err != nil {
			return authMaterial{}, err
		}
		if err := writeSecretToken(tokenPath, token); err != nil {
			return authMaterial{}, err
		}
	}
	absPath := tokenPath
	if abs, err := filepath.Abs(tokenPath); err == nil {
		absPath = abs
	}
	return authMaterial{
		mode:              "auto",
		token:             token,
		tokenSource:       "secret.token",
		tokenFile:         absPath,
		authorizationHint: "Bearer <token-from-secret.token>",
	}, nil
}

// hardenSecretToken enforces SPEC §4.2 on the auto-managed token file before
// it is read or rewritten, and tells the operator when it had to.
//
// Repair rather than refuse: this file is dir2mcp's own, and the modes get
// lost for mundane reasons (a corpus restored from a tarball, copied with
// `cp -r`, created by an older build under a 022 umask). Refusing would turn
// every one of those into a dead daemon for a condition the daemon can fix
// itself. But the repair is announced, because tightening the file does not
// un-read it: a token another local account could read may already be in that
// account's hands, and only the operator knows whether this host has other
// accounts that matter.
//
// It is announced rather than acted on, too. Rotating the token would be the
// safe reflex, but it invalidates every client that already holds it (the
// Claude/ElevenLabs registrations, connection.json consumers, anything with
// the bearer baked into a config), so a permission repair on startup would
// silently break a working deployment. Whether the exposure warrants that
// depends on who else has an account on this host, which the daemon cannot
// know, so the warning names the one command instead of guessing.
//
// A non-regular path is refused outright: see statefs.HardenSecret for why a
// symlinked credential is a different question from a symlinked cache.
func hardenSecretToken(path string, warn io.Writer) error {
	prior, exists, err := statefs.HardenSecret(path)
	if err != nil {
		if errors.Is(err, statefs.ErrNotRegular) {
			return fmt.Errorf("%w; auto auth owns this file: it reads it and may rewrite it, so it must be a plain "+
				"file. Remove it to have a fresh token generated, or point --auth file:<path> at an "+
				"operator-managed token", err)
		}
		return fmt.Errorf("secure %s: %w", path, err)
	}
	if !exists || !statefs.WiderThanOwnerOnly(prior) {
		return nil
	}
	writef(warn, "warning: %s was mode %04o, readable by other local accounts, and has been tightened to %04o\n",
		path, prior.Perm(), statefs.FileMode.Perm())
	writef(warn, "warning: tightening does not undo the exposure. If other accounts on this host may have read it, "+
		"rotate the token: stop the server, delete %s, start again (a new token is generated) and re-register your clients\n", path)
	return nil
}

// readToken reads and trims the token at path; when allowMissing is set, a
// missing file yields an empty token instead of an error.
func readToken(path string, allowMissing bool) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}

// generateTokenHex returns a cryptographically random 32-byte token as a hex
// string.
func generateTokenHex() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// writeSecretToken writes token to path owner-only, truncating any existing
// file.
//
// statefs.Create rather than a bare os.OpenFile with the same mode: a create
// mode says nothing about a file that already exists, so an existing token
// left 0644 (by an older build, or by a restore that dropped the modes) would
// otherwise keep that mode through every rewrite. statefs.Create tightens the
// open descriptor, which also settles the case where the path was swapped
// between the check in hardenSecretToken and this write.
func writeSecretToken(path, token string) error {
	file, err := statefs.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()

	if _, err := file.WriteString(token + "\n"); err != nil {
		return err
	}
	return nil
}

// buildMCPURL assembles the MCP endpoint URL from the address and path,
// choosing the https scheme when TLS is enabled.
func buildMCPURL(addr, path string, tlsEnabled bool) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	return scheme + "://" + addr + path
}

// PublicURLAddress derives the public-facing address using the configured
// listen host and the resolved runtime port.
func PublicURLAddress(configuredListenAddr, resolvedListenAddr string) string {
	return publicURLAddress(configuredListenAddr, resolvedListenAddr)
}

// publicURLAddress joins the configured listen host with the resolved
// runtime port (falling back to the configured port, then "0"), defaulting
// the host to 0.0.0.0 when none was configured.
func publicURLAddress(configuredListenAddr, resolvedListenAddr string) string {
	configuredListenAddr = strings.TrimSpace(configuredListenAddr)
	resolvedListenAddr = strings.TrimSpace(resolvedListenAddr)

	host := "0.0.0.0"
	if parsedHost, _, err := net.SplitHostPort(configuredListenAddr); err == nil && strings.TrimSpace(parsedHost) != "" {
		host = parsedHost
	}

	if port := extractPortFromAddress(resolvedListenAddr); port != "" {
		return net.JoinHostPort(host, port)
	}
	if port := extractPortFromAddress(configuredListenAddr); port != "" {
		return net.JoinHostPort(host, port)
	}

	return net.JoinHostPort(host, "0")
}

// ExtractPortFromAddress extracts a numeric trailing port token from a
// host:port address or malformed best-effort address string.
func ExtractPortFromAddress(addr string) string {
	return extractPortFromAddress(addr)
}

// extractPortFromAddress returns the numeric port from a host:port address,
// with a best-effort fallback for malformed values that still end in a
// numeric ":port" token, and "" when none can be determined.
func extractPortFromAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}

	if _, port, err := net.SplitHostPort(addr); err == nil {
		port = strings.TrimSpace(port)
		if isNumericPort(port) {
			return port
		}
		return ""
	}

	// Best effort for malformed values where SplitHostPort fails but the
	// value still contains a trailing numeric ":port" token.
	i := strings.LastIndex(addr, ":")
	if i < 0 || i == len(addr)-1 {
		return ""
	}
	port := addr[i+1:]
	if strings.ContainsAny(port, " \t\r\n/\\") {
		return ""
	}
	if isNumericPort(port) {
		return port
	}
	return ""
}

// isNumericPort reports whether port is a non-empty string of ASCII digits.
func isNumericPort(port string) bool {
	if port == "" {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// buildConnectionPayload assembles the connection.json payload describing
// the MCP transport, URL, required headers, session policy, and token
// source; the Authorization header hint is omitted when auth is disabled.
func buildConnectionPayload(cfg config.Config, url string, auth authMaterial) connectionPayload {
	headers := map[string]string{
		protocol.MCPProtocolVersionHeader: cfg.ProtocolVersion,
	}
	if auth.mode != "none" {
		headers["Authorization"] = auth.authorizationHint
	}

	return connectionPayload{
		FormatVersion: protocol.FormatVersion,
		Transport:     "mcp_streamable_http",
		URL:           url,
		Headers:       headers,
		Session: connectionSession{
			UsesMCPSessionID:     true,
			HeaderName:           protocol.MCPSessionHeader,
			AssignedOnInitialize: true,
		},
		Public:      cfg.Public,
		TokenSource: auth.tokenSource,
		TokenFile:   auth.tokenFile,
	}
}

// writeConnectionFile writes the connection payload as indented JSON to
// path with 0o600 permissions (the file is credential-adjacent).
func writeConnectionFile(path string, payload connectionPayload) error {
	content, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	// 0o600 — connection.json carries the bearer-token file path and the
	// MCP URL; on shared dev machines (think /Users/<me>/Development/...
	// readable by other accounts) those are credential-adjacent. The
	// state directory is 0o700 already, but defending in depth keeps a
	// permissive parent directory from leaking the file's contents.
	//
	// Write atomically (temp file + fsync + rename) so a concurrent reader
	// — readiness poll, support bundle, or claude_cmd — never observes a
	// truncated/partial file the way an in-place os.WriteFile(O_TRUNC) can.
	return atomicWriteFile(path, content, 0o600)
}

// atomicWriteFile writes data to path atomically: it writes to a sibling
// temp file in the same directory, fsyncs it, chmods it to mode, then
// renames it over path (with a Windows remove-and-retry fallback). The
// rename guarantees a concurrent reader sees either the old or the new file,
// never a partial one. A per-write temp name keeps concurrent writers from
// stomping each other's temp file.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// os.Rename fails on Windows when the destination already exists.
		// Remove the existing file and retry once to support Windows.
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("rename temp file: %w", err2)
		}
	}
	return nil
}

// newNDJSONEmitter constructs an ndjsonEmitter that writes events to out
// when enabled.
func newNDJSONEmitter(out io.Writer, enabled bool) *ndjsonEmitter {
	return &ndjsonEmitter{enabled: enabled, out: out}
}

// runCorpusWriter periodically writes the corpus snapshot using the default
// 5s interval until ctx is cancelled.
func runCorpusWriter(ctx context.Context, stateDir string, st model.Store, indexingState *appstate.IndexingState, stderr io.Writer, emitter *ndjsonEmitter) {
	runCorpusWriterWithInterval(ctx, stateDir, st, indexingState, stderr, emitter, 5*time.Second)
}

// runCorpusWriterWithInterval emits an initial corpus snapshot, then
// refreshes it every interval while indexing is running, returning when ctx
// is cancelled. A non-positive interval defaults to 5s.
func runCorpusWriterWithInterval(ctx context.Context, stateDir string, st model.Store, indexingState *appstate.IndexingState, stderr io.Writer, emitter *ndjsonEmitter, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	// Emit an initial snapshot immediately, then refresh while indexing runs.
	logSnapshotErr(stderr, writeCorpusSnapshot(ctx, stateDir, st, indexingState, stderr, emitter))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if indexingState != nil && !indexingState.Snapshot().Running {
				continue
			}
			logSnapshotErr(stderr, writeCorpusSnapshot(ctx, stateDir, st, indexingState, stderr, emitter))
		}
	}
}

// logSnapshotErr writes a corpus-snapshot error to stderr unless it is a
// context cancellation. Cancellation is the normal shutdown signal — emitting
// it as plaintext on stderr pollutes the JSON event stream the caller may be
// consuming and provides no actionable information. Suppress it.
func logSnapshotErr(stderr io.Writer, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	writef(stderr, "write corpus snapshot: %v\n", err)
}

// writeCorpusSnapshot builds the corpus snapshot and atomically writes it to
// corpus.json in stateDir via a per-write temp file and rename (with a
// Windows remove-and-retry fallback).
func writeCorpusSnapshot(ctx context.Context, stateDir string, st model.Store, indexingState *appstate.IndexingState, stderr io.Writer, emitter *ndjsonEmitter) error {
	snapshot, err := buildCorpusSnapshot(ctx, st, indexingState, stderr, emitter)
	if err != nil {
		return err
	}
	emitProgressEvents(emitter, snapshot.Indexing)

	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal corpus snapshot: %w", err)
	}

	// Owner-only: this lives under the state directory and carries the corpus
	// snapshot (paths, titles, counts), so it is corpus content in another
	// shape rather than a public artifact (#726). atomicWriteFile chmods the
	// temp file before the rename, so the mode is never briefly wider.
	path := filepath.Join(stateDir, "corpus.json")
	if err := atomicWriteFile(path, raw, statefs.FileMode); err != nil {
		return fmt.Errorf("write corpus snapshot: %w", err)
	}
	return nil
}

// emitProgressEvents emits the spec-required `scan_progress` and
// `embed_progress` NDJSON events (SPEC §3.2) from a freshly built snapshot.
// It is called on every corpus-writer tick, which is what makes the events
// *periodic* — before #414 they were emitted exactly once at startup with
// hardcoded zeros, so an operator tailing `--json` never saw indexing advance.
//
// Representations, ChunksTotal and EmbeddedOK carry the -1 "not derivable"
// sentinel on the ListFiles-only fallback path; it is passed through verbatim
// because the spec defines -1 as "unavailable", not as an error.
func emitProgressEvents(emitter *ndjsonEmitter, idx corpusIndexing) {
	emitter.Emit("info", "scan_progress", map[string]interface{}{
		"scanned": idx.Scanned,
		"indexed": idx.Indexed,
		"skipped": idx.Skipped,
		"deleted": idx.Deleted,
		"reps":    idx.Representations,
		"chunks":  idx.ChunksTotal,
		"errors":  idx.Errors,
	})
	emitter.Emit("info", "embed_progress", map[string]interface{}{
		"embedded": idx.EmbeddedOK,
		"chunks":   idx.ChunksTotal,
		"errors":   idx.Errors,
	})
}

// watchOverflowsField returns a pointer to the lifetime watch-overflow count
// when a watcher is running, or nil otherwise so `status --json` omits the
// optional watch_overflows field entirely (absence = "not applicable", not 0;
// spec 0.33.0 / stats.json, #591).
func watchOverflowsField(idx appstate.IndexingSnapshot) *int64 {
	if !idx.WatchActive {
		return nil
	}
	v := idx.WatchOverflows
	return &v
}

// buildCorpusSnapshot collects corpus stats and assembles a corpusSnapshot,
// preferring live indexing-state counters when available and otherwise
// deriving them from the store, including the code/total doc ratio.
func buildCorpusSnapshot(ctx context.Context, st model.Store, indexingState *appstate.IndexingState, stderr io.Writer, emitter *ndjsonEmitter) (corpusSnapshot, error) {
	corpusStats, err := collectCorpusStats(ctx, st, stderr, emitter)
	if err != nil {
		return corpusSnapshot{}, err
	}

	docCounts := corpusStats.DocCounts
	totalDocs := corpusStats.TotalDocs
	codeDocs := docCounts["code"]
	codeRatio := 0.0
	if totalDocs > 0 {
		codeRatio = float64(codeDocs) / float64(totalDocs)
	}

	idx := appstate.IndexingSnapshot{Mode: appstate.ModeIncremental}
	if indexingState != nil {
		idx = indexingState.Snapshot()
	} else {
		idx.Scanned = corpusStats.Scanned
		idx.Indexed = corpusStats.Indexed
		idx.Skipped = corpusStats.Skipped
		idx.Deleted = corpusStats.Deleted
		idx.Representations = corpusStats.Representations
		idx.ChunksTotal = corpusStats.ChunksTotal
		idx.EmbeddedOK = corpusStats.EmbeddedOK
		idx.Errors = corpusStats.Errors
		idx.Unknown = corpusStats.Unknown
	}

	return corpusSnapshot{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Indexing: corpusIndexing{
			Mode:            idx.Mode,
			Running:         idx.Running,
			Scanned:         idx.Scanned,
			Indexed:         idx.Indexed,
			Skipped:         idx.Skipped,
			Deleted:         idx.Deleted,
			Representations: idx.Representations,
			ChunksTotal:     idx.ChunksTotal,
			EmbeddedOK:      idx.EmbeddedOK,
			// embedded_pending is always a store-derived count (no live
			// appstate counter tracks it), so source it straight from
			// corpusStats rather than idx — correct in both the live-snapshot
			// and computed-fallback branches above (#364).
			EmbeddedPending: corpusStats.EmbeddedPending,
			Errors:          idx.Errors,
			Unknown:         idx.Unknown,
			// FailureSummary travels straight through from the
			// aggregate CorpusStats so `status --json` consumers see
			// the same grouping the doctor renders. Omitted from JSON
			// when no failures have been recorded (omitempty on the
			// struct tag).
			FailureSummary: corpusStats.FailureSummary,
			// SkipSummary likewise travels through from CorpusStats so
			// `status` can render an honest not-indexed coverage block
			// (#414). Omitted from JSON on corpora with nothing skipped.
			SkipSummary: corpusStats.SkipSummary,
			// watch_overflows is surfaced only when a watcher is running
			// (#591); on the store-derived fallback (indexingState == nil)
			// WatchActive is false, so it stays omitted.
			WatchOverflows: watchOverflowsField(idx),
		},
		DocCounts: docCounts,
		TotalDocs: totalDocs,
		CodeRatio: codeRatio,
	}, nil
}

// collectCorpusStats returns corpus statistics, preferring the store's
// aggregate CorpusStats and otherwise falling back to a ListFiles scan
// (which leaves representation/chunk/embed counters at the -1 sentinel).
func collectCorpusStats(ctx context.Context, st model.Store, stderr io.Writer, emitter *ndjsonEmitter) (model.CorpusStats, error) {
	if st == nil {
		return model.CorpusStats{DocCounts: map[string]int64{}}, nil
	}

	if agg, ok := st.(corpusStatsStore); ok {
		stats, err := agg.CorpusStats(ctx)
		if err == nil {
			if stats.DocCounts == nil {
				stats.DocCounts = map[string]int64{}
			}
			return stats, nil
		}
		if !errors.Is(err, model.ErrNotImplemented) {
			return model.CorpusStats{}, fmt.Errorf("corpus stats: %w", err)
		}
	}

	docCounts, totalDocs, err := collectActiveDocCounts(ctx, st)
	if err != nil {
		return model.CorpusStats{}, err
	}

	statusCounts, err := collectDocumentStatusCounts(ctx, st, stderr, emitter)
	if err != nil {
		return model.CorpusStats{}, err
	}

	return model.CorpusStats{
		DocCounts: docCounts,
		TotalDocs: totalDocs,
		Scanned:   statusCounts.Scanned,
		Indexed:   statusCounts.Indexed,
		Skipped:   statusCounts.Skipped,
		Deleted:   statusCounts.Deleted,
		// Not derivable from ListFiles-only fallback path.
		Representations: -1,
		ChunksTotal:     -1,
		EmbeddedOK:      -1,
		Errors:          statusCounts.Errors,
		Unknown:         statusCounts.Unknown,
	}, nil
}

// collectActiveDocCounts returns per-doc-type and total counts of non-deleted
// documents, preferring the store's aggregate ActiveDocCounts and otherwise
// paginating through ListFiles.
func collectActiveDocCounts(ctx context.Context, st model.Store) (map[string]int64, int64, error) {
	if st == nil {
		return map[string]int64{}, 0, nil
	}
	if agg, ok := st.(activeDocCountStore); ok {
		counts, total, err := agg.ActiveDocCounts(ctx)
		if err == nil {
			return counts, total, nil
		}
		if !errors.Is(err, model.ErrNotImplemented) {
			return nil, 0, fmt.Errorf("active doc counts: %w", err)
		}
	}

	const pageSize = 500
	offset := 0
	counts := make(map[string]int64)
	var totalActive int64

	for {
		docs, total, err := st.ListFiles(ctx, "", "", pageSize, offset)
		if err != nil {
			return nil, 0, fmt.Errorf("list files: %w", err)
		}
		for _, doc := range docs {
			if doc.Deleted {
				continue
			}
			docType := strings.TrimSpace(doc.DocType)
			if docType == "" {
				docType = "unknown"
			}
			counts[docType]++
			totalActive++
		}
		offset += len(docs)
		if len(docs) == 0 || int64(offset) >= total {
			break
		}
	}

	return counts, totalActive, nil
}

type documentStatusCounts struct {
	Scanned int64
	Indexed int64
	Skipped int64
	Deleted int64
	Errors  int64
	Unknown int64
}

// collectDocumentStatusCounts paginates through ListFiles and tallies
// documents by status (indexed/skipped/error/deleted/unknown), reporting any
// unexpected status values it encounters.
func collectDocumentStatusCounts(ctx context.Context, st model.Store, stderr io.Writer, emitter *ndjsonEmitter) (documentStatusCounts, error) {
	if st == nil {
		return documentStatusCounts{}, nil
	}

	const pageSize = 500
	offset := 0
	counts := documentStatusCounts{}
	unexpectedStatusCounts := make(map[string]int64)
	unexpectedStatusExample := make(map[string]string)

	for {
		docs, total, err := st.ListFiles(ctx, "", "", pageSize, offset)
		if err != nil {
			return documentStatusCounts{}, fmt.Errorf("list files: %w", err)
		}
		for _, doc := range docs {
			counts.Scanned++
			if doc.Deleted {
				counts.Deleted++
				continue
			}

			switch strings.ToLower(strings.TrimSpace(doc.Status)) {
			case "indexed", "ok":
				counts.Indexed++
			case "skipped":
				counts.Skipped++
			case "error":
				counts.Errors++
			default:
				rawStatus := strings.TrimSpace(doc.Status)
				if rawStatus == "" {
					rawStatus = "<empty>"
				}
				counts.Unknown++
				unexpectedStatusCounts[rawStatus]++
				if _, exists := unexpectedStatusExample[rawStatus]; !exists {
					unexpectedStatusExample[rawStatus] = strings.TrimSpace(doc.RelPath)
				}
			}
		}

		offset += len(docs)
		if len(docs) == 0 || int64(offset) >= total {
			break
		}
	}

	if len(unexpectedStatusCounts) > 0 {
		reportUnexpectedDocStatuses(unexpectedStatusCounts, unexpectedStatusExample, stderr, emitter)
	}

	return counts, nil
}

// reportUnexpectedDocStatuses surfaces a sorted summary of unexpected
// document statuses, emitting a structured NDJSON warning when an emitter is
// active and otherwise a plain stderr warning.
func reportUnexpectedDocStatuses(statusCounts map[string]int64, examples map[string]string, stderr io.Writer, emitter *ndjsonEmitter) {
	parts := make([]string, 0, len(statusCounts))
	for statusVal, count := range statusCounts {
		example := examples[statusVal]
		parts = append(parts, fmt.Sprintf("%s=%d (example rel_path=%q)", statusVal, count, example))
	}
	sort.Strings(parts)
	msg := fmt.Sprintf("unexpected document statuses encountered during scan: %s", strings.Join(parts, ", "))
	if emitter != nil && emitter.enabled {
		emitter.Emit("warning", "unexpected_document_statuses", map[string]interface{}{
			"message":  msg,
			"counts":   statusCounts,
			"examples": examples,
		})
	} else {
		writef(stderr, "warning: %s\n", msg)
	}
}

// Emit writes a single timestamped NDJSON event line when the emitter is
// enabled; encoding failures are silently dropped.
func (e *ndjsonEmitter) Emit(level, event string, data interface{}) {
	if e == nil || !e.enabled {
		return
	}
	entry := ndjsonEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     level,
		Event:     event,
		Data:      data,
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = fmt.Fprintln(e.out, string(encoded))
}

// printHumanConnection prints the styled "ready for connections" banner to
// stdout, summarizing the mode, MCP endpoint URL, auth source, required
// headers, and client registration hint.
func (a *App) printHumanConnection(cfg config.Config, connection connectionPayload, auth authMaterial, readOnly bool) {
	s := a.sty(false)
	writeln(a.stdout)
	writef(a.stdout, "  %s %s\n", s.banner(), s.dim(buildinfo.Display()))
	writeln(a.stdout, s.separator(44))
	writeln(a.stdout)

	mode := "incremental (server-first; indexing in background)"
	if readOnly {
		mode += ", read-only"
	}
	if cfg.Public {
		mode += ", public"
	}
	writeln(a.stdout, s.kv("Index", cfg.StateDir))
	writeln(a.stdout, s.kv("Mode", mode))
	writeln(a.stdout)

	if cfg.Public {
		writef(a.stdout, "  %s server is bound to all interfaces. Ensure auth is enabled.\n", s.warnPrefix())
		writeln(a.stdout)
	}

	writef(a.stdout, "  %s\n", s.sectionHeader("MCP endpoint"))
	writeln(a.stdout, s.kv("URL", s.URL.Render(connection.URL)))
	if auth.mode == "none" {
		writeln(a.stdout, s.kv("Auth", s.Yellow.Render("none")))
	} else {
		writeln(a.stdout, s.kv("Auth", fmt.Sprintf("Bearer %s", s.dim("(source="+auth.tokenSource+")"))))
	}
	if auth.tokenFile != "" {
		writeln(a.stdout, s.kv("Token file", auth.tokenFile))
	}
	writeln(a.stdout)

	writef(a.stdout, "  %s\n", s.sectionHeader("Required headers"))
	writeln(a.stdout, s.subkv(protocol.MCPProtocolVersionHeader, cfg.ProtocolVersion))
	if auth.mode != "none" {
		writeln(a.stdout, s.subkv("Authorization", "Bearer <token>"))
	}
	writeln(a.stdout, s.subkv(protocol.MCPSessionHeader, s.dim("(assigned after initialize response)")))
	writeln(a.stdout)

	printRoutingSection(a.stdout, s, routingDecisions(cfg))
	printRegistrationHint(a.stdout, s, cfg.ServerName, connection.URL, cfg.ProtocolVersion, auth.mode != "none")
	writeln(a.stdout, s.separator(44))
	writef(a.stdout, "  %s\n", s.Success.Render("Ready for connections"))
	// In foreground mode the user can stop with q+Enter (the stdin
	// listener) or Ctrl-C; either way we don't crowd the banner with
	// terminal-control hints. Daemon mode prints its own one-line
	// "Stop with: dir2mcp down" hint via printDaemonReady.
	writeln(a.stdout)
}

// isTerminal reports whether file is a character device (an interactive
// terminal).
func isTerminal(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
