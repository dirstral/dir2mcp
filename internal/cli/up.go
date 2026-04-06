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

	"dir2mcp/internal/appstate"
	"dir2mcp/internal/config"
	"dir2mcp/internal/elevenlabs"
	"dir2mcp/internal/index"
	"dir2mcp/internal/mcp"
	"dir2mcp/internal/mistral"
	"dir2mcp/internal/model"
	"dir2mcp/internal/protocol"
	"dir2mcp/internal/retrieval"
)

func (a *App) runUp(ctx context.Context, opts upOptions) int {
	cfg, err := loadConfigWithGlobalOptions(opts.globalOptions)
	if err != nil {
		writef(a.stderr, "load config: %v\n", err)
		return exitConfigInvalid
	}
	tlsCertFile := strings.TrimSpace(opts.tlsCert)
	if tlsCertFile == "" {
		tlsCertFile = strings.TrimSpace(cfg.ServerTLSCertFile)
	}
	tlsKeyFile := strings.TrimSpace(opts.tlsKey)
	if tlsKeyFile == "" {
		tlsKeyFile = strings.TrimSpace(cfg.ServerTLSKeyFile)
	}
	if err := validateTLSFlags(tlsCertFile, tlsKeyFile); err != nil {
		writef(a.stderr, "CONFIG_INVALID: %v\n", err)
		return exitConfigInvalid
	}
	cfg.ServerTLSCertFile = tlsCertFile
	cfg.ServerTLSKeyFile = tlsKeyFile
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
	x402TokenSource := ""
	// precedence: file path > env var > flag
	if opts.x402FacilitatorTokenFile != "" {
		data, err := os.ReadFile(filepath.Clean(opts.x402FacilitatorTokenFile))
		if err != nil {
			writef(a.stderr, "failed to read x402 facilitator token file: %v\n", err)
			return exitAuthOrPayment
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			writef(a.stderr, "x402 facilitator token file is empty\n")
			return exitAuthOrPayment
		}
		cfg.X402.FacilitatorToken = token
		x402TokenSource = "file"
	} else if token := strings.TrimSpace(os.Getenv(x402FacilitatorTokenEnvVar)); token != "" {
		cfg.X402.FacilitatorToken = token
		x402TokenSource = "env"
	} else if strings.TrimSpace(opts.x402FacilitatorToken) != "" {
		cfg.X402.FacilitatorToken = strings.TrimSpace(opts.x402FacilitatorToken)
		x402TokenSource = "flag"
	} else if strings.TrimSpace(cfg.X402.FacilitatorToken) != "" {
		x402TokenSource = "configured"
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
		return exitAuthOrPayment
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
		return exitAuthOrPayment
	}
	cfg.AuthMode = auth.mode
	cfg.ResolvedAuthToken = auth.token
	if snapErr := saveEffectiveConfigSnapshot(cfg, auth, x402TokenSource); snapErr != nil && !opts.quiet {
		writef(a.stderr, "warning: write config snapshot: %v\n", snapErr)
	}

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
	ret.SetRAGSystemPrompt(cfg.RAGSystemPrompt)
	ret.SetMaxContextChars(cfg.RAGMaxContextChars)
	ret.SetOversampleFactor(cfg.RAGOversampleFactor)

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
	ing, err := a.newIngestor(cfg, st)
	if err != nil {
		writef(a.stderr, "initialize ingestor: %v\n", err)
		return exitConfigInvalid
	}
	if stateAware, ok := ing.(indexingStateAware); ok {
		stateAware.SetIndexingState(indexingState)
	}
	if notifier, ok := ing.(documentDeleteNotifier); ok {
		notifier.SetOnDocumentsDeleted(ret.EvictDocuments)
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
	mcpURL := buildMCPURL(mcpAddr, cfg.MCPPath, tlsCertFile != "")

	mcpTransportMode := strings.TrimSpace(os.Getenv("MCP_TRANSPORT"))
	transport, err := mcp.NewTransport(mcpTransportMode, mcpServer, ln, tlsCertFile, tlsKeyFile)
	if err != nil {
		writef(a.stderr, "transport init: %v\n", err)
		return exitConfigInvalid
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- transport.Serve(runCtx, mcpServer.Handler())
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

	if !opts.jsonOutput && !opts.quiet {
		a.printHumanConnection(cfg, connection, auth, opts.readOnly)
	}

	// In interactive mode, listen for a quit command on stdin so the user can
	// stop the server without reaching for Ctrl+C.
	stdinQuitCh := make(chan struct{})
	if !nonInteractiveMode && !opts.jsonOutput {
		go func() {
			scanner := bufio.NewScanner(os.Stdin)
			for scanner.Scan() {
				line := strings.TrimSpace(strings.ToLower(scanner.Text()))
				if line == "q" || line == "quit" || line == "exit" || line == "stop" || line == "" {
					close(stdinQuitCh)
					return
				}
			}
			close(stdinQuitCh)
		}()
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
		case <-stdinQuitCh:
			cancel()
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
