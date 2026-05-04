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
	"syscall"
	"time"

	"dir2mcp/internal/appstate"
	"dir2mcp/internal/buildinfo"
	"dir2mcp/internal/config"
	"dir2mcp/internal/index"
	"dir2mcp/internal/ingest"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/mistral"
	"dir2mcp/internal/model"
	"dir2mcp/internal/protocol"
	"dir2mcp/internal/retrieval"
	"dir2mcp/internal/store"
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
	"up":      {},
	"status":  {},
	"ask":     {},
	"reindex": {},
	"bridge":  {},
	"config":  {},
	"version": {},
}

type App struct {
	stdout io.Writer
	stderr io.Writer

	newIngestor  func(config.Config, model.Store) (model.Ingestor, error)
	newStore     func(config.Config) model.Store
	newRetriever func(config.Config, model.Store) model.Retriever

	cachedStyles map[bool]*styles
}

type indexingStateAware interface {
	SetIndexingState(state *appstate.IndexingState)
}

type documentDeleteNotifier interface {
	SetOnDocumentsDeleted(fn func(relPaths []string))
}

type contentHashResetter interface {
	ClearDocumentContentHashes(ctx context.Context) error
}

type embeddedChunkLister interface {
	ListEmbeddedChunkMetadata(ctx context.Context, indexKind string, limit, offset int) ([]model.ChunkTask, error)
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
	readOnly           bool
	public             bool
	forceInsecure      bool
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
	// overrideable models, set via flags or env/config
	embedModelText            string
	embedModelCode            string
	chatModel                 string
	mistralMaxOCRPayloadBytes int
}

type optionalBoolFlag struct {
	value bool
	set   bool
}

func (o *optionalBoolFlag) String() string {
	if o == nil {
		return "false"
	}
	return strconv.FormatBool(o.value)
}

func (o *optionalBoolFlag) Set(s string) error {
	parsed, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	o.value = parsed
	o.set = true
	return nil
}

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
	Transport   string            `json:"transport"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	Session     connectionSession `json:"session"`
	Public      bool              `json:"public"`
	TokenSource string            `json:"token_source"`
	TokenFile   string            `json:"token_file,omitempty"`
}

type ndjsonEvent struct {
	Timestamp string      `json:"ts"`
	Level     string      `json:"level"`
	Event     string      `json:"event"`
	Data      interface{} `json:"data"`
}

type ndjsonEmitter struct {
	enabled bool
	out     io.Writer
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
	Mode            string `json:"mode"`
	Running         bool   `json:"running"`
	Scanned         int64  `json:"scanned"`
	Indexed         int64  `json:"indexed"`
	Skipped         int64  `json:"skipped"`
	Deleted         int64  `json:"deleted"`
	Representations int64  `json:"representations"`
	ChunksTotal     int64  `json:"chunks_total"`
	EmbeddedOK      int64  `json:"embedded_ok"`
	Errors          int64  `json:"errors"`
	Unknown         int64  `json:"unknown"`
}

func NewApp() *App {
	return NewAppWithIO(os.Stdout, os.Stderr)
}

func NewAppWithIO(stdout, stderr io.Writer) *App {
	return &App{
		stdout: stdout,
		stderr: stderr,
		newIngestor: func(cfg config.Config, st model.Store) (model.Ingestor, error) {
			svc, err := ingest.NewService(cfg, st)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(cfg.MistralAPIKey) != "" {
				client := mistral.NewClient(cfg.MistralBaseURL, cfg.MistralAPIKey)
				if cfg.MistralMaxOCRPayloadBytes > 0 {
					client.MaxOCRPayloadBytes = cfg.MistralMaxOCRPayloadBytes
				}
				svc.SetOCR(client)
			}
			return svc, nil
		},
		// default store constructor uses sqlite in the configured state
		// directory.  tests can override via RuntimeHooks.NewStore.
		newStore: func(cfg config.Config) model.Store {
			return store.NewSQLiteStore(filepath.Join(cfg.StateDir, "meta.sqlite"))
		},
	}
}

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
	return app
}

func writef(out io.Writer, format string, args ...interface{}) {
	_, _ = fmt.Fprintf(out, format, args...)
}

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

func (a *App) storeForConfig(cfg config.Config) model.Store {
	if a != nil && a.newStore != nil {
		return a.newStore(cfg)
	}
	return store.NewSQLiteStore(filepath.Join(cfg.StateDir, "meta.sqlite"))
}

func (a *App) Run(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code := a.RunWithContext(ctx, args)
	if errors.Is(ctx.Err(), context.Canceled) && code == exitSuccess {
		return exitSignalInterrupt
	}
	return code
}

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

func (a *App) runCommand(ctx context.Context, globalOpts globalOptions, remaining []string, jsonRequested bool) int {
	switch remaining[0] {
	case "up":
		upOpts, parseErr := parseUpOptions(globalOpts, remaining[1:])
		if parseErr != nil {
			writeCLIError(a.stderr, globalOpts.jsonOutput || argsContainJSONFlag(remaining[1:]), exitConfigInvalid, fmt.Sprintf("invalid up flags: %v", parseErr))
			return exitConfigInvalid
		}
		return a.runUp(ctx, upOpts)
	case "status":
		return a.runStatus(ctx, globalOpts, remaining[1:])
	case "ask":
		return a.runAsk(ctx, globalOpts, remaining[1:])
	case "reindex":
		return a.runReindex(ctx, globalOpts, remaining[1:])
	case "config":
		return a.runConfig(ctx, globalOpts, remaining[1:])
	case "bridge":
		return a.runBridge(ctx, globalOpts, remaining[1:])
	case "version":
		if !globalOpts.quiet {
			writeln(a.stdout, "dir2mcp v"+strings.TrimPrefix(buildinfo.Version, "v"))
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
		{"up", "start the MCP server and begin indexing"},
		{"status", "show server health and corpus stats"},
		{"ask", "query the knowledge base from the CLI"},
		{"reindex", "force a full re-index of all documents"},
		{"bridge", "run helper adapters (for example ElevenLabs webhooks)"},
		{"config", "view or edit configuration"},
		{"version", "print build version"},
	}
	for _, c := range cmds {
		writef(o, "  %-12s %s\n", s.Bold.Render(c[0]), s.Dim.Render(c[1]))
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
		writef(o, "  %-26s %s\n", s.Cyan.Render(f[0]), s.Dim.Render(f[1]))
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
		writef(o, "  %-30s %s\n", s.Cyan.Render(f[0]), s.Dim.Render(f[1]))
	}
	writeln(o)

	writeln(o, s.sectionHeader("Model Flags")+" "+s.Dim.Render("(up)"))
	modelFlags := [][2]string{
		{"--embed-model-text <model>", "embedding model for text chunks"},
		{"--embed-model-code <model>", "embedding model for code chunks"},
		{"--chat-model <model>", "model used for ask / retrieval"},
	}
	for _, f := range modelFlags {
		writef(o, "  %-30s %s\n", s.Cyan.Render(f[0]), s.Dim.Render(f[1]))
	}
	writeln(o)

	writeln(o, s.sectionHeader("Payment Flags")+" "+s.Dim.Render("(up, x402)"))
	x402Flags := [][2]string{
		{"--x402 <mode>", "payment gating: off | on | required"},
		{"--x402-facilitator-url", "x402 facilitator endpoint"},
	}
	for _, f := range x402Flags {
		writef(o, "  %-30s %s\n", s.Cyan.Render(f[0]), s.Dim.Render(f[1]))
	}
}

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
	lines := strings.Split(string(existing), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, prefix) {
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

func (a *App) buildRetrieverForAsk(ctx context.Context, cfg config.Config, st model.Store) (model.Retriever, func(), error) {
	if a != nil && a.newRetriever != nil {
		return a.newRetriever(cfg, st), nil, nil
	}

	textIndexPath := filepath.Join(cfg.StateDir, "vectors_text.hnsw")
	codeIndexPath := filepath.Join(cfg.StateDir, "vectors_code.hnsw")

	textIx := index.NewHNSWIndex(textIndexPath)
	if err := textIx.Load(textIndexPath); err != nil &&
		!errors.Is(err, model.ErrNotImplemented) &&
		!errors.Is(err, os.ErrNotExist) {
		_ = textIx.Close()
		return nil, nil, fmt.Errorf("load text index: %w", err)
	}

	codeIx := index.NewHNSWIndex(codeIndexPath)
	if err := codeIx.Load(codeIndexPath); err != nil &&
		!errors.Is(err, model.ErrNotImplemented) &&
		!errors.Is(err, os.ErrNotExist) {
		_ = textIx.Close()
		_ = codeIx.Close()
		return nil, nil, fmt.Errorf("load code index: %w", err)
	}

	client := mistral.NewClient(cfg.MistralBaseURL, cfg.MistralAPIKey)
	if cfg.MistralMaxOCRPayloadBytes > 0 {
		client.MaxOCRPayloadBytes = cfg.MistralMaxOCRPayloadBytes
	}
	ret := retrieval.NewService(st, textIx, client, client)
	ret.SetCodeIndex(codeIx)
	ret.SetRootDir(cfg.RootDir)
	ret.SetStateDir(cfg.StateDir)
	ret.SetProtocolVersion(cfg.ProtocolVersion)
	ret.SetRAGSystemPrompt(cfg.RAGSystemPrompt)
	ret.SetMaxContextChars(cfg.RAGMaxContextChars)
	ret.SetOversampleFactor(cfg.RAGOversampleFactor)

	if metadataStore, ok := st.(embeddedChunkLister); ok {
		if _, err := preloadEmbeddedChunkMetadata(ctx, metadataStore, ret); err != nil && !errors.Is(err, model.ErrNotImplemented) {
			_ = textIx.Close()
			_ = codeIx.Close()
			return nil, nil, fmt.Errorf("preload embedded chunk metadata: %w", err)
		}
	}

	cleanup := func() {
		_ = textIx.Close()
		_ = codeIx.Close()
	}
	return ret, cleanup, nil
}

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

func emitJSON(out io.Writer, payload interface{}) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(payload)
}

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

func argsContainJSONFlag(args []string) bool {
	for _, arg := range args {
		if isJSONFlagEnabled(arg) {
			return true
		}
	}
	return false
}

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

func writeCLIError(stderr io.Writer, jsonOutput bool, exitCode int, message string, hints ...string) {
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
				Code:    exitCodeLabel(exitCode),
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
				exitCodeLabel(exitCode),
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

func parseGlobalOptions(args []string) (globalOptions, []string, error) {
	opts := globalOptions{}
	remaining := args

	for len(remaining) > 0 {
		arg := remaining[0]
		if _, ok := commands[arg]; ok {
			break
		}
		if newRem, ok, err := tryConsumeGlobalFlagSet(arg, remaining, &opts); ok || err != nil {
			if err != nil {
				return globalOptions{}, nil, err
			}
			remaining = newRem
			continue
		}
		if enabled, matched, err := parseJSONFlagValue(arg); matched {
			if err != nil {
				return globalOptions{}, nil, err
			}
			opts.jsonOutput = enabled
			remaining = remaining[1:]
			continue
		}
		switch arg {
		case "--non-interactive":
			opts.nonInteractive = true
		case "--quiet":
			opts.quiet = true
		default:
			if strings.HasPrefix(arg, "-") {
				return globalOptions{}, nil, fmt.Errorf("unknown global flag: %s", arg)
			}
			return opts, remaining, nil
		}
		remaining = remaining[1:]
	}

	return opts, remaining, nil
}

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

func resolveConfigPath(global globalOptions) string {
	if trimmed := strings.TrimSpace(global.configPath); trimmed != "" {
		return trimmed
	}
	return ".dir2mcp.yaml"
}

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

func saveEffectiveConfigSnapshot(cfg config.Config, auth authMaterial, x402TokenSource string) error {
	sources := config.SecretSourceMetadata{}
	if strings.TrimSpace(cfg.MistralAPIKey) != "" {
		sources.MistralAPIKey = "configured"
		if strings.TrimSpace(os.Getenv("MISTRAL_API_KEY")) != "" {
			sources.MistralAPIKey = "env"
		}
	}
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

func parseUpOptions(global globalOptions, args []string) (upOptions, error) {
	opts := upOptions{globalOptions: global}
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.jsonOutput, "json", opts.jsonOutput, "emit NDJSON events")
	fs.BoolVar(&opts.nonInteractive, "non-interactive", opts.nonInteractive, "disable prompts")
	fs.BoolVar(&opts.readOnly, "read-only", false, "run in read-only mode")
	fs.BoolVar(&opts.public, "public", false, "bind to all interfaces for external access")
	fs.BoolVar(&opts.forceInsecure, "force-insecure", false, "allow public mode without auth (unsafe)")
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
	fs.StringVar(&opts.embedModelText, "embed-model-text", "", "override embedding model used for text chunks")
	fs.StringVar(&opts.embedModelCode, "embed-model-code", "", "override embedding model used for code chunks")
	fs.StringVar(&opts.chatModel, "chat-model", "", "override model used for chat/completions")
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
	if opts.mistralMaxOCRPayloadBytes < 0 {
		return upOptions{}, fmt.Errorf("invalid --mistral-max-ocr-payload-bytes: must be >= 0")
	}
	return opts, nil
}

func (a *App) closeStoreWithLog(st model.Store) {
	if closeErr := st.Close(); closeErr != nil {
		writef(a.stderr, "close store: %v\n", closeErr)
	}
}

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

func prepareAuthMaterial(cfg config.Config) (authMaterial, error) {
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
		return prepareAutoAuthMaterial(cfg)
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

func prepareAutoAuthMaterial(cfg config.Config) (authMaterial, error) {
	if token := strings.TrimSpace(os.Getenv(authTokenEnvVar)); token != "" {
		return authMaterial{
			mode:              "auto",
			token:             token,
			tokenSource:       "env",
			authorizationHint: "Bearer <token-from-env>",
		}, nil
	}
	tokenPath := filepath.Join(cfg.StateDir, secretTokenName)
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

func generateTokenHex() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func writeSecretToken(path, token string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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

func buildConnectionPayload(cfg config.Config, url string, auth authMaterial) connectionPayload {
	headers := map[string]string{
		protocol.MCPProtocolVersionHeader: cfg.ProtocolVersion,
	}
	if auth.mode != "none" {
		headers["Authorization"] = auth.authorizationHint
	}

	return connectionPayload{
		Transport: "mcp_streamable_http",
		URL:       url,
		Headers:   headers,
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

func writeConnectionFile(path string, payload connectionPayload) error {
	content, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	return os.WriteFile(path, content, 0o644)
}

func newNDJSONEmitter(out io.Writer, enabled bool) *ndjsonEmitter {
	return &ndjsonEmitter{enabled: enabled, out: out}
}

func runCorpusWriter(ctx context.Context, stateDir string, st model.Store, indexingState *appstate.IndexingState, stderr io.Writer, emitter *ndjsonEmitter) {
	runCorpusWriterWithInterval(ctx, stateDir, st, indexingState, stderr, emitter, 5*time.Second)
}

func runCorpusWriterWithInterval(ctx context.Context, stateDir string, st model.Store, indexingState *appstate.IndexingState, stderr io.Writer, emitter *ndjsonEmitter, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	// Emit an initial snapshot immediately, then refresh while indexing runs.
	if err := writeCorpusSnapshot(ctx, stateDir, st, indexingState, stderr, emitter); err != nil {
		writef(stderr, "write corpus snapshot: %v\n", err)
	}

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
			if err := writeCorpusSnapshot(ctx, stateDir, st, indexingState, stderr, emitter); err != nil {
				writef(stderr, "write corpus snapshot: %v\n", err)
			}
		}
	}
}

func writeCorpusSnapshot(ctx context.Context, stateDir string, st model.Store, indexingState *appstate.IndexingState, stderr io.Writer, emitter *ndjsonEmitter) error {
	snapshot, err := buildCorpusSnapshot(ctx, st, indexingState, stderr, emitter)
	if err != nil {
		return err
	}

	path := filepath.Join(stateDir, "corpus.json")
	// Use a per-write temporary file so concurrent snapshot writers don't
	// stomp each other's tmp file and trigger spurious ENOENT on rename.
	tmpFile, err := os.CreateTemp(stateDir, "corpus.json.tmp.")
	if err != nil {
		return fmt.Errorf("create temp corpus snapshot: %w", err)
	}
	tmp := tmpFile.Name()

	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("marshal corpus snapshot: %w", err)
	}

	if _, err := tmpFile.Write(raw); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp corpus snapshot: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp corpus snapshot: %w", err)
	}
	// Match previous file mode (0o644) used with os.WriteFile.
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chmod temp corpus snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// os.Rename fails on Windows when the destination already exists.
		// Remove the existing file and retry once to support Windows.
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("rename corpus snapshot: %w", err2)
		}
	}
	return nil
}

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
			Errors:          idx.Errors,
			Unknown:         idx.Unknown,
		},
		DocCounts: docCounts,
		TotalDocs: totalDocs,
		CodeRatio: codeRatio,
	}, nil
}

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

func (e *ndjsonEmitter) Emit(level, event string, data interface{}) {
	if !e.enabled {
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
	_, _ = fmt.Fprintln(e.out, string(encoded))
}

func (a *App) printHumanConnection(cfg config.Config, connection connectionPayload, auth authMaterial, readOnly bool) {
	s := a.sty(false)
	writeln(a.stdout)
	writef(a.stdout, "  %s %s\n", s.banner(), s.dim("v0.0.0-dev"))
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
	writeln(a.stdout, s.separator(44))
	writef(a.stdout, "  %s\n", s.Success.Render("Ready for connections"))
	writef(a.stdout, "  %s\n\n", s.dim("(q + Enter to stop)"))
}

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
