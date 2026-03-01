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
	"log"
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
	"dir2mcp/internal/config"
	"dir2mcp/internal/elevenlabs"
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
	exitSuccess = iota
	exitGeneric
	exitConfigInvalid
	exitRootInaccessible
	exitServerBindFailure
	exitIndexLoadFailure
	exitIngestionFatal
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
	"config":  {},
	"version": {},
}

type App struct {
	stdout io.Writer
	stderr io.Writer

	newIngestor  func(config.Config, model.Store) model.Ingestor
	newStore     func(config.Config) model.Store
	newRetriever func(config.Config, model.Store) model.Retriever

	cachedStyles map[bool]*styles
}

type indexingStateAware interface {
	SetIndexingState(state *appstate.IndexingState)
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
	NewIngestor  func(config.Config, model.Store) model.Ingestor
	NewStore     func(config.Config) model.Store
	NewRetriever func(config.Config, model.Store) model.Retriever
}

type globalOptions struct {
	jsonOutput     bool
	nonInteractive bool
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
	allowedOrigins                string
	// overrideable models, set via flags or env/config
	embedModelText string
	embedModelCode string
	chatModel      string
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
		newIngestor: func(cfg config.Config, st model.Store) model.Ingestor {
			svc := ingest.NewService(cfg, st)
			if strings.TrimSpace(cfg.MistralAPIKey) != "" {
				client := mistral.NewClient(cfg.MistralBaseURL, cfg.MistralAPIKey)
				svc.SetOCR(client)
				svc.SetTranscriber(client)
			}
			return svc
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
	return a.RunWithContext(ctx, args)
}

func (a *App) RunWithContext(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printUsage()
		return exitSuccess
	}

	globalOpts, remaining, err := parseGlobalOptions(args)
	if err != nil {
		writef(a.stderr, "%v\n", err)
		return exitGeneric
	}
	if len(remaining) == 0 {
		a.printUsage()
		return exitSuccess
	}

	switch remaining[0] {
	case "up":
		upOpts, parseErr := parseUpOptions(globalOpts, remaining[1:])
		if parseErr != nil {
			writef(a.stderr, "invalid up flags: %v\n", parseErr)
			return exitConfigInvalid
		}
		return a.runUp(ctx, upOpts)
	case "status":
		return a.runStatus(ctx, globalOpts, remaining[1:])
	case "ask":
		return a.runAsk(ctx, globalOpts, remaining[1:])
	case "reindex":
		return a.runReindex(ctx)
	case "config":
		return a.runConfig(ctx, globalOpts, remaining[1:])
	case "version":
		s := a.sty(globalOpts.jsonOutput)
		writef(a.stdout, "%s %s\n", s.banner(), s.dim("v0.0.0-dev"))
		return exitSuccess
	default:
		writef(a.stderr, "unknown command: %s\n", remaining[0])
		a.printUsage()
		return exitGeneric
	}
}

func (a *App) printUsage() {
	s := a.sty(false)
	writeln(a.stdout)
	writef(a.stdout, "  %s\n", s.banner())
	writeln(a.stdout)
	writef(a.stdout, "  %s dir2mcp [--json] [--non-interactive] <command>\n", s.Dim.Render("Usage:"))
	writeln(a.stdout)
	writef(a.stdout, "  %s\n", s.sectionHeader("Commands"))
	writef(a.stdout, "    %s   %s\n", s.Brand.Render("up      "), s.dim("Start MCP server with indexing"))
	writef(a.stdout, "    %s   %s\n", s.Brand.Render("status  "), s.dim("Show indexing progress & corpus stats"))
	writef(a.stdout, "    %s   %s\n", s.Brand.Render("ask     "), s.dim("Query with RAG answer generation"))
	writef(a.stdout, "    %s   %s\n", s.Brand.Render("reindex "), s.dim("Force re-indexing of all files"))
	writef(a.stdout, "    %s   %s\n", s.Brand.Render("config  "), s.dim("Manage configuration (init, print)"))
	writef(a.stdout, "    %s   %s\n", s.Brand.Render("version "), s.dim("Show version"))
	writeln(a.stdout)
}

func (a *App) runUp(ctx context.Context, opts upOptions) int {
	cfg, err := config.Load(".dir2mcp.yaml")
	if err != nil {
		writef(a.stderr, "load config: %v\n", err)
		return exitConfigInvalid
	}
	// warn the user if a direct facilitator token was supplied but is being
	// ignored in favor of a file path. parseUpOptions recorded the original
	// flag presence in x402FacilitatorTokenDirectSet.
	if opts.x402FacilitatorTokenDirectSet && opts.x402FacilitatorTokenFile != "" {
		writef(a.stderr, "warning: --x402-facilitator-token ignored; using --x402-facilitator-token-file\n")
	}

	if opts.listen != "" {
		cfg.ListenAddr = opts.listen
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		cfg.ListenAddr = protocol.DefaultListenAddr
	}
	if opts.mcpPath != "" {
		cfg.MCPPath = opts.mcpPath
	}
	if strings.TrimSpace(cfg.MCPPath) == "" {
		cfg.MCPPath = protocol.DefaultMCPPath
	}
	if opts.auth != "" {
		cfg.AuthMode = opts.auth
	}
	if opts.allowedOrigins != "" {
		cfg.AllowedOrigins = config.MergeAllowedOrigins(cfg.AllowedOrigins, opts.allowedOrigins)
	}
	if opts.embedModelText != "" {
		cfg.EmbedModelText = opts.embedModelText
	}
	if opts.embedModelCode != "" {
		cfg.EmbedModelCode = opts.embedModelCode
	}
	if strings.TrimSpace(opts.chatModel) != "" {
		cfg.ChatModel = strings.TrimSpace(opts.chatModel)
	}
	if strings.TrimSpace(opts.x402Mode) != "" {
		cfg.X402.Mode = strings.TrimSpace(opts.x402Mode)
	}
	if strings.TrimSpace(opts.x402FacilitatorURL) != "" {
		cfg.X402.FacilitatorURL = strings.TrimSpace(opts.x402FacilitatorURL)
	}
	// precedence: file path > env var > flag
	if opts.x402FacilitatorTokenFile != "" {
		data, err := os.ReadFile(filepath.Clean(opts.x402FacilitatorTokenFile))
		if err != nil {
			writef(a.stderr, "failed to read x402 facilitator token file: %v\n", err)
			return exitConfigInvalid
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			writef(a.stderr, "x402 facilitator token file is empty\n")
			return exitConfigInvalid
		}
		cfg.X402.FacilitatorToken = token
	} else if token := strings.TrimSpace(os.Getenv(x402FacilitatorTokenEnvVar)); token != "" {
		cfg.X402.FacilitatorToken = token
	} else if strings.TrimSpace(opts.x402FacilitatorToken) != "" {
		cfg.X402.FacilitatorToken = strings.TrimSpace(opts.x402FacilitatorToken)
	}
	if strings.TrimSpace(opts.x402ResourceBaseURL) != "" {
		cfg.X402.ResourceBaseURL = strings.TrimSpace(opts.x402ResourceBaseURL)
	}
	if strings.TrimSpace(opts.x402Network) != "" {
		cfg.X402.Network = strings.TrimSpace(opts.x402Network)
	}
	if strings.TrimSpace(opts.x402Price) != "" {
		cfg.X402.PriceAtomic = strings.TrimSpace(opts.x402Price)
	}
	if strings.TrimSpace(opts.x402Scheme) != "" {
		cfg.X402.Scheme = strings.TrimSpace(opts.x402Scheme)
	}
	if strings.TrimSpace(opts.x402Asset) != "" {
		cfg.X402.Asset = strings.TrimSpace(opts.x402Asset)
	}
	if strings.TrimSpace(opts.x402PayTo) != "" {
		cfg.X402.PayTo = strings.TrimSpace(opts.x402PayTo)
	}
	if opts.x402ToolsCallEnabledIsSet {
		cfg.X402.ToolsCallEnabled = opts.x402ToolsCallEnabled
	}
	if opts.public {
		cfg.Public = true

		// Public mode defaults to all interfaces unless operator provided --listen explicitly.
		if opts.listen == "" {
			port := "0"
			if _, parsedPort, splitErr := net.SplitHostPort(cfg.ListenAddr); splitErr == nil && parsedPort != "" {
				port = parsedPort
			}
			cfg.ListenAddr = net.JoinHostPort("0.0.0.0", port)
		}

		authMode := strings.TrimSpace(cfg.AuthMode)
		if strings.EqualFold(authMode, "none") && !opts.forceInsecure {
			se := a.sty(opts.jsonOutput)
			writef(a.stderr, "%s --public requires auth. Use --auth auto or --force-insecure to override (unsafe).\n", se.errPrefix())
			return exitConfigInvalid
		}
	}
	if !strings.HasPrefix(cfg.MCPPath, "/") {
		writeln(a.stderr, "CONFIG_INVALID: --mcp-path must start with '/'")
		return exitConfigInvalid
	}

	strictX402 := strings.EqualFold(strings.TrimSpace(cfg.X402.Mode), "required")
	if err := cfg.ValidateX402(strictX402); err != nil {
		writef(a.stderr, "CONFIG_INVALID: %v\n", err)
		return exitConfigInvalid
	}

	if err := ensureRootAccessible(cfg.RootDir); err != nil {
		writef(a.stderr, "root inaccessible: %v\n", err)
		return exitRootInaccessible
	}

	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		writef(a.stderr, "create state dir: %v\n", err)
		return exitRootInaccessible
	}
	// create payments subdirectory while x402 configuration has been
	// validated above. creating the state directory first ensures the
	// parent exists. the call is intentionally unconditional here: we ensure
	// the directory exists regardless of mode, avoiding inconsistent state.
	// because it's done after x402 validation, a valid config (including
	// mode="off") won't leave an inconsistent state.
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "payments"), 0o755); err != nil {
		writef(a.stderr, "create payments dir: %v\n", err)
		return exitRootInaccessible
	}

	nonInteractiveMode := opts.nonInteractive || !isTerminal(os.Stdin) || !isTerminal(os.Stdout)
	if strings.TrimSpace(cfg.MistralAPIKey) == "" {
		se := a.sty(opts.jsonOutput)
		if nonInteractiveMode {
			writef(a.stderr, "%s CONFIG_INVALID: Missing MISTRAL_API_KEY\n", se.errPrefix())
			writeln(a.stderr, "Set env: MISTRAL_API_KEY=...")
			writeln(a.stderr, "Or run: dir2mcp config init")
		} else {
			writef(a.stderr, "%s Missing MISTRAL_API_KEY\n", se.errPrefix())
			writeln(a.stderr, "Run: dir2mcp config init")
		}
		return exitConfigInvalid
	}

	auth, err := prepareAuthMaterial(cfg)
	if err != nil {
		writef(a.stderr, "auth setup: %v\n", err)
		return exitConfigInvalid
	}
	cfg.AuthMode = auth.mode
	cfg.ResolvedAuthToken = auth.token

	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writef(a.stderr, "initialize metadata store: %v\n", err)
		return exitIndexLoadFailure
	}

	textIndexPath := filepath.Join(cfg.StateDir, "vectors_text.hnsw")
	codeIndexPath := filepath.Join(cfg.StateDir, "vectors_code.hnsw")

	textIx := index.NewHNSWIndex(textIndexPath)
	defer func() {
		_ = textIx.Close()
	}()
	if err := textIx.Load(textIndexPath); err != nil &&
		!errors.Is(err, model.ErrNotImplemented) &&
		!errors.Is(err, os.ErrNotExist) {
		writef(a.stderr, "load text index: %v\n", err)
		return exitIndexLoadFailure
	}

	codeIx := index.NewHNSWIndex(codeIndexPath)
	defer func() {
		_ = codeIx.Close()
	}()
	if err := codeIx.Load(codeIndexPath); err != nil &&
		!errors.Is(err, model.ErrNotImplemented) &&
		!errors.Is(err, os.ErrNotExist) {
		writef(a.stderr, "load code index: %v\n", err)
		return exitIndexLoadFailure
	}

	client := mistral.NewClient(cfg.MistralBaseURL, cfg.MistralAPIKey)
	if strings.TrimSpace(cfg.ChatModel) != "" {
		client.DefaultChatModel = strings.TrimSpace(cfg.ChatModel)
	}
	ret := retrieval.NewService(st, textIx, client, client)
	ret.SetCodeIndex(codeIx)
	ret.SetRootDir(cfg.RootDir)
	ret.SetStateDir(cfg.StateDir)
	ret.SetProtocolVersion(cfg.ProtocolVersion)

	// events are emitted to stdout only after we create the emitter; moving
	// creation before the preload call lets us report failures from that
	// bootstrap step as structured events (see SPEC.md for NDJSON schema).
	emitter := newNDJSONEmitter(a.stdout, opts.jsonOutput)

	preloadedChunks := 0
	if metadataStore, ok := st.(embeddedChunkLister); ok {
		preloadedChunks, err = preloadEmbeddedChunkMetadata(ctx, metadataStore, ret)
		if err != nil {
			// surface the problem in both stderr and the NDJSON event stream so
			// automation can detect a bootstrap warning.
			writef(a.stderr, "bootstrap embedded chunk metadata: %v\n", err)
			emitter.Emit("warning", "bootstrap_embedded_chunk_metadata", map[string]interface{}{
				"message": err.Error(),
			})
		}
	}
	indexingState := appstate.NewIndexingState(appstate.ModeIncremental)
	if preloadedChunks > 0 {
		indexingState.AddEmbeddedOK(int64(preloadedChunks))
	}
	ret.SetIndexingCompleteProvider(func() bool {
		return !indexingState.Snapshot().Running
	})

	serverOptions := []mcp.ServerOption{
		mcp.WithStore(st),
		mcp.WithIndexingState(indexingState),
		mcp.WithEventEmitter(emitter.Emit),
	}
	if strings.TrimSpace(cfg.ElevenLabsAPIKey) != "" {
		ttsClient := elevenlabs.NewClient(cfg.ElevenLabsAPIKey, cfg.ElevenLabsTTSVoiceID)
		if strings.TrimSpace(cfg.ElevenLabsBaseURL) != "" {
			ttsClient.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.ElevenLabsBaseURL), "/")
		}
		serverOptions = append(serverOptions, mcp.WithTTS(ttsClient))
	}

	mcpServer := mcp.NewServer(cfg, ret, serverOptions...)
	ing := a.newIngestor(cfg, st)
	if stateAware, ok := ing.(indexingStateAware); ok {
		stateAware.SetIndexingState(indexingState)
	}

	emitter.Emit("info", "index_loaded", map[string]interface{}{
		"state_dir": cfg.StateDir,
	})

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		writef(a.stderr, "bind server: %v\n", err)
		return exitServerBindFailure
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		_ = ln.Close()
	}()
	persistence := index.NewPersistenceManager(
		[]index.IndexedFile{
			{Path: textIndexPath, Index: textIx},
			{Path: codeIndexPath, Index: codeIx},
		},
		15*time.Second,
		func(saveErr error) { writef(a.stderr, "index autosave warning: %v\n", saveErr) },
	)
	persistence.Start(runCtx)
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if stopErr := persistence.StopAndSave(stopCtx); stopErr != nil && !errors.Is(stopErr, context.Canceled) {
			writef(a.stderr, "final index save warning: %v\n", stopErr)
		}
	}()

	embedErrCh := make(chan error, 4)
	if !opts.readOnly {
		if chunkSource, ok := st.(index.ChunkSource); ok {
			// choose an embed worker logger appropriate for JSON mode so that
			// unstructured log output never leaks into the NDJSON stream.  when
			// in JSON mode we simply discard logs; otherwise forward to the CLI
			// stderr writer (which tests can capture).
			var embedLogger *log.Logger
			if opts.jsonOutput {
				embedLogger = log.New(io.Discard, "", 0)
			} else {
				embedLogger = log.New(a.stderr, "", log.LstdFlags)
			}
			startEmbeddingWorkers(runCtx, chunkSource, textIx, codeIx, client, ret, indexingState, embedErrCh, embedLogger, cfg.EmbedModelText, cfg.EmbedModelCode)
		}
	}
	mcpAddr := ln.Addr().String()
	if cfg.Public {
		mcpAddr = publicURLAddress(cfg.ListenAddr, mcpAddr)
	}
	mcpURL := buildMCPURL(mcpAddr, cfg.MCPPath)

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- mcpServer.RunOnListener(runCtx, ln)
	}()

	emitter.Emit("info", "server_started", map[string]interface{}{
		"url":         mcpURL,
		"listen_addr": ln.Addr().String(),
		"public":      cfg.Public,
	})

	connection := buildConnectionPayload(cfg, mcpURL, auth)
	if err := writeConnectionFile(filepath.Join(cfg.StateDir, connectionFileName), connection); err != nil {
		writef(a.stderr, "write %s: %v\n", connectionFileName, err)
		return exitGeneric
	}

	emitter.Emit("info", "connection", connection)
	emitter.Emit("info", "scan_progress", map[string]interface{}{
		"scanned": 0,
		"indexed": 0,
		"skipped": 0,
		"deleted": 0,
		"reps":    0,
		"chunks":  0,
		"errors":  0,
	})
	emitter.Emit("info", "embed_progress", map[string]interface{}{
		"embedded": 0,
		"chunks":   0,
		"errors":   0,
	})

	if !opts.jsonOutput {
		a.printHumanConnection(cfg, connection, auth, opts.readOnly)
	}

	ingestErrCh := make(chan error, 1)
	go runCorpusWriter(runCtx, cfg.StateDir, st, indexingState, a.stderr, emitter)

	if opts.readOnly {
		close(ingestErrCh)
	} else {
		go func() {
			defer close(ingestErrCh)
			// mode is already set at creation time; just mark running state
			indexingState.SetRunning(true)
			defer indexingState.SetRunning(false)
			runErr := ing.Run(runCtx)
			if errors.Is(runErr, model.ErrNotImplemented) {
				ingestErrCh <- nil
				return
			}
			ingestErrCh <- runErr
		}()
	}

	for {
		select {
		case <-runCtx.Done():
			return exitSuccess
		case serverErr := <-serverErrCh:
			if serverErr != nil {
				writef(a.stderr, "server failed: %v\n", serverErr)
				emitter.Emit("error", "fatal", map[string]interface{}{
					"code":    "SERVER_FAILURE",
					"message": serverErr.Error(),
				})
				return exitGeneric
			}
			return exitSuccess
		case ingestErr, ok := <-ingestErrCh:
			if !ok {
				ingestErrCh = nil
				_ = writeCorpusSnapshot(runCtx, cfg.StateDir, st, indexingState, a.stderr, emitter)
				continue
			}
			if ingestErr == nil {
				_ = writeCorpusSnapshot(runCtx, cfg.StateDir, st, indexingState, a.stderr, emitter)
				continue
			}
			writef(a.stderr, "ingestion failed: %v\n", ingestErr)
			emitter.Emit("error", "file_error", map[string]interface{}{
				"message": ingestErr.Error(),
			})
			emitter.Emit("error", "fatal", map[string]interface{}{
				"code":    "INGESTION_FATAL",
				"message": ingestErr.Error(),
			})
			return exitIngestionFatal
		case embedErr := <-embedErrCh:
			if embedErr == nil {
				continue
			}
			writef(a.stderr, "embedding worker warning: %v\n", embedErr)
			emitter.Emit("error", "embed_error", map[string]interface{}{
				"message": embedErr.Error(),
			})
		}
	}
}

func preloadEmbeddedChunkMetadata(ctx context.Context, source embeddedChunkLister, ret *retrieval.Service) (int, error) {
	if source == nil || ret == nil {
		return 0, nil
	}
	const pageSize = 500
	total := 0
	kinds := []string{"text", "code"}
	for _, kind := range kinds {
		offset := 0
		for {
			tasks, err := source.ListEmbeddedChunkMetadata(ctx, kind, pageSize, offset)
			if err != nil {
				if errors.Is(err, model.ErrNotImplemented) {
					break
				}
				return total, err
			}
			for _, task := range tasks {
				ret.SetChunkMetadataForIndex(kind, task.Metadata.ChunkID, model.SearchHit{
					ChunkID: task.Metadata.ChunkID,
					RelPath: task.Metadata.RelPath,
					DocType: task.Metadata.DocType,
					RepType: task.Metadata.RepType,
					Snippet: task.Metadata.Snippet,
					Span:    task.Metadata.Span,
				})
				total++
			}
			if len(tasks) < pageSize {
				break
			}
			offset += len(tasks)
		}
	}
	return total, nil
}

func startEmbeddingWorkers(
	ctx context.Context,
	st index.ChunkSource,
	textIndex model.Index,
	codeIndex model.Index,
	embedder model.Embedder,
	ret *retrieval.Service,
	indexingState *appstate.IndexingState,
	errCh chan<- error,
	logger *log.Logger,
	textModel, codeModel string,
) {
	if st == nil || embedder == nil {
		return
	}

	start := func(kind string, ix model.Index) {
		if ix == nil {
			return
		}
		workerKind := kind
		worker := &index.EmbeddingWorker{
			Source:       st,
			Index:        ix,
			Embedder:     embedder,
			ModelForText: textModel,
			ModelForCode: codeModel,
			BatchSize:    32,
			Logger:       logger,
			OnIndexedChunk: func(label uint64, metadata model.ChunkMetadata) {
				if ret != nil {
					ret.SetChunkMetadataForIndex(workerKind, label, model.SearchHit{
						ChunkID: metadata.ChunkID,
						RelPath: metadata.RelPath,
						DocType: metadata.DocType,
						RepType: metadata.RepType,
						Snippet: metadata.Snippet,
						Span:    metadata.Span,
					})
				}
				if indexingState != nil {
					indexingState.AddEmbeddedOK(1)
				}
			},
		}

		go func() {
			err := worker.Run(ctx, 750*time.Millisecond, workerKind)
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			errCh <- fmt.Errorf("%s worker: %w", workerKind, err)
		}()
	}

	start("text", textIndex)
	start("code", codeIndex)
}

func (a *App) runStatus(ctx context.Context, global globalOptions, args []string) int {
	if len(args) > 0 {
		writef(a.stderr, "status command does not accept arguments: %s\n", strings.Join(args, " "))
		return exitGeneric
	}

	cfg, err := config.Load(".dir2mcp.yaml")
	if err != nil {
		writef(a.stderr, "load config: %v\n", err)
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
				writeln(a.stderr, "no state found in .dir2mcp; run: dir2mcp up")
				return exitGeneric
			}
			writef(a.stderr, "read state: %v\n", statErr)
			return exitGeneric
		}

		st := a.storeForConfig(cfg)
		defer func() { _ = st.Close() }()
		if initErr := st.Init(ctx); initErr != nil && !errors.Is(initErr, model.ErrNotImplemented) {
			writef(a.stderr, "initialize metadata store: %v\n", initErr)
			return exitIndexLoadFailure
		}
		emitter := newNDJSONEmitter(a.stdout, global.jsonOutput)
		snapshot, err = buildCorpusSnapshot(ctx, st, nil, a.stderr, emitter)
		if err != nil {
			writef(a.stderr, "build status snapshot: %v\n", err)
			return exitGeneric
		}
		source = "computed"
	}

	if global.jsonOutput {
		payload := map[string]interface{}{
			"source":    source,
			"state_dir": cfg.StateDir,
			"snapshot":  snapshot,
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writef(a.stderr, "encode status json: %v\n", err)
			return exitGeneric
		}
		return exitSuccess
	}

	s := a.sty(false)
	writeln(a.stdout)
	writeln(a.stdout, s.kv("State", cfg.StateDir))
	writeln(a.stdout, s.kv("Source", source))
	writeln(a.stdout, s.kv("Timestamp", snapshot.Timestamp))
	writeln(a.stdout)

	runningLabel := s.dim("stopped")
	if snapshot.Indexing.Running {
		runningLabel = s.Green.Render("running")
	}

	writef(a.stdout, "  %s  %s  %s\n", s.sectionHeader("Indexing"), s.dim("mode="+snapshot.Indexing.Mode), runningLabel)
	writef(a.stdout, "    %s  %s  %s  %s\n",
		s.stat("scanned", snapshot.Indexing.Scanned),
		s.stat("indexed", snapshot.Indexing.Indexed),
		s.stat("skipped", snapshot.Indexing.Skipped),
		s.stat("deleted", snapshot.Indexing.Deleted),
	)
	writef(a.stdout, "    %s  %s  %s  %s",
		s.stat("reps", snapshot.Indexing.Representations),
		s.stat("chunks", snapshot.Indexing.ChunksTotal),
		s.stat("embedded", snapshot.Indexing.EmbeddedOK),
		s.stat("unknown", snapshot.Indexing.Unknown),
	)
	if snapshot.Indexing.Errors > 0 {
		writef(a.stdout, "  %s", s.Red.Render(fmt.Sprintf("errors=%d", snapshot.Indexing.Errors)))
	} else {
		writef(a.stdout, "  %s", s.stat("errors", snapshot.Indexing.Errors))
	}
	writeln(a.stdout)
	writeln(a.stdout)

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

func (a *App) runAsk(ctx context.Context, global globalOptions, args []string) int {
	opts, err := parseAskOptions(args)
	if err != nil {
		writef(a.stderr, "invalid ask flags: %v\n", err)
		return exitGeneric
	}

	cfg, err := config.Load(".dir2mcp.yaml")
	if err != nil {
		writef(a.stderr, "load config: %v\n", err)
		return exitConfigInvalid
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(".", ".dir2mcp")
	}

	if strings.TrimSpace(cfg.MistralAPIKey) == "" {
		se := a.sty(global.jsonOutput)
		nonInteractiveMode := global.nonInteractive || !isTerminal(os.Stdin) || !isTerminal(os.Stdout)
		if nonInteractiveMode {
			writef(a.stderr, "%s CONFIG_INVALID: Missing MISTRAL_API_KEY\n", se.errPrefix())
			writeln(a.stderr, "Set env: MISTRAL_API_KEY=...")
			writeln(a.stderr, "Or run: dir2mcp config init")
		} else {
			writef(a.stderr, "%s Missing MISTRAL_API_KEY\n", se.errPrefix())
			writeln(a.stderr, "Run: dir2mcp config init")
		}
		return exitConfigInvalid
	}

	st := a.storeForConfig(cfg)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writef(a.stderr, "initialize metadata store: %v\n", err)
		return exitIndexLoadFailure
	}

	retriever, cleanup, err := a.buildRetrieverForAsk(ctx, cfg, st)
	if err != nil {
		writef(a.stderr, "initialize retriever: %v\n", err)
		return exitIndexLoadFailure
	}
	if cleanup != nil {
		defer cleanup()
	}

	query := model.SearchQuery{
		Query:      opts.question,
		K:          opts.k,
		Index:      opts.index,
		PathPrefix: opts.pathPrefix,
		FileGlob:   opts.fileGlob,
		DocTypes:   opts.docTypes,
	}

	if opts.mode == "search_only" {
		hits, searchErr := retriever.Search(ctx, query)
		if searchErr != nil {
			writef(a.stderr, "ask failed: %v\n", searchErr)
			return exitGeneric
		}
		if global.jsonOutput {
			// JSON output now uses a dedicated accessor instead of running Ask
			// again.  this avoids the extra search/generation work while still
			// providing the same boolean value returned by AskResult.IndexingComplete.
			indexingComplete := true
			if ic, err := retriever.IndexingComplete(ctx); err == nil {
				indexingComplete = ic
			}

			payload := map[string]interface{}{
				"question":          opts.question,
				"answer":            "",
				"citations":         []interface{}{},
				"hits":              serializeHits(hits),
				"indexing_complete": indexingComplete,
			}
			if err := emitJSON(a.stdout, payload); err != nil {
				writef(a.stderr, "encode ask json: %v\n", err)
				return exitGeneric
			}
			return exitSuccess
		}

		s := a.sty(false)
		writeln(a.stdout)
		writef(a.stdout, "  %s %s\n\n", s.sectionHeader("Search results"), s.dim(fmt.Sprintf("(%d hits)", len(hits))))
		for i, hit := range hits {
			snippet := strings.TrimSpace(hit.Snippet)
			if snippet == "" {
				snippet = "(no snippet)"
			}
			writef(a.stdout, "  %s %s  %s\n", s.Brand.Render(fmt.Sprintf("[%d]", i+1)), s.Cyan.Render(hit.RelPath), s.dim(fmt.Sprintf("score=%.4f", hit.Score)))
			writef(a.stdout, "      %s\n", s.dim(snippet))
		}
		writeln(a.stdout)
		return exitSuccess
	}

	askResult, askErr := retriever.Ask(ctx, opts.question, query)
	if askErr != nil {
		writef(a.stderr, "ask failed: %v\n", askErr)
		return exitGeneric
	}

	if global.jsonOutput {
		payload := map[string]interface{}{
			"question":          askResult.Question,
			"answer":            askResult.Answer,
			"citations":         serializeCitations(askResult.Citations),
			"hits":              serializeHits(askResult.Hits),
			"indexing_complete": askResult.IndexingComplete,
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writef(a.stderr, "encode ask json: %v\n", err)
			return exitGeneric
		}
		return exitSuccess
	}

	s := a.sty(false)
	writeln(a.stdout)
	writeln(a.stdout, askResult.Answer)
	if len(askResult.Citations) > 0 {
		writeln(a.stdout)
		writef(a.stdout, "  %s\n", s.sectionHeader("Citations"))
		for i, citation := range askResult.Citations {
			writef(a.stdout, "  %s %s  %s\n",
				s.Brand.Render(fmt.Sprintf("[%d]", i+1)),
				s.Cyan.Render(citation.RelPath),
				s.dim(fmt.Sprintf("chunk=%d span=%s", citation.ChunkID, formatSpan(citation.Span))),
			)
		}
	}
	writeln(a.stdout)
	return exitSuccess
}

func (a *App) runReindex(ctx context.Context) int {
	// load configuration first so that both the ingestor and any
	// auxiliary components (OCR client) share the same settings.  When
	// Load returns an error we treat it as fatal instead of silently
	// proceeding with defaults as was previously the case.
	cfg, err := config.Load(".dir2mcp.yaml")
	if err != nil {
		writef(a.stderr, "load config: %v\n", err)
		return exitConfigInvalid
	}

	baseDir := strings.TrimSpace(cfg.StateDir)
	if baseDir == "" {
		baseDir = ".dir2mcp"
	}
	// ensure the directory exists before we let the store constructor write
	// to it
	textIndexPath := filepath.Join(baseDir, "vectors_text.hnsw")
	codeIndexPath := filepath.Join(baseDir, "vectors_code.hnsw")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		writef(a.stderr, "create state dir: %v\n", err)
		return exitRootInaccessible
	}
	// update cfg so that the store factory uses the same baseDir
	cfg.StateDir = baseDir
	st := a.storeForConfig(cfg)
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			writef(a.stderr, "close store: %v\n", closeErr)
		}
	}()
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writef(a.stderr, "initialize metadata store: %v\n", err)
		return exitIndexLoadFailure
	}
	if resetter, ok := interface{}(st).(contentHashResetter); ok {
		if err := resetter.ClearDocumentContentHashes(ctx); err != nil {
			writef(a.stderr, "clear content hashes: %v\n", err)
			return exitGeneric
		}
	}
	for _, indexPath := range []string{textIndexPath, codeIndexPath} {
		if err := os.Remove(indexPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			writef(a.stderr, "remove stale index file %s: %v\n", indexPath, err)
			return exitGeneric
		}
	}

	// use the factory hook (same as runUp) to allow tests to intercept
	ing := a.newIngestor(cfg, st)

	err = ing.Reindex(ctx)
	if errors.Is(err, model.ErrNotImplemented) {
		writeln(a.stdout, "reindex is not available yet: ingestion pipeline not implemented")
		return exitSuccess
	}
	if err != nil {
		writef(a.stderr, "reindex failed: %v\n", err)
		return exitGeneric
	}
	return exitSuccess
}

func (a *App) runConfig(ctx context.Context, global globalOptions, args []string) int {
	if len(args) == 0 {
		writeln(a.stdout, "config command: supported subcommands are init and print")
		return exitSuccess
	}
	switch args[0] {
	case "init":
		return a.runConfigInit(global, args[1:])
	case "print":
		cfg, err := config.Load(".dir2mcp.yaml")
		if err != nil {
			writef(a.stderr, "load config: %v\n", err)
			return exitConfigInvalid
		}
		writef(
			a.stdout,
			"root=%s state_dir=%s listen=%s mcp_path=%s mistral_base_url=%s mistral_api_key_set=%t\n",
			cfg.RootDir,
			cfg.StateDir,
			cfg.ListenAddr,
			cfg.MCPPath,
			cfg.MistralBaseURL,
			cfg.MistralAPIKey != "",
		)
	default:
		writef(a.stderr, "unknown config subcommand: %s\n", args[0])
		return exitGeneric
	}
	return exitSuccess
}

func (a *App) runConfigInit(global globalOptions, args []string) int {
	if len(args) > 0 {
		writef(a.stderr, "config init does not accept arguments: %s\n", strings.Join(args, " "))
		return exitGeneric
	}

	configPath := ".dir2mcp.yaml"
	cfg := config.Default()
	created := false

	if _, err := os.Stat(configPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			writef(a.stderr, "stat config: %v\n", err)
			return exitGeneric
		}
		created = true
	} else {
		existing, err := config.LoadFile(configPath)
		if err != nil {
			writef(a.stderr, "load config file: %v\n", err)
			return exitConfigInvalid
		}
		cfg = existing
	}

	if err := config.SaveFile(configPath, cfg); err != nil {
		writef(a.stderr, "save config file: %v\n", err)
		return exitGeneric
	}

	nextSteps := []string{
		"Set env: MISTRAL_API_KEY=...",
		"Or add MISTRAL_API_KEY to .env.local/.env",
		"Run: dir2mcp up",
	}

	if global.jsonOutput {
		payload := map[string]interface{}{
			"path":       configPath,
			"created":    created,
			"updated":    !created,
			"next_steps": nextSteps,
		}
		if err := emitJSON(a.stdout, payload); err != nil {
			writef(a.stderr, "encode config init json: %v\n", err)
			return exitGeneric
		}
		return exitSuccess
	}

	s := a.sty(false)
	if created {
		writef(a.stdout, "%s created %s with baseline settings\n", s.Success.Render("✓"), configPath)
	} else {
		writef(a.stdout, "%s updated %s and ensured baseline settings are present\n", s.Success.Render("✓"), configPath)
	}
	writef(a.stdout, "\n%s\n", s.sectionHeader("Next steps"))
	for _, step := range nextSteps {
		writef(a.stdout, "  %s %s\n", s.dim("•"), step)
	}
	writeln(a.stdout)
	return exitSuccess
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
	ret := retrieval.NewService(st, textIx, client, client)
	ret.SetCodeIndex(codeIx)
	ret.SetRootDir(cfg.RootDir)
	ret.SetStateDir(cfg.StateDir)
	ret.SetProtocolVersion(cfg.ProtocolVersion)

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
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
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

func parseGlobalOptions(args []string) (globalOptions, []string, error) {
	opts := globalOptions{}
	remaining := args

	for len(remaining) > 0 {
		arg := remaining[0]
		if _, ok := commands[arg]; ok {
			break
		}

		switch arg {
		case "--json":
			opts.jsonOutput = true
			remaining = remaining[1:]
		case "--non-interactive":
			opts.nonInteractive = true
			remaining = remaining[1:]
		default:
			if strings.HasPrefix(arg, "-") {
				return globalOptions{}, nil, fmt.Errorf("unknown global flag: %s", arg)
			}
			return opts, remaining, nil
		}
	}

	return opts, remaining, nil
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
	return opts, nil
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

func buildMCPURL(addr, path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + addr + path
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
		parts := make([]string, 0, len(unexpectedStatusCounts))
		for statusVal, count := range unexpectedStatusCounts {
			example := unexpectedStatusExample[statusVal]
			parts = append(parts, fmt.Sprintf("%s=%d (example rel_path=%q)", statusVal, count, example))
		}
		sort.Strings(parts)
		msg := fmt.Sprintf("unexpected document statuses encountered during scan: %s", strings.Join(parts, ", "))
		if emitter != nil && emitter.enabled {
			emitter.Emit("warning", "unexpected_document_statuses", map[string]interface{}{
				"message":  msg,
				"counts":   unexpectedStatusCounts,
				"examples": unexpectedStatusExample,
			})
		} else {
			writef(stderr, "warning: %s\n", msg)
		}
	}

	return counts, nil
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
	writef(a.stdout, "    %s %s\n", s.Dim.Render(protocol.MCPProtocolVersionHeader+":"), cfg.ProtocolVersion)
	if auth.mode != "none" {
		writef(a.stdout, "    %s %s\n", s.Dim.Render("Authorization:"), "Bearer <token>")
	}
	writef(a.stdout, "    %s %s\n", s.Dim.Render(protocol.MCPSessionHeader+":"), s.dim("(assigned after initialize response)"))
	writeln(a.stdout)
	writeln(a.stdout, s.separator(44))
	writef(a.stdout, "  %s\n\n", s.Success.Render("Ready for connections"))
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
