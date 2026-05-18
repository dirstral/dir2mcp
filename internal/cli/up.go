package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/buildinfo"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/identity"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

func (a *App) runUp(ctx context.Context, opts upOptions) int {
	// When this is the launching parent (not the daemon child) and the
	// caller hasn't asked for foreground/JSON behavior, fork a detached
	// child to run the server and exit the parent once the child is
	// ready. The child re-enters runUp with daemonChildEnv set and falls
	// through to the in-process body below.
	if shouldDaemonize(a, opts) {
		return a.runUpAsDaemonParent(ctx, opts)
	}

	cfg, auth, tlsCertFile, tlsKeyFile, nonInteractiveMode, code := a.prepareUpConfig(opts)
	if code != exitSuccess {
		return code
	}

	st, textIx, codeIx, code := a.initStoreAndIndices(ctx, &cfg, opts.jsonOutput)
	if code != exitSuccess {
		return code
	}
	defer func() { _ = st.Close() }()
	textIndexPath := filepath.Join(cfg.StateDir, "vectors_text.hnsw")
	codeIndexPath := filepath.Join(cfg.StateDir, "vectors_code.hnsw")
	defer func() { _ = textIx.Close() }()
	defer func() { _ = codeIx.Close() }()

	embedder, generator, etm, ecm := a.resolveModelClients(cfg)
	ret := retrieval.NewService(st, textIx, embedder, generator)
	ret.SetQueryEmbeddingModel(etm)
	ret.SetCodeEmbeddingModel(ecm)
	ret.SetCodeIndex(codeIx)
	ret.SetRootDir(cfg.RootDir)
	ret.SetStateDir(cfg.StateDir)
	ret.SetProtocolVersion(cfg.ProtocolVersion)
	ret.SetRAGSystemPrompt(cfg.RAGSystemPrompt)
	ret.SetMaxContextChars(cfg.RAGMaxContextChars)
	ret.SetOversampleFactor(cfg.RAGOversampleFactor)
	a.configureReranker(ret, cfg)

	// events are emitted to stdout only after we create the emitter; moving
	// creation before the preload call lets us report failures from that
	// bootstrap step as structured events (see dirstral-spec/docs/SPEC.md for NDJSON schema).
	emitter := newNDJSONEmitter(a.stdout, opts.jsonOutput)

	indexingState := initIndexingState(ctx, st, ret, emitter, a.stderr)
	ret.SetIndexingCompleteProvider(func() bool {
		return !indexingState.Snapshot().Running
	})

	serverOptions := buildMCPServerOptions(&cfg, st, indexingState, emitter)
	mcpServer := mcp.NewServer(cfg, ret, serverOptions...)
	ing, err := a.newIngestor(cfg, st)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("initialize ingestor: %v", err))
		return exitConfigInvalid
	}
	wireIngestorHooks(ing, indexingState, ret.EvictDocuments)

	emitter.Emit("info", "index_loaded", map[string]interface{}{
		"state_dir": cfg.StateDir,
	})

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitServerBindFailure, fmt.Sprintf("bind server: %v", err))
		return exitServerBindFailure
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = ln.Close() }()

	cleanupPIDFile, code := a.setupDaemonChildIfApplicable(cfg.StateDir, opts, cancel)
	if code != exitSuccess {
		return code
	}
	defer cleanupPIDFile()

	persistence := index.NewPersistenceManager(
		[]index.IndexedFile{
			{Path: textIndexPath, Index: textIx},
			{Path: codeIndexPath, Index: codeIx},
		},
		15*time.Second,
		func(saveErr error) { writef(a.stderr, "index autosave warning: %v\n", saveErr) },
	)
	persistence.Start(runCtx)
	defer a.stopPersistenceWithLog(persistence)

	embedErrCh := make(chan error, 4)
	startEmbeddingIfNotReadOnly(runCtx, opts.readOnly, st, textIx, codeIx, embedder, ret, indexingState, embedErrCh, a.stderr, opts.jsonOutput, etm, ecm)

	mcpAddr := ln.Addr().String()
	if cfg.Public {
		mcpAddr = publicURLAddress(cfg.ListenAddr, mcpAddr)
	}
	mcpURL := buildMCPURL(mcpAddr, cfg.MCPPath, tlsCertFile != "")

	transport := mcp.NewSDKTransport(mcpServer, ln, tlsCertFile, tlsKeyFile)

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- transport.Serve(runCtx, mcpServer.Handler())
	}()

	emitter.Emit("info", "server_started", map[string]interface{}{
		"url":         mcpURL,
		"listen_addr": ln.Addr().String(),
		"public":      cfg.Public,
	})

	connection, code := a.publishConnection(cfg, mcpURL, auth, emitter, opts)
	if code != exitSuccess {
		return code
	}

	stdinQuitCh := a.installInteractionForUp(cancel, cfg, connection, auth, opts, nonInteractiveMode)

	ingestErrCh := make(chan error, 1)
	go runCorpusWriter(runCtx, cfg.StateDir, st, indexingState, a.stderr, emitter)
	startIngestWorker(runCtx, opts.readOnly, ing, indexingState, ingestErrCh)

	return a.runEventLoop(runCtx, cancel, &cfg, st, indexingState, emitter, serverErrCh, ingestErrCh, embedErrCh, stdinQuitCh)
}

// applyTLSConfig resolves TLS cert/key from opts and cfg, validates them, and
// returns the effective paths along with an exit code (exitSuccess on success).
func (a *App) applyTLSConfig(cfg *config.Config, opts upOptions) (tlsCertFile, tlsKeyFile string, code int) {
	tlsCertFile = strings.TrimSpace(opts.tlsCert)
	if tlsCertFile == "" {
		tlsCertFile = strings.TrimSpace(cfg.ServerTLSCertFile)
	}
	tlsKeyFile = strings.TrimSpace(opts.tlsKey)
	if tlsKeyFile == "" {
		tlsKeyFile = strings.TrimSpace(cfg.ServerTLSKeyFile)
	}
	if err := validateTLSFlags(tlsCertFile, tlsKeyFile); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("CONFIG_INVALID: %v", err))
		return "", "", exitConfigInvalid
	}
	cfg.ServerTLSCertFile = tlsCertFile
	cfg.ServerTLSKeyFile = tlsKeyFile
	return tlsCertFile, tlsKeyFile, exitSuccess
}

// applyUpFlagOverrides applies all CLI flag overrides to cfg and resolves the
// x402 token source. It returns the token source label and an exit code.
func (a *App) applyUpFlagOverrides(cfg *config.Config, opts upOptions) (x402TokenSource string, code int) {
	// warn when both direct token and token file are supplied
	if opts.x402FacilitatorTokenDirectSet && opts.x402FacilitatorTokenFile != "" {
		writef(a.stderr, "warning: --x402-facilitator-token ignored; using --x402-facilitator-token-file\n")
	}

	applyScalarOverrides(cfg, opts)

	src, code := a.resolveX402Token(cfg, opts)
	if code != exitSuccess {
		return "", code
	}
	return src, exitSuccess
}

// applyScalarOverrides applies simple flag-to-config assignments that have no
// side-effects (no I/O, no validation).
func applyScalarOverrides(cfg *config.Config, opts upOptions) {
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
	applyX402Overrides(cfg, opts)
	resolveServerName(cfg)
}

// resolveServerName finalizes cfg.ServerName so downstream code (mcp
// server, banner) can read a non-empty value without re-deriving the
// auto-name. The override (if any) wins; otherwise we hash the absolute
// RootDir so the name is stable across cwd changes and reinstalls.
//
// Dev builds use a `dir2mcp-dev-` prefix so a developer running their
// in-tree binary alongside the brew-installed release sees two visibly
// distinct entries in `claude mcp list` instead of colliding on the
// same auto-derived name.
func resolveServerName(cfg *config.Config) {
	abs, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		abs = cfg.RootDir
	}
	cfg.ServerName = identity.Resolve(abs, cfg.ServerName, buildinfo.IsDev())
}

// applyX402Overrides applies x402-specific flag overrides to cfg.
func applyX402Overrides(cfg *config.Config, opts upOptions) {
	if strings.TrimSpace(opts.x402Mode) != "" {
		cfg.X402.Mode = strings.TrimSpace(opts.x402Mode)
	}
	if strings.TrimSpace(opts.x402FacilitatorURL) != "" {
		cfg.X402.FacilitatorURL = strings.TrimSpace(opts.x402FacilitatorURL)
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
}

// resolveX402Token resolves the x402 facilitator token from file, env, flag,
// or existing config. Returns the source label and an exit code.
func (a *App) resolveX402Token(cfg *config.Config, opts upOptions) (source string, code int) {
	// precedence: file path > env var > flag > pre-configured
	if opts.x402FacilitatorTokenFile != "" {
		data, err := os.ReadFile(filepath.Clean(opts.x402FacilitatorTokenFile))
		if err != nil {
			writeCLIError(a.stderr, opts.jsonOutput, exitAuthOrPayment, fmt.Sprintf("failed to read x402 facilitator token file: %v", err))
			return "", exitAuthOrPayment
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			writeCLIError(a.stderr, opts.jsonOutput, exitAuthOrPayment, "x402 facilitator token file is empty")
			return "", exitAuthOrPayment
		}
		cfg.X402.FacilitatorToken = token
		return "file", exitSuccess
	}
	if token := strings.TrimSpace(os.Getenv(x402FacilitatorTokenEnvVar)); token != "" {
		cfg.X402.FacilitatorToken = token
		return "env", exitSuccess
	}
	if strings.TrimSpace(opts.x402FacilitatorToken) != "" {
		cfg.X402.FacilitatorToken = strings.TrimSpace(opts.x402FacilitatorToken)
		return "flag", exitSuccess
	}
	if strings.TrimSpace(cfg.X402.FacilitatorToken) != "" {
		return "configured", exitSuccess
	}
	return "", exitSuccess
}

// validateUpConfig runs all post-override config validations (public mode,
// MCP path prefix, x402, root/state directories).
func (a *App) validateUpConfig(cfg *config.Config, opts upOptions) int {
	if cfg.Public || opts.public {
		if code := a.applyPublicMode(cfg, opts); code != exitSuccess {
			return code
		}
	}
	if !strings.HasPrefix(cfg.MCPPath, "/") {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, "CONFIG_INVALID: --mcp-path must start with '/'")
		return exitConfigInvalid
	}
	strictX402 := strings.EqualFold(strings.TrimSpace(cfg.X402.Mode), "required")
	if err := cfg.ValidateX402(strictX402); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("x402 configuration invalid: %v", err))
		return exitConfigInvalid
	}
	if strings.EqualFold(strings.TrimSpace(cfg.IngestExtractor), "docling") && ingest.DocumentExtractorFromConfig(*cfg) == nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, "CONFIG_INVALID: ingest.extractor=docling but docling command is unavailable")
		return exitConfigInvalid
	}
	if err := ensureRootAccessible(cfg.RootDir); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitRootInaccessible, fmt.Sprintf("root inaccessible: %v", err))
		return exitRootInaccessible
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitRootInaccessible, fmt.Sprintf("create state dir: %v", err))
		return exitRootInaccessible
	}
	// create payments subdirectory while x402 configuration has been
	// validated above. creating the state directory first ensures the
	// parent exists. the call is intentionally unconditional here: we ensure
	// the directory exists regardless of mode, avoiding inconsistent state.
	// because it's done after x402 validation, a valid config (including
	// mode="off") won't leave an inconsistent state.
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "payments"), 0o755); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitRootInaccessible, fmt.Sprintf("create payments dir: %v", err))
		return exitRootInaccessible
	}
	return exitSuccess
}

// applyPublicMode enforces public-mode listen address and auth rules.
func (a *App) applyPublicMode(cfg *config.Config, opts upOptions) int {
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
		writeCLIError(
			a.stderr,
			opts.jsonOutput,
			exitConfigInvalid,
			"--public requires auth. Use --auth auto or --force-insecure to override (unsafe).",
		)
		return exitConfigInvalid
	}
	return exitSuccess
}

// checkMistralAPIKey is the §2.5 startup preflight for the embedding
// provider (SPEC 8.1.3): it requires that an embed provider *resolves*
// — a credentialed or credential-less profile that can serve embed.
// Mistral via MISTRAL_API_KEY remains the default that satisfies it.
// (Name retained to avoid caller churn.)
func (a *App) checkMistralAPIKey(cfg *config.Config, opts upOptions, nonInteractiveMode bool) int {
	if !requiresMistralAPIKey(opts) {
		return exitSuccess
	}
	if _, embErr := cfg.Providers().Resolve(provider.CapEmbed); embErr == nil {
		return exitSuccess
	} else {
		msg := "CONFIG_INVALID: no embedding provider configured"
		hint := "Set a provider credential (e.g. MISTRAL_API_KEY / OPENAI_API_KEY) or configure providers: in .dir2mcp.yaml"
		var ce *provider.ConfigError
		if errors.As(embErr, &ce) {
			// Surface the actionable reason (bad explicit binding /
			// incapable kind) instead of the generic message
			// (ce.Error() is already CONFIG_INVALID-prefixed).
			msg = ce.Error()
		}
		return a.reportEmbedPreflightError(opts, nonInteractiveMode, msg, hint)
	}
}

// reportEmbedPreflightError emits the §2.5 preflight failure in the
// CLI's JSON / non-interactive / interactive styles.
func (a *App) reportEmbedPreflightError(opts upOptions, nonInteractiveMode bool, msg, hint string) int {
	if opts.jsonOutput {
		writeCLIError(a.stderr, true, exitConfigInvalid, msg, hint, "Or run: dir2mcp config init")
		return exitConfigInvalid
	}
	se := a.sty(opts.jsonOutput)
	if nonInteractiveMode {
		writef(a.stderr, "%s %s\n", se.errPrefix(), msg)
		writeln(a.stderr, hint)
		writeln(a.stderr, "Or run: dir2mcp config init")
	} else {
		writef(a.stderr, "%s %s\n", se.errPrefix(), strings.TrimPrefix(msg, "CONFIG_INVALID: "))
		writeln(a.stderr, "Run: dir2mcp config init")
	}
	return exitConfigInvalid
}

// requiresMistralAPIKey reports whether the embed-provider preflight
// applies. Embedding workers require a resolvable embed provider unless
// running read-only (serving an existing index, where the embedder is
// never exercised). (Name retained to avoid caller churn.)
func requiresMistralAPIKey(opts upOptions) bool {
	return !opts.readOnly
}

// initStoreAndIndices initialises the metadata store and both HNSW indices.
// On success the caller is responsible for closing all three.
func (a *App) initStoreAndIndices(ctx context.Context, cfg *config.Config, jsonOutput bool) (model.Store, model.Index, model.Index, int) {
	st := a.storeForConfig(*cfg)
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writeCLIError(a.stderr, jsonOutput, exitIndexLoadFailure, fmt.Sprintf("initialize metadata store: %v", err))
		return nil, nil, nil, exitIndexLoadFailure
	}

	textIndexPath := filepath.Join(cfg.StateDir, "vectors_text.hnsw")
	textIx := index.NewHNSWIndex(textIndexPath)
	if err := textIx.Load(textIndexPath); err != nil &&
		!errors.Is(err, model.ErrNotImplemented) &&
		!errors.Is(err, os.ErrNotExist) {
		writeCLIError(a.stderr, jsonOutput, exitIndexLoadFailure, fmt.Sprintf("load text index: %v", err))
		_ = st.Close()
		_ = textIx.Close()
		return nil, nil, nil, exitIndexLoadFailure
	}

	codeIndexPath := filepath.Join(cfg.StateDir, "vectors_code.hnsw")
	codeIx := index.NewHNSWIndex(codeIndexPath)
	if err := codeIx.Load(codeIndexPath); err != nil &&
		!errors.Is(err, model.ErrNotImplemented) &&
		!errors.Is(err, os.ErrNotExist) {
		writeCLIError(a.stderr, jsonOutput, exitIndexLoadFailure, fmt.Sprintf("load code index: %v", err))
		_ = st.Close()
		_ = textIx.Close()
		_ = codeIx.Close()
		return nil, nil, nil, exitIndexLoadFailure
	}

	return st, textIx, codeIx, exitSuccess
}

// buildMCPServerOptions constructs the list of mcp.ServerOption values,
// including optional ElevenLabs TTS when configured.
func buildMCPServerOptions(cfg *config.Config, st model.Store, indexingState *appstate.IndexingState, emitter *ndjsonEmitter) []mcp.ServerOption {
	opts := []mcp.ServerOption{
		mcp.WithStore(st),
		mcp.WithIndexingState(indexingState),
		mcp.WithEventEmitter(emitter.Emit),
	}
	// TTS is optional and fail-open (SPEC 8.3). Prefer ElevenLabs when
	// it is eligible (preserves the legacy "ELEVENLABS_API_KEY -> TTS"
	// behavior, incl. its voice/base-url config, even when an LLM
	// provider key is also present); otherwise fall back to auto
	// precedence among TTS-capable providers.
	r := cfg.Providers()
	prof, err := r.ResolveExplicit(provider.CapTTS, "elevenlabs", true)
	if err != nil {
		prof, err = r.Resolve(provider.CapTTS)
	}
	if err == nil {
		if tts, terr := providerfactory.TTS(prof); terr == nil {
			opts = append(opts, mcp.WithTTS(tts))
		}
	}
	return opts
}

// pickEmbedLogger returns a logger appropriate for the embed workers based on
// whether JSON output mode is active (discard) or not (stderr).
func pickEmbedLogger(stderr io.Writer, jsonOutput bool) *log.Logger {
	if jsonOutput {
		return log.New(io.Discard, "", 0)
	}
	return log.New(stderr, "", log.LstdFlags)
}

// startStdinQuitListener starts an optional goroutine that closes the returned
// channel when the user types a quit command on stdin.
func startStdinQuitListener(nonInteractiveMode, jsonOutput bool) chan struct{} {
	ch := make(chan struct{})
	if nonInteractiveMode || jsonOutput {
		return ch
	}
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(strings.ToLower(scanner.Text()))
			if line == "q" || line == "quit" || line == "exit" || line == "stop" || line == "" {
				close(ch)
				return
			}
		}
		close(ch)
	}()
	return ch
}

// startIngestWorker launches the ingest goroutine (or closes the channel
// immediately when readOnly is true).
func startIngestWorker(runCtx context.Context, readOnly bool, ing interface {
	Run(context.Context) error
}, indexingState *appstate.IndexingState, ingestErrCh chan error) {
	if readOnly {
		close(ingestErrCh)
		return
	}
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

// runEventLoop runs the main select loop until the server stops, context is
// cancelled, or a fatal error occurs.
func (a *App) runEventLoop(
	runCtx context.Context,
	cancel context.CancelFunc,
	cfg *config.Config,
	st model.Store,
	indexingState *appstate.IndexingState,
	emitter *ndjsonEmitter,
	serverErrCh <-chan error,
	ingestErrCh chan error,
	embedErrCh <-chan error,
	stdinQuitCh <-chan struct{},
) int {
	for {
		select {
		case <-runCtx.Done():
			return exitSuccess
		case <-stdinQuitCh:
			cancel()
			return exitSuccess
		case serverErr := <-serverErrCh:
			if serverErr != nil {
				writeCLIError(a.stderr, emitter.enabled, exitGeneric, fmt.Sprintf("server failed: %v", serverErr))
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
			writeCLIError(a.stderr, emitter.enabled, exitIngestionFatal, fmt.Sprintf("ingestion failed: %v", ingestErr))
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
			writeCLIError(a.stderr, emitter.enabled, exitGeneric, fmt.Sprintf("embedding worker warning: %v", embedErr))
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

// upNonInteractiveMode reports whether the server was started in a context
// where interactive prompts are not possible (flag set, or stdin/stdout are
// not terminals).
func upNonInteractiveMode(opts upOptions) bool {
	return opts.nonInteractive || !isTerminal(os.Stdin) || !isTerminal(os.Stdout)
}

// warnConfigSnapshotErr writes a warning to stderr when the config snapshot
// could not be saved and the server is not running in quiet mode.
func warnConfigSnapshotErr(stderr io.Writer, quiet bool, err error) {
	if err != nil && !quiet {
		writef(stderr, "warning: write config snapshot: %v\n", err)
	}
}

// initIndexingState creates a new IndexingState and optionally preloads
// already-embedded chunk metadata from the store.
func initIndexingState(ctx context.Context, st model.Store, ret *retrieval.Service, emitter *ndjsonEmitter, stderr io.Writer) *appstate.IndexingState {
	indexingState := appstate.NewIndexingState(appstate.ModeIncremental)
	metadataStore, ok := st.(embeddedChunkLister)
	if !ok {
		return indexingState
	}
	chunks, err := preloadEmbeddedChunkMetadata(ctx, metadataStore, ret)
	if err != nil {
		// surface the problem in both stderr and the NDJSON event stream so
		// automation can detect a bootstrap warning.
		writef(stderr, "bootstrap embedded chunk metadata: %v\n", err)
		emitter.Emit("warning", "bootstrap_embedded_chunk_metadata", map[string]interface{}{
			"message": err.Error(),
		})
	}
	if chunks > 0 {
		indexingState.AddEmbeddedOK(int64(chunks))
	}
	return indexingState
}

// wireIngestorHooks connects the optional indexing-state and document-delete
// notification interfaces on ing, if the concrete type supports them.
func wireIngestorHooks(ing model.Ingestor, indexingState *appstate.IndexingState, evict func([]string)) {
	if stateAware, ok := ing.(indexingStateAware); ok {
		stateAware.SetIndexingState(indexingState)
	}
	if notifier, ok := ing.(documentDeleteNotifier); ok {
		notifier.SetOnDocumentsDeleted(evict)
	}
}

// stopPersistenceWithLog stops the persistence manager and logs any non-cancel
// errors to stderr.
func (a *App) stopPersistenceWithLog(persistence *index.PersistenceManager) {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if stopErr := persistence.StopAndSave(stopCtx); stopErr != nil && !errors.Is(stopErr, context.Canceled) {
		writef(a.stderr, "final index save warning: %v\n", stopErr)
	}
}

// startEmbeddingIfNotReadOnly starts embedding workers when readOnly is false
// and the store exposes the ChunkSource interface.
func startEmbeddingIfNotReadOnly(ctx context.Context, readOnly bool, st model.Store, textIx, codeIx model.Index, embedder model.Embedder, ret *retrieval.Service, indexingState *appstate.IndexingState, embedErrCh chan error, stderr io.Writer, jsonOutput bool, embedModelText, embedModelCode string) {
	if readOnly {
		return
	}
	chunkSource, ok := st.(index.ChunkSource)
	if !ok {
		return
	}
	embedLogger := pickEmbedLogger(stderr, jsonOutput)
	startEmbeddingWorkers(ctx, chunkSource, textIx, codeIx, embedder, ret, indexingState, embedErrCh, embedLogger, embedModelText, embedModelCode)
}

// printHumanConnectionIfVerbose prints the human-readable connection block
// when neither --json nor --quiet is active.
func (a *App) printHumanConnectionIfVerbose(cfg config.Config, connection connectionPayload, auth authMaterial, opts upOptions) {
	if !opts.jsonOutput && !opts.quiet {
		a.printHumanConnection(cfg, connection, auth, opts.readOnly)
	}
}

// prepareUpConfig runs the full config-load + validation pipeline that
// must succeed before runUp can bind a listener: loadConfigWithGlobalOptions,
// TLS resolution, flag overrides, validateUpConfig, Mistral key check,
// auth material prep, and the effective-config snapshot write. Returns
// the resolved cfg, auth material, TLS file paths, the derived
// non-interactive flag, and an exit code; runUp returns immediately
// when the code is non-zero (the helper has already written the error
// to stderr).
//
// Extracted from runUp to keep that function under the
// cyclomatic-complexity budget after PR #174's daemon-setup branches
// landed; the exit-code-or-fall-through pattern collected enough `if`
// guards in runUp to push it over.
func (a *App) prepareUpConfig(opts upOptions) (config.Config, authMaterial, string, string, bool, int) {
	cfg, err := loadConfigWithGlobalOptions(opts.globalOptions)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return config.Config{}, authMaterial{}, "", "", false, exitConfigInvalid
	}
	tlsCertFile, tlsKeyFile, code := a.applyTLSConfig(&cfg, opts)
	if code != exitSuccess {
		return config.Config{}, authMaterial{}, "", "", false, code
	}
	x402TokenSource, code := a.applyUpFlagOverrides(&cfg, opts)
	if code != exitSuccess {
		return config.Config{}, authMaterial{}, "", "", false, code
	}
	if code := a.validateUpConfig(&cfg, opts); code != exitSuccess {
		return config.Config{}, authMaterial{}, "", "", false, code
	}
	nonInteractiveMode := upNonInteractiveMode(opts)
	if code := a.checkMistralAPIKey(&cfg, opts, nonInteractiveMode); code != exitSuccess {
		return config.Config{}, authMaterial{}, "", "", false, code
	}
	// SPEC 8.1.4: refuse to mix vector spaces if the embed
	// provider/model changed since the index was built.
	if err := cfg.VerifyEmbedIdentity(cfg.StateDir); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, err.Error(),
			"The embed provider/model changed since the index was built — run `dir2mcp reindex`, or restore the previous embed config.")
		return config.Config{}, authMaterial{}, "", "", false, exitConfigInvalid
	}
	auth, err := prepareAuthMaterial(cfg)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitAuthOrPayment, fmt.Sprintf("auth setup: %v", err))
		return config.Config{}, authMaterial{}, "", "", false, exitAuthOrPayment
	}
	cfg.AuthMode = auth.mode
	cfg.ResolvedAuthToken = auth.token
	warnConfigSnapshotErr(a.stderr, opts.quiet, saveEffectiveConfigSnapshot(cfg, auth, x402TokenSource))
	return cfg, auth, tlsCertFile, tlsKeyFile, nonInteractiveMode, exitSuccess
}

// publishConnection writes connection.json and emits the standard
// "connection / scan_progress / embed_progress" startup events. Split
// from runUp purely to keep that function under the cyclomatic budget
// after PR #174's daemon-setup branches landed.
func (a *App) publishConnection(cfg config.Config, mcpURL string, auth authMaterial, emitter *ndjsonEmitter, opts upOptions) (connectionPayload, int) {
	connection := buildConnectionPayload(cfg, mcpURL, auth)
	if err := writeConnectionFile(filepath.Join(cfg.StateDir, connectionFileName), connection); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("write %s: %v", connectionFileName, err))
		return connectionPayload{}, exitGeneric
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
	return connection, exitSuccess
}

// setupDaemonChildIfApplicable performs the two daemon-child-only
// preconditions that have to happen BEFORE writing connection.json: the
// SIGTERM handler install and the O_EXCL pid-file claim. The handler is
// installed first so a SIGTERM that arrives during the window between
// here and the event loop is converted into a graceful cancel rather
// than killing the child abruptly. The pid-file claim must come before
// connection.json so a second daemon racing for the same state dir
// loses deterministically and exits before touching anything else.
//
// Returns the deferred cleanup closure (a no-op when not running as
// daemon child) and an exit code; runUp returns immediately when the
// code is non-zero (the helper has already written the error).
//
// Extracted from runUp to keep that function under the
// cyclomatic-complexity budget after PR #174's daemon-child setup grew.
func (a *App) setupDaemonChildIfApplicable(stateDir string, opts upOptions, cancel context.CancelFunc) (cleanup func(), code int) {
	cleanup = func() {}
	if !isRunningAsDaemonChild() {
		return cleanup, exitSuccess
	}
	installDaemonChildSignalHandler(cancel)
	pidPath := pidFilePath(stateDir)
	if err := claimPIDFile(pidPath, os.Getpid()); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric,
			fmt.Sprintf("claim pid file %s: %v", pidPath, err),
			"another dir2mcp daemon is already running for this state directory; check with `dir2mcp status` or stop it with `dir2mcp down`.",
		)
		return cleanup, exitGeneric
	}
	return func() { _ = removePIDFile(pidPath) }, exitSuccess
}

// installInteractionForUp picks the right termination interaction for
// the current run mode and returns the channel runEventLoop watches.
//
//   - Daemon child: skip the foreground banner (the parent printed a
//     daemon-ready summary on the user's terminal) and translate
//     SIGTERM/SIGINT into cancel() so `dir2mcp down` can stop us cleanly
//     through the existing event-loop shutdown path. The stdin listener
//     is suppressed because the child has no terminal.
//   - Foreground: print the banner and start the q+Enter stdin listener
//     the way the pre-daemonization code did.
//
// Extracted from runUp purely to keep that function under the
// cyclomatic-complexity budget after daemon mode was added.
func (a *App) installInteractionForUp(
	cancel context.CancelFunc,
	cfg config.Config,
	connection connectionPayload,
	auth authMaterial,
	opts upOptions,
	nonInteractiveMode bool,
) <-chan struct{} {
	if isRunningAsDaemonChild() {
		installDaemonChildSignalHandler(cancel)
		return startStdinQuitListener(true, opts.jsonOutput)
	}
	a.printHumanConnectionIfVerbose(cfg, connection, auth, opts)
	return startStdinQuitListener(nonInteractiveMode, opts.jsonOutput)
}

// shouldDaemonize decides whether `dir2mcp up` should fork a detached
// daemon child instead of running the server in the foreground.
//
// Daemon mode is the default for an interactive shell — `dir2mcp up`
// returns control once the server is bound and ready. The opt-outs:
//   - --foreground / -f: explicit caller request
//   - --json: NDJSON event stream callers (smoke tests, automation) need
//     events on stdout in real time, which detaching breaks
//   - daemon child: we're already inside the forked process; fall through
//     to the in-process server body
//   - non-unix platforms: setsid isn't available, so degrade gracefully
//   - stdout not a TTY: piped, redirected, or being scraped by a test
//     harness — the caller wants to see real-time output (and any
//     startup errors) on the same stream the parent would have used.
//     Daemon mode hides the child's stderr inside the server log, which
//     is unhelpful for non-interactive callers.
//
// The TTY check is the gate that keeps existing CLI/integration tests
// (which shell out and expect synchronous up/down semantics) working
// without changes.
func shouldDaemonize(a *App, opts upOptions) bool {
	if opts.foreground || opts.jsonOutput {
		return false
	}
	if isRunningAsDaemonChild() {
		return false
	}
	if !isDaemonSupported() {
		return false
	}
	if !writerIsTerminal(a.stdout) && !opts.daemon {
		return false
	}
	return true
}

// writerIsTerminal reports whether w corresponds to a terminal file
// descriptor. Anything that isn't an *os.File (a bytes.Buffer in tests,
// a pipe in CI) is treated as non-terminal.
func writerIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(f)
}
