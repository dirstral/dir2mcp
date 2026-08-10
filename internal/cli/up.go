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
	"sync"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/buildinfo"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/identity"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
	"github.com/dirstral/dir2mcp/internal/retrieval"
	"github.com/dirstral/dir2mcp/internal/setupwizard"
	"github.com/dirstral/dir2mcp/internal/statefs"
	"github.com/dirstral/dir2mcp/internal/store"
)

func (a *App) runUp(ctx context.Context, opts upOptions) int {
	// First-run guided setup (SPEC §147) must run here, in the launching parent
	// while still attached to the TTY — before any daemon fork, whose child has
	// no terminal and would silently skip the wizard. maybeFirstRunSetup is a
	// no-op on non-TTY / --json / --non-interactive runs, in the daemon child,
	// and when an embedding provider already resolves.
	if code := a.maybeFirstRunSetup(opts); code != exitSuccess {
		return code
	}

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

	// Mirror the process logger to <state_dir>/server.log for foreground/service
	// launches too (the daemon child already redirects stderr there). This keeps
	// recovered-panic stacks and log output reachable in the support bundle
	// regardless of how the server was started (issue #360). Bounded to this
	// call so it never leaks into other code paths/tests.
	if restoreLog := a.teeServerLog(cfg.StateDir); restoreLog != nil {
		defer restoreLog()
	}

	st, textBuilt, codeBuilt, code := a.openStateForServing(ctx, &cfg, opts.jsonOutput)
	if code != exitSuccess {
		return code
	}
	defer func() { _ = st.Close() }()
	textIx, textIndexPath := textBuilt.index, textBuilt.path
	codeIx, codeIndexPath := codeBuilt.index, codeBuilt.path
	// One mutex-guarded sink shared by every goroutine that logs to stderr during
	// the concurrent phase (embed workers, corpus writer, watch worker, the
	// persistence autosave callback, and the event loop). Without it, direct
	// writef(stderr, ...) writes race the embed worker's *log.Logger writes on the
	// same underlying sink (a bytes.Buffer under `go test -race`) — issue #419.
	//
	// It is created here, before the first shutdown defer, because the shutdown
	// writers need it too: when the drain gives up, a background goroutine can
	// still be writing to this sink while the teardown reports the forced end.
	logSink := NewSyncWriter(a.stderr)

	// Step 4 of the shutdown order in up_shutdown.go. The fence keeps the
	// indexes open when a periodic save still owns them (issue #689); the
	// persistence stop below raises it.
	indexFence := &indexCloseFence{}
	defer func() { indexFence.closeIndexesAfterPersistence(logSink, codeIx, textIx) }()

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
	ret.SetCrossFileDedupEnabled(cfg.DedupRetrieval)
	ret.SetMinScore(cfg.RetrievalMinScore)
	ret.SetRecencyHalfLife(cfg.RetrievalRecencyHalfLife)
	ret.SetContextCompression(cfg.ContextCompressionEnabled, cfg.ContextCompressionTargetRatio)
	ret.SetAdaptiveRetrieval(cfg.RetrievalAdaptiveEnabled, cfg.RetrievalAdaptiveKMin, cfg.RetrievalAdaptiveKMax)
	ret.SetMMR(cfg.RetrievalMMREnabled, cfg.RetrievalMMRLambda)
	ret.SetHyDE(cfg.RetrievalHyDEEnabled, cfg.RetrievalHyDEMode)
	// Hierarchical (coarse-to-fine) retrieval (SPEC §9.7): gates only the expand
	// step; summary hits are never citable regardless of this flag.
	ret.SetHierarchical(cfg.RetrievalHierarchicalEnabled)
	a.configureCrossLingual(ret, cfg, st, generator)

	// events are emitted to stdout only after we create the emitter; moving
	// creation before the preload call lets us report failures from that
	// bootstrap step as structured events (see dirstral-spec/docs/SPEC.md for NDJSON schema).
	emitter := newNDJSONEmitter(a.stdout, opts.jsonOutput)

	// Wire per-query cost/latency observability (issue #327): always-on,
	// additive, never alters tool results. Emits one query_metrics event per
	// ask/search via the structured emitter plus a concise log line.
	a.configureQueryMetrics(ret, cfg, emitter.Emit)

	// Load the cross-file dedup hash map AFTER the emitter exists so a load
	// failure is reported as a structured NDJSON warning event (machine-parseable
	// in JSON/automation flows), consistent with other bootstrap steps.
	a.loadCrossFileDedupHashes(ctx, cfg, st, ret, emitter)

	indexingState := initIndexingState(ctx, st, ret, emitter, a.stderr)
	ret.SetIndexingCompleteProvider(func() bool {
		return !indexingState.Snapshot().Running
	})

	corpusFS, err := buildCorpusFS(ctx, cfg)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("initialize corpus source: %v", err))
		return exitConfigInvalid
	}

	serverOptions := buildMCPServerOptions(&cfg, st, indexingState, emitter)
	// Route the on-demand tool paths (open_media_clip, on-demand audio init,
	// on-demand content reads) through the corpus filesystem so they work on an
	// object-store corpus, which has no file at RootDir/rel_path (#759). The
	// server ignores it for a local/NFS corpus, which keeps its filesystem-native
	// resolution.
	serverOptions = append(serverOptions, mcp.WithCorpusFS(corpusFS))
	mcpServer := mcp.NewServer(cfg, ret, serverOptions...)
	ing, err := a.newIngestor(cfg, st)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("initialize ingestor: %v", err))
		return exitConfigInvalid
	}
	wireIngestorHooks(ing, ingestorHooks{
		indexingState: indexingState,
		evict:         ret.EvictDocuments,
		onDocError:    newFileErrorEmitter(emitter),
		onDocSkip:     newFileSkipEmitter(emitter),
	})
	wireDerivationCacheIdentities(ret, ing)

	setCorpusFSIfSupported(ing, corpusFS)
	// Route retrieval-time reads (open_file raw text / OCR / transcript) through
	// the corpus filesystem for object-store backends so open_file works on an S3
	// corpus (#432). Local/NFS corpora keep the historical local read path (their
	// resolved-path + symlink-containment behavior) untouched.
	if sourceIsRemote(cfg) {
		ret.SetCorpusFS(corpusFS)
	}

	emitter.Emit("info", "index_loaded", map[string]interface{}{
		"state_dir": cfg.StateDir,
	})

	ln, code := a.bindServerListener(cfg, opts.jsonOutput)
	if code != exitSuccess {
		return code
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = ln.Close() }()

	cleanupPIDFile, code := a.setupServerSingleInstance(cfg.StateDir, opts, cancel)
	if code != exitSuccess {
		return code
	}
	defer cleanupPIDFile()

	if code := a.startManagedRecognizeBackend(runCtx, ing, opts.jsonOutput); code != exitSuccess {
		return code
	}

	persistence := index.NewPersistenceManager(
		[]index.IndexedFile{
			{Path: textIndexPath, Index: textIx},
			{Path: codeIndexPath, Index: codeIx},
		},
		15*time.Second,
		func(saveErr error) { writef(logSink, "index autosave warning: %v\n", saveErr) },
	)
	persistence.Start(runCtx)
	// Step 3 of the shutdown order in up_shutdown.go.
	defer func() { stopPersistenceWithLog(persistence, indexFence, logSink) }()

	// Steps 1 and 2 of the shutdown order in up_shutdown.go. The drain group
	// holds every long-lived background goroutine: the MCP transport, the
	// initial ingest, the embed workers (or, in distributed mode, the
	// coordinator/worker/broker-closer), the corpus writer, and the watch
	// worker. Waiting for them stops their logging from outliving the call and
	// racing a caller that reads the sink after return (issue #419), and stops
	// them from touching the store or the indexes that the deferred Close calls
	// below tear down (issue #688).
	var bgWG sync.WaitGroup
	defer func() { drainBackgroundWorkers(cancel, &bgWG, logSink) }()
	// Startup reconciliation (#402 A2): re-pend chunks sqlite counts embedded but
	// whose vectors were lost to an ungraceful crash before the in-memory index's
	// periodic snapshot. Must run before the embed worker so the re-pended chunks
	// are picked up this session. No-op for durable backends and in read-only mode.
	// Writes go through the synchronized logSink, not raw a.stderr: persistence
	// autosave (started above) and the embed workers already write logSink
	// concurrently, so a raw-stderr write here would race that shared sink (#419).
	reconcileEmbeddedVectorsAtStartup(runCtx, opts.readOnly, st, textIx, codeIx, emitter, logSink)

	embedErrCh := make(chan error, 4)
	if err := startEmbeddingIfNotReadOnly(runCtx, cfg, opts.readOnly, st, textIx, codeIx, embedder, ret, indexingState, embedErrCh, logSink, opts.jsonOutput, etm, ecm, cfg.RootDir, corpusFS, emitter, &bgWG); err != nil {
		// A distributed-embedding setup failure is fatal: refuse to run a server
		// that would silently never drain its pending queue.
		writeCLIError(logSink, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("start embedding: %v", err))
		return exitConfigInvalid
	}

	mcpAddr := ln.Addr().String()
	if cfg.Public {
		mcpAddr = publicURLAddress(cfg.ListenAddr, mcpAddr)
	}
	mcpURL := buildMCPURL(mcpAddr, cfg.MCPPath, tlsCertFile != "")

	transport := mcp.NewSDKTransport(mcpServer, ln, tlsCertFile, tlsKeyFile)

	serverErrCh := startTransportWorker(runCtx, transport, mcpServer.Handler(), &bgWG)

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
	// Track the corpus writer on bgWG so the deferred drain waits for it to stop
	// before st.Close() runs; otherwise writeCorpusSnapshot can query a closed
	// store, and its logging can race a caller reading the sink after return (#419).
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		runCorpusWriter(runCtx, cfg.StateDir, st, indexingState, logSink, emitter)
	}()
	startIngestWorker(runCtx, opts.readOnly, ing, indexingState, ingestErrCh, &bgWG)
	startWatchWorker(runCtx, opts.readOnly, cfg.IngestWatch, cfg.Source.Kind, ing, logSink, &bgWG)

	// The server is now serving. A signal-triggered shutdown from here on is
	// a normal, successful termination (a supervisor/`down`-requested stop),
	// not an interrupted command — so it must exit 0, not exitSignalInterrupt.
	// See resolveProcessExitCode (#434).
	a.serverGracefulStop = true

	return a.runEventLoop(runCtx, cancel, &cfg, st, indexingState, emitter, serverErrCh, ingestErrCh, embedErrCh, stdinQuitCh, logSink)
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
		writeCLIErrorWithCode(a.stderr, opts.jsonOutput, exitConfigInvalid, protocol.ErrorCodeTLSConfigInvalid, fmt.Sprintf("TLS_CONFIG_INVALID: %v", err))
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
	if t := strings.TrimSpace(cfg.ServerName); t != "" {
		// An explicit name stays authoritative on every backend, and is
		// resolved before the source branch so the two paths cannot disagree
		// about what an override means.
		cfg.ServerName = t
		return
	}
	// A remote corpus is not identified by its local root. `S3FS.Walk` ignores
	// that root, and an S3 deployment commonly leaves `root_dir` at its
	// default, so deriving from it gave two different buckets launched from one
	// directory the same server name, service label and client alias (#737).
	if sourceIsRemote(*cfg) {
		cfg.ServerName = identity.AutoServerNameForS3(
			cfg.Source.S3Bucket, cfg.Source.S3Prefix, cfg.Source.S3Endpoint, buildinfo.IsDev(),
		)
		return
	}
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
	// Re-validate AFTER the CLI flag overlay (#405). Load() validated the
	// on-disk config, but applyUpFlagOverrides may have set values Load never
	// saw, so without this a flag could smuggle an invalid value past validation.
	if err := cfg.Validate(); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("CONFIG_INVALID: %v", err))
		return exitConfigInvalid
	}
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
	if strings.EqualFold(strings.TrimSpace(cfg.IngestExtractor), "docling-serve") {
		if d := ingest.DescribeDocumentExtractor(*cfg); d.Name == "" {
			writeCLIError(a.stderr, opts.jsonOutput, exitConfigInvalid, fmt.Sprintf("CONFIG_INVALID: %s", d.Reason))
			return exitConfigInvalid
		}
	}
	return a.prepareUpDirectories(cfg, opts)
}

// prepareUpDirectories enforces the corpus-root requirement and creates the
// local state hierarchy `up` needs. Split out of validateUpConfig to keep that
// function under the cyclomatic-complexity budget.
//
// It runs AFTER x402 validation so the payments subdirectory is only created
// for a config that already validated (including mode="off"), and never leaves
// an inconsistent state behind.
func (a *App) prepareUpDirectories(cfg *config.Config, opts upOptions) int {
	// The corpus root is a LOCAL directory only for the filesystem-backed
	// schemes. For an object-store source the bucket + prefix IS the corpus root
	// (SPEC §7.8) and S3FS.Walk ignores its root argument outright, so demanding
	// a local directory here fails a healthy remote-only deployment before S3 is
	// ever contacted (#738). Only the state directory must stay local, and it is
	// created and hardened immediately below. local/nfs keep the requirement:
	// an NFS mount is an ordinary local path.
	if !sourceIsRemote(*cfg) {
		if err := ensureRootAccessible(cfg.RootDir); err != nil {
			writeCLIError(a.stderr, opts.jsonOutput, exitRootInaccessible, fmt.Sprintf("root inaccessible: %v", err))
			return exitRootInaccessible
		}
	}
	if err := statefs.MkdirAllHardened(cfg.StateDir); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitRootInaccessible, fmt.Sprintf("create state dir: %v", err))
		return exitRootInaccessible
	}
	// Repair a tree an older build left world-readable. Creating the root
	// owner-only does nothing for the caches, snapshots and index segments
	// already under it, and those are the corpus in another shape. Failing
	// here rather than warning: continuing would leave that content readable
	// by other local accounts while reporting a private state directory.
	if err := statefs.HardenTree(cfg.StateDir); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitRootInaccessible, fmt.Sprintf("secure state dir: %v", err))
		return exitRootInaccessible
	}
	// The payments subdirectory is created unconditionally so the layout is the
	// same regardless of x402 mode, avoiding inconsistent state.
	if err := statefs.MkdirAllHardened(filepath.Join(cfg.StateDir, "payments")); err != nil {
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
	hint := "Set a provider credential (e.g. MISTRAL_API_KEY / OPENAI_API_KEY) or configure providers: in .dir2mcp.yaml"
	prof, embErr := cfg.Providers().Resolve(provider.CapEmbed)
	if embErr != nil {
		msg := "CONFIG_INVALID: no embedding provider configured"
		var ce *provider.ConfigError
		if errors.As(embErr, &ce) {
			// Surface the actionable reason (bad explicit binding /
			// incapable kind) instead of the generic message
			// (ce.Error() is already CONFIG_INVALID-prefixed).
			msg = ce.Error()
		}
		return a.reportEmbedPreflightError(opts, nonInteractiveMode, msg, hint)
	}
	// Resolution alone is not enough: the runtime also builds the
	// adapter (resolveModelClients -> providerfactory.Embedder). Fail
	// fast here rather than letting a non-read-only server ingest with
	// a nil embedder and silently produce no embeddings.
	emb, ferr := providerfactory.Embedder(prof)
	if ferr != nil {
		return a.reportEmbedPreflightError(opts, nonInteractiveMode,
			fmt.Sprintf("CONFIG_INVALID: embedding provider %q is unusable: %v", prof.Name, ferr), hint)
	}
	// A resolvable + buildable adapter is still not proof the credential works:
	// a typo'd/expired key (or a wrong embed model, #396) passes every check
	// above and then 401/403/404s on the first real embed, failing the WHOLE
	// corpus mid-run (issue #399 item 3). Probe once here so that surfaces as a
	// clear CONFIG_INVALID at startup instead. The probe is fail-open on
	// transient/network errors (server-first: a flaky link must not block a
	// correctly-configured server, and a momentarily-down local endpoint is a
	// connection error, hence transient). Operators who cannot afford a startup
	// network round-trip (air-gapped bring-up, deterministic CI) opt out with
	// DIR2MCP_SKIP_EMBED_PROBE — set to any non-empty value, mirroring
	// DIR2MCP_DISABLE_KEYCHAIN.
	if strings.TrimSpace(os.Getenv(skipEmbedProbeEnvVar)) == "" {
		if perr := a.probeEmbedProvider(emb, prof); perr != nil {
			return a.reportEmbedPreflightError(opts, nonInteractiveMode,
				fmt.Sprintf("CONFIG_INVALID: embedding provider %q failed a preflight probe: %v", prof.Name, perr), hint)
		}
	}
	return exitSuccess
}

// skipEmbedProbeEnvVar, when set to any non-empty value, disables the §2.5
// startup credential probe (issue #399 item 3). The provider-resolution and
// adapter-build preflight checks still run; only the one-shot network embed is
// skipped, so an invalid credential resurfaces at first real embed rather than
// at startup. Intended for air-gapped bring-up and hermetic tests.
const skipEmbedProbeEnvVar = "DIR2MCP_SKIP_EMBED_PROBE"

// probeEmbedProvider issues one throwaway embed to confirm the resolved
// provider's credential and model actually work, so a present-but-invalid key
// (or wrong embed model) fails preflight loudly instead of silently failing the
// whole corpus mid-run (issue #399 item 3, #396). It returns nil (fail-open) for
// a transient/network error — including a context deadline and a
// connection-refused from a not-yet-up local endpoint — so only a definitive
// credential/config rejection blocks startup. Uses a bounded background context
// because the preflight call chain is not context-threaded.
func (a *App) probeEmbedProvider(emb model.Embedder, prof provider.Profile) error {
	if emb == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := emb.Embed(ctx, prof.EmbedTextModel, model.EmbedDocument, []string{"dir2mcp preflight probe"}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || store.IsTransientError(err) {
			// Fail-open: a transient failure is not a credential/config problem.
			return nil
		}
		return err
	}
	return nil
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

// builtIndex pairs a constructed vector index with the on-disk path it persists
// to, so runUp can wire the PersistenceManager with the backend-appropriate
// path (HNSW v2 snapshot vs. disk segment).
type builtIndex struct {
	index model.Index
	path  string
}

// indexLoadFailure describes a failed vector-index load: the exit code to
// return plus the operator-facing message and hints to print.
//
// The index loads run concurrently (issue #429 F6), so they cannot write to
// a.stderr themselves: two goroutines racing on the same writer would interleave
// their output and, worse, whichever lost the race would decide the exit code.
// Each load therefore returns its failure as data and the (single-threaded)
// caller reports exactly one of them, chosen by a fixed kind order.
type indexLoadFailure struct {
	exitCode int
	message  string
	hints    []string
}

// openStateForServing brings the corpus's on-disk state back to a known-good
// generation and only then opens it for serving. It is `up`'s half of issue
// #727: PR #761 taught `reindex` to finish the rollback a crashed run owed, so
// an operator whose next move was another rebuild recovered. An operator whose
// next move was simply restarting the daemon did not: they got the crashed run's
// PARTIAL index served, with the complete one sitting untouched beside it in the
// backup slot and nothing said about it (issue #764). That is the quieter
// failure of the two: nothing errors, retrieval just returns less than it should.
//
// Policy: recover, exactly as `reindex` does, by calling the same two functions
// rather than growing a second recovery path that can drift from the first.
//
//   - Serving the partial generation is the bug, so "warn and carry on" is not a
//     fix; the operator would still be answering questions from a corpus that is
//     missing documents.
//   - Refusing to start is the safest-sounding option and the worst one here.
//     `up` is what service managers restart unattended, so a refusal turns every
//     crashed reindex into a corpus-wide outage that lasts until a human
//     notices; and the only way out of it is `reindex`, which performs this very
//     recovery and then rebuilds everything from scratch (hours of OCR and paid
//     embedding calls) to arrive at the generation that was already on disk.
//   - Recovering is a mutation the operator did not ask for, but it is not a new
//     kind of one: startup already hardens the state tree, re-pends chunks whose
//     vectors were lost to a crash (#402 A2), and rewrites the index snapshot on
//     its autosave timer. Restoring a rename that a killed process never got to
//     make is the same class of self-repair, and it lands the corpus in the
//     state the operator last saw rather than in a new one.
//
// The cost is the same narrow window #761 accepted: a crash between the rebuild
// becoming durable and commit deleting the backups makes us restore an older
// complete generation over a newer complete one. Both are valid indices, the
// restored hashes make the next ingest pass re-index anything that changed since,
// and an older complete index is strictly better to serve than a partial one.
//
// Failure to recover IS fatal, because the only alternative left at that point is
// to serve the partial generation. Two servers starting in the same instant, both
// before either has claimed the pid file, are lock-free here (the ownership check
// below sees no owner for either), but the failure mode is benign: the rename is
// atomic, so the loser finds the backup already gone, reports that it could not
// restore it, and exits without serving, which is the start the single-instance
// lock was about to refuse anyway.
//
// A healthy corpus pays one os.ReadDir and one "does the snapshot table exist"
// query, and nothing is written.
//
// --read-only does not exempt a corpus from this. That flag governs what the
// server does to the corpus once it is open (no ingest, no embedding), not
// whether the state directory may be repaired before it can be opened at all;
// `up` already requires a writable state directory there (it creates and hardens
// the tree). A read-only server is in fact the case that most needs the repair,
// since it never re-indexes its way out of a partial generation.
func (a *App) openStateForServing(ctx context.Context, cfg *config.Config, jsonOutput bool) (model.Store, builtIndex, builtIndex, int) {
	// Never repair a corpus another daemon already owns: renaming an index file
	// out from under its open handles is worse than anything repaired here. `up`
	// claims the single-instance lock much further down (it needs the bound
	// listener first), so the pid file is the only ownership signal available this
	// early, and it is the one `reindex` guards itself with.
	//
	// A live owner is refused, not merely skipped. Skipping the repair and
	// carrying on would reintroduce this issue through the back door: if the owner
	// exits between this check and the lock claim, that claim SUCCEEDS (it
	// reconciles a dead owner) and we would go on to serve a corpus we
	// deliberately did not repair. Refusing costs nothing, because a live owner
	// makes acquireSingleInstanceLock refuse this start anyway; it just says so
	// before opening sqlite and rehydrating two snapshots, using that same
	// contract's wording. A start refused this way recovers on its next attempt,
	// once the pid file no longer names a live process.
	//
	// The daemon child is unaffected: its parent clears the pid file before
	// forking and writes it only once the child reports ready.
	if pid, ownership := classifyPIDFile(pidFilePath(cfg.StateDir)); ownership == pidLive {
		writeCLIError(a.stderr, jsonOutput, exitGeneric,
			fmt.Sprintf("%v for %s (pid %d)", errAlreadyRunning, cfg.StateDir, pid),
			"Another dir2mcp server is already running for this state directory; check with `dir2mcp status` or stop it with `dir2mcp down`.",
		)
		return nil, builtIndex{}, builtIndex{}, exitGeneric
	}
	// Must run before the indices are loaded below: once a partial snapshot is
	// rehydrated, the autosave timer will write it back over the live slot.
	// prepareUpDirectories has already run, so the state tree (backups included)
	// is hardened and a restored file keeps its owner-only mode across the rename.
	recovered, err := recoverInterruptedReindex(cfg.StateDir)
	if err != nil {
		writeCLIError(a.stderr, jsonOutput, exitIndexLoadFailure, fmt.Sprintf("recover interrupted reindex: %v", err),
			"The last-known-good index generation is still on disk as *"+reindexBackupSuffix+" in the state directory; fix the error above, then start the server again or rebuild with `dir2mcp reindex`.")
		return nil, builtIndex{}, builtIndex{}, exitIndexLoadFailure
	}
	if len(recovered) > 0 {
		// Loud on purpose, and durable: teeServerLog is already installed, so this
		// reaches <state_dir>/server.log and the support bundle, which is where an
		// operator looks after the fact to explain a rebuild that never finished.
		writef(a.stderr, "warning: an interrupted reindex left this corpus inconsistent; restored %d last-known-good index file(s) before serving: %s\n",
			len(recovered), strings.Join(recovered, ", "))
	}
	st, textBuilt, codeBuilt, code := a.initStoreAndIndices(ctx, cfg, jsonOutput)
	if code != exitSuccess {
		return nil, builtIndex{}, builtIndex{}, code
	}
	// Detached from ctx for the same reason the reindex rollback is: a Ctrl-C
	// during startup must not abandon the rollback half-done, leaving a restored
	// index behind a cleared gate.
	if err := restoreInterruptedReindexHashes(context.WithoutCancel(ctx), st); err != nil {
		closeBuiltIndex(textBuilt)
		closeBuiltIndex(codeBuilt)
		_ = st.Close()
		writeCLIError(a.stderr, jsonOutput, exitIndexLoadFailure,
			fmt.Sprintf("recover interrupted reindex: restore content hashes: %v", err),
			"An interrupted reindex left a content-hash snapshot this server could not roll back; rebuild with `dir2mcp reindex`.")
		return nil, builtIndex{}, builtIndex{}, exitIndexLoadFailure
	}
	return st, textBuilt, codeBuilt, exitSuccess
}

// closeBuiltIndex closes a loaded index, tolerating the nil index a single-axis
// configuration leaves behind (closing a nil model.Index would panic).
func closeBuiltIndex(b builtIndex) {
	if b.index != nil {
		_ = b.index.Close()
	}
}

// initStoreAndIndices initialises the metadata store and both vector indices,
// dispatching on cfg.IndexBackend ("memory" default | "disk" | "qdrant" |
// "pgvector"; issues #246, #268, #269). On success the caller is responsible for
// closing all three.
//
// `up` reaches this through openStateForServing, which repairs an interrupted
// reindex first. The distributed embed worker calls it directly, deliberately: a
// worker starts BESIDE a live daemon that already has these files open, so it
// must never rename one out from under it. The daemon owns that repair.
func (a *App) initStoreAndIndices(ctx context.Context, cfg *config.Config, jsonOutput bool) (model.Store, builtIndex, builtIndex, int) {
	st := a.storeForConfig(*cfg)
	if err := st.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		writeStoreInitError(a.stderr, jsonOutput, exitIndexLoadFailure, err, fmt.Sprintf("initialize metadata store: %v", err))
		return nil, builtIndex{}, builtIndex{}, exitIndexLoadFailure
	}

	// The persisted snapshot format is versioned (issue #247): for the memory
	// backend the v2 file (vectors_*.v2.hnsw) carries vectors + per-vector
	// payloads + the embed identity, where the legacy file held a bare vector
	// map; for the disk backend the segment (vectors_*.diskv1.idx) memory-maps
	// vector payloads on disk (issue #246). A missing snapshot is treated as a
	// fresh index and repopulated on the next reindex.
	identity := cfg.Providers().EmbedIdentity()
	built, fail := a.loadVectorIndices(ctx, cfg, identity)
	if fail != nil {
		writeCLIError(a.stderr, jsonOutput, fail.exitCode, fail.message, fail.hints...)
		_ = st.Close()
		return nil, builtIndex{}, builtIndex{}, fail.exitCode
	}
	return st, built[0], built[1], exitSuccess
}

// vectorIndexKinds is the fixed order of the vector index kinds: it drives the
// concurrent load below, which kind's error is reported when both fail, and the
// order of the embedded-chunk metadata warm-load.
var vectorIndexKinds = [2]string{index.KindText, index.KindCode}

// loadVectorIndices loads the text and code indices concurrently (issue #429
// F6). Serially, cold start paid the two rehydrations back to back: for the
// memory backend each one is a full gob decode of that kind's snapshot, so a
// large corpus spent tens of seconds before anything could be served. The two
// loads touch disjoint state (separate index instances, separate snapshot
// paths, separate identity reconciliation), so they are genuinely independent.
//
// On failure the surviving index is closed and exactly one failure is reported,
// picked in vectorIndexKinds order rather than by whichever goroutine finished
// first, so the message and exit code stay deterministic and attributable. A
// failing load does NOT cancel its sibling: the sibling may be mid-Reset against
// a networked backend, and interrupting that is a worse outcome than waiting out
// a load that is about to abort the process anyway.
func (a *App) loadVectorIndices(ctx context.Context, cfg *config.Config, identity string) ([2]builtIndex, *indexLoadFailure) {
	var (
		built [2]builtIndex
		fails [2]*indexLoadFailure
		wg    sync.WaitGroup
	)
	for i, kind := range vectorIndexKinds {
		wg.Add(1)
		go func(slot int, kind string) {
			defer wg.Done()
			built[slot], fails[slot] = a.loadVectorIndex(ctx, cfg, kind, identity)
		}(i, kind)
	}
	wg.Wait()

	for i, fail := range fails {
		if fail == nil {
			continue
		}
		for j := range built {
			if j != i && built[j].index != nil {
				_ = built[j].index.Close()
			}
		}
		return [2]builtIndex{}, fail
	}
	return built, nil
}

// loadVectorIndex constructs the configured vector backend for one kind via the
// index.backend dispatch (issues #246, #268, #269), restores it from its
// persisted snapshot, and reconciles its recorded embed identity with the
// configured one (issue #247): EnsureIdentity resets the index when the identity
// is empty (fresh) or differs, so a vector space built under a different embed
// provider/model/dimension is never silently reused.
//
// It runs on its own goroutine (see loadVectorIndices), so it reports problems
// by returning an *indexLoadFailure instead of writing to a.stderr.
func (a *App) loadVectorIndex(ctx context.Context, cfg *config.Config, kind, identity string) (builtIndex, *indexLoadFailure) {
	// Networked backends (Qdrant #268, pgvector #269) carry connection config
	// NewBackend can't reach and have no local persistence path, so they branch
	// out before the local memory/disk dispatch. EnsureIdentity still runs (both
	// implement Identity/Reset); the Persistable Load step is skipped because the
	// remote store owns its own durability.
	if a.newIndex == nil {
		if strings.EqualFold(strings.TrimSpace(cfg.IndexBackend), index.BackendQdrant) {
			return a.loadQdrantIndex(ctx, cfg, kind, identity)
		}
		if strings.EqualFold(strings.TrimSpace(cfg.IndexBackend), index.BackendPgvector) {
			return a.loadPgvectorIndex(ctx, cfg, kind, identity)
		}
	}
	ix, path := a.newBackendIndex(*cfg, kind)
	if p, ok := ix.(model.Persistable); ok {
		if err := p.Load(ctx, path); err != nil &&
			!errors.Is(err, model.ErrNotImplemented) &&
			!errors.Is(err, os.ErrNotExist) {
			_ = ix.Close()
			return builtIndex{}, &indexLoadFailure{
				exitCode: exitIndexLoadFailure,
				message:  fmt.Sprintf("load %s index: %v", kind, err),
			}
		}
	}
	if err := index.EnsureIdentity(ctx, ix, identity); err != nil {
		_ = ix.Close()
		return builtIndex{}, identityFailure(kind, err)
	}
	return builtIndex{index: ix, path: path}, nil
}

// newBackendIndex resolves the local index.backend dispatch, honouring the
// newIndex hook when tests inject one.
func (a *App) newBackendIndex(cfg config.Config, kind string) (model.Index, string) {
	if a.newIndex != nil {
		return a.newIndex(cfg, kind)
	}
	return index.NewBackend(cfg.IndexBackend, cfg.StateDir, kind)
}

// identityFailure describes a failed embed-identity reconciliation for one kind.
func identityFailure(kind string, err error) *indexLoadFailure {
	return &indexLoadFailure{
		exitCode: exitIndexLoadFailure,
		message:  fmt.Sprintf("reconcile %s index identity: %v", kind, err),
	}
}

// loadQdrantIndex constructs the networked Qdrant backend for one kind (issue
// #268): it dials Qdrant (verifying reachability up front — no silent fallback)
// against a per-kind collection, then reconciles the recorded embed identity.
// There is no local persistence path, so builtIndex.path is left empty and the
// PersistenceManager is never wired for this kind.
func (a *App) loadQdrantIndex(ctx context.Context, cfg *config.Config, kind, identity string) (builtIndex, *indexLoadFailure) {
	ix, err := index.NewQdrantBackend(ctx, index.QdrantParams{
		URL:        cfg.Qdrant.URL,
		APIKey:     cfg.Qdrant.APIKey,
		Collection: cfg.Qdrant.Collection,
	}, kind)
	if err != nil {
		return builtIndex{}, &indexLoadFailure{
			exitCode: exitIndexLoadFailure,
			message:  fmt.Sprintf("connect %s index to qdrant: %v", kind, err),
		}
	}
	if err := index.EnsureIdentity(ctx, ix, identity); err != nil {
		_ = ix.Close()
		return builtIndex{}, identityFailure(kind, err)
	}
	return builtIndex{index: ix, path: ""}, nil
}

// loadPgvectorIndex constructs the networked PostgreSQL + pgvector backend for
// one kind (issue #269): it connects (verifying reachability and the pgvector
// extension up front — no silent fallback) against a per-kind table, then
// reconciles the recorded embed identity. A missing DSN, an unreachable server,
// or a missing extension is reported as a remediable error and aborts startup.
// There is no local persistence path, so builtIndex.path is left empty and the
// PersistenceManager is never wired for this kind.
func (a *App) loadPgvectorIndex(ctx context.Context, cfg *config.Config, kind, identity string) (builtIndex, *indexLoadFailure) {
	dsn := strings.TrimSpace(cfg.IndexPgvectorDSN)
	if dsn == "" {
		return builtIndex{}, &indexLoadFailure{
			exitCode: exitConfigInvalid,
			message:  "CONFIG_INVALID: index.backend=pgvector but no DSN configured",
			hints: []string{
				"Set DIR2MCP_INDEX_PGVECTOR_DSN (or store it in the keychain / .env.local) to a Postgres connection string.",
			},
		}
	}
	ix, err := index.NewPgvectorBackend(ctx, index.PgvectorParams{
		DSN:    dsn,
		Schema: cfg.IndexPgvectorSchema,
		Table:  cfg.IndexPgvectorTable,
	}, kind)
	if err != nil {
		return builtIndex{}, &indexLoadFailure{
			exitCode: exitIndexLoadFailure,
			message:  fmt.Sprintf("connect %s index to pgvector: %v", kind, err),
		}
	}
	if err := index.EnsureIdentity(ctx, ix, identity); err != nil {
		_ = ix.Close()
		return builtIndex{}, identityFailure(kind, err)
	}
	return builtIndex{index: ix, path: ""}, nil
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

// pickEmbedLogger returns a logger for the embed workers.
//
// In human mode it writes to stderr (the operator's terminal). In JSON mode it
// MUST NOT discard: the daemonized server child runs in JSON mode, and silently
// dropping the worker's "embed worker started [kind=...]" startup line (issue
// #364 added it precisely so its ABSENCE in server.log is the diagnosis) and the
// per-batch "embedded N chunk(s)" progress would make those diagnostics
// unreachable in a real daemon. Instead it writes to the process-global log
// destination (log.Writer()), which is already wired to reach
// <state_dir>/server.log in EVERY launch mode:
//
//   - daemon child: the parent redirects the child's stderr to server.log, and
//     log.Writer() defaults to stderr (issue #360);
//   - foreground/service: teeServerLog() has already swapped log.Writer() for a
//     MultiWriter that includes the server.log file handle.
//
// Routing through that single destination (rather than opening server.log a
// second time here) is what avoids double-writing the worker lines.
func pickEmbedLogger(stderr io.Writer, jsonOutput bool) *log.Logger {
	if jsonOutput {
		return log.New(log.Writer(), "", log.LstdFlags)
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
//
// The goroutine joins the shutdown drain group (issue #688). The initial scan
// writes to the same store and the same indexes that runUp closes on the way
// out, so shutdown must wait for it. Without the wait, a cancelled scan can
// still be inside a store write when st.Close runs.
func startIngestWorker(runCtx context.Context, readOnly bool, ing interface {
	Run(context.Context) error
}, indexingState *appstate.IndexingState, ingestErrCh chan error, bgWG *sync.WaitGroup) {
	if readOnly {
		close(ingestErrCh)
		return
	}
	if bgWG != nil {
		bgWG.Add(1)
	}
	go func() {
		if bgWG != nil {
			defer bgWG.Done()
		}
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

// watchable is implemented by ingestors that support continuous incremental
// indexing via a filesystem watcher.
type watchable interface {
	Watch(context.Context) error
}

// startWatchWorker launches the continuous-sync worker in the background when
// ingest.watch is enabled and the ingestor supports it. It runs concurrently
// with the initial scan; processDocument hash-diffs, so any overlap is a cheap
// no-op. Watcher failures are non-fatal — the watcher's own periodic safety
// rescan reconciles drift — so a setup error is logged rather than terminating
// the server.
//
// sourceKind is the configured source.kind. The worker still starts for a
// remote source, but the ingestor runs a periodic reconcile instead of the
// filesystem watcher (issue #695). The operator gets a warning here, because
// the mechanism they asked for is not the mechanism they get.
func startWatchWorker(runCtx context.Context, readOnly, enabled bool, sourceKind string, ing interface {
	Run(context.Context) error
}, stderr io.Writer, wg *sync.WaitGroup) {
	if !enabled {
		return
	}
	if readOnly {
		// Don't leave the knob silently inert — tell the operator continuous
		// sync is disabled because the server is read-only.
		_, _ = fmt.Fprintln(stderr, "warning: ingest.watch is enabled but ignored in read-only mode; continuous indexing is disabled")
		return
	}
	w, ok := ing.(watchable)
	if !ok {
		return
	}
	warnIfSourceHasNoFileWatch(sourceKind, stderr)
	// Register on the drain group so the deferred cancel()+Wait() in runUp waits
	// for this goroutine to exit before returning; its Watch error logging would
	// otherwise race a caller reading the shared sink after return (issue #419).
	if wg != nil {
		wg.Add(1)
	}
	go func() {
		if wg != nil {
			defer wg.Done()
		}
		if err := w.Watch(runCtx); err != nil && runCtx.Err() == nil {
			_, _ = fmt.Fprintf(stderr, "watch: %v\n", err)
		}
	}()
}

// warnIfSourceHasNoFileWatch tells the operator that ingest.watch does not start
// the filesystem watcher for the configured source, and says what runs instead.
//
// A remote corpus has no filesystem to watch. Before issue #695 the watcher
// started anyway and rooted itself at the local root_dir, so a local edit or
// delete could reprocess or tombstone the remote document with the same
// relative path. The watcher is now gated, and the message keeps the change
// visible: an operator who set ingest.watch expects a watcher, so the server
// must not swap the mechanism in silence. The gate itself lives in
// ingest.SourceSupportsFileWatch, so the CLI and the ingestor cannot disagree.
func warnIfSourceHasNoFileWatch(sourceKind string, stderr io.Writer) {
	if ingest.SourceSupportsFileWatch(sourceKind) {
		return
	}
	kind := strings.ToLower(strings.TrimSpace(sourceKind))
	_, _ = fmt.Fprintf(stderr,
		"warning: ingest.watch is enabled, but source.kind=%s has no filesystem to watch; "+
			"the file watcher is disabled and the corpus reconciles on a periodic rescan instead\n",
		kind)
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
	logSink io.Writer,
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
				writeCLIError(logSink, emitter.enabled, exitGeneric, fmt.Sprintf("server failed: %v", serverErr))
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
				_ = writeCorpusSnapshot(runCtx, cfg.StateDir, st, indexingState, logSink, emitter)
				continue
			}
			if ingestErr == nil {
				_ = writeCorpusSnapshot(runCtx, cfg.StateDir, st, indexingState, logSink, emitter)
				continue
			}
			writeCLIError(logSink, emitter.enabled, exitIngestionFatal, fmt.Sprintf("ingestion failed: %v", ingestErr))
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
			writeCLIError(logSink, emitter.enabled, exitGeneric, fmt.Sprintf("embedding worker warning: %v", embedErr))
			emitter.Emit("error", "embed_error", map[string]interface{}{
				"message": embedErr.Error(),
			})
		}
	}
}

// embeddedChunkPreloadPageSize is how many already-embedded chunks the startup
// warm-load asks for per page (issue #429 F6).
//
// It is deliberately large. The store re-evaluates its "all embedded chunks"
// filter for every page, so a page costs O(embedded chunks) no matter how many
// rows it returns and the walk costs O(N^2 / pageSize) in total: the page COUNT
// is the multiplier, not the page size. Measured on a synthetic 100k-chunk
// corpus (50k text + 50k code), cold-start preload was
//
//	page=500 -> 67.2s   page=2000 -> 19.0s   page=5000 -> 9.4s   page=20000 -> 4.1s
//
// with peak RSS flat across all of them (1.70-1.71 GB, dominated by the vectors
// themselves), because only one page of rows is live at a time. 5000 keeps the
// transient page small while removing ~85% of the wall time. The quadratic shape
// itself is a store-side fix and survives this constant.
const embeddedChunkPreloadPageSize = 5000

// preloadEmbeddedChunkMetadata warm-loads the retrieval metadata for every
// already-embedded chunk, one index kind at a time.
//
// The two walks are NOT run concurrently. They are genuinely independent (each
// pages over a disjoint, kind-scoped row set, and a chunk_id belongs to exactly
// one axis), but the store's listing runs on the single-connection writer handle
// rather than the #631 query-only read pool, so two concurrent walks serialize
// inside database/sql anyway: measured on the 100k-chunk corpus, concurrent
// walks took 62-67s against 59-67s serial, i.e. no gain outside the noise. Only
// the page size above moves that number.
func preloadEmbeddedChunkMetadata(ctx context.Context, source embeddedChunkLister, ret *retrieval.Service) (int, error) {
	if source == nil || ret == nil {
		return 0, nil
	}
	total := 0
	for _, kind := range vectorIndexKinds {
		loaded, err := preloadEmbeddedChunkMetadataForKind(ctx, source, ret, kind)
		total += loaded
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// preloadEmbeddedChunkMetadataForKind pages through one index kind's embedded
// chunks and registers each one's retrieval metadata. A store that does not
// implement the listing is treated as "nothing to preload" for that kind.
func preloadEmbeddedChunkMetadataForKind(ctx context.Context, source embeddedChunkLister, ret *retrieval.Service, kind string) (int, error) {
	const pageSize = embeddedChunkPreloadPageSize
	total := 0
	var afterChunkID int64
	for {
		tasks, err := source.ListEmbeddedChunkMetadata(ctx, kind, pageSize, afterChunkID)
		if err != nil {
			if errors.Is(err, model.ErrNotImplemented) {
				return total, nil
			}
			return total, err
		}
		for _, task := range tasks {
			// ToSearchHit carries every field the in-memory metadata needs,
			// including the representation Language (SPEC §5.2/§8.8) so the
			// per-language retrieval filter (§9.5) works against warm-loaded
			// metadata after a restart, not just freshly-indexed chunks.
			ret.SetChunkMetadataForIndex(kind, task.Metadata.ChunkID, task.Metadata.ToSearchHit())
			total++
		}
		if len(tasks) < pageSize {
			return total, nil
		}
		// Keyset seek: pages are ordered by chunk_id ascending, so carry the
		// last chunk_id forward instead of an OFFSET that rescans skipped rows.
		afterChunkID = int64(tasks[len(tasks)-1].Metadata.ChunkID)
	}
}

// Compile-time guard: the concrete store handed to startEmbeddingIfNotReadOnly
// MUST satisfy index.ChunkSource, otherwise the runtime `st.(index.ChunkSource)`
// assertion there fails silently and the embed worker never starts (issue #364).
// A signature drift on any ChunkSource method (e.g. MarkFailedWithCategory) now
// breaks the build instead of disabling embedding corpus-wide at runtime.
var _ index.ChunkSource = (*store.SQLiteStore)(nil)

// Compile-time guard: the concrete store handed to ingest.NewService MUST
// satisfy model.RepresentationStore, otherwise the runtime
// `store.(model.RepresentationStore)` assertion there fails silently and the
// representation generator (repGen) is never constructed — disabling ALL
// representation/chunk/embedding generation corpus-wide with no trace (#398, the
// #364 failure mode one layer up). A signature drift on any RepresentationStore
// method now breaks the build instead of silently dropping every document.
var _ model.RepresentationStore = (*store.SQLiteStore)(nil)

// Compile-time guard: the store MUST also satisfy index.EmbeddedVectorSource so
// the runtime `st.(index.EmbeddedVectorSource)` assertion in
// reconcileEmbeddedVectorsAtStartup keeps working — a signature drift would
// otherwise silently disable the #402 A2 crash-recovery reconciliation.
var _ index.EmbeddedVectorSource = (*store.SQLiteStore)(nil)

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
	textModel, codeModel, rootDir string,
	corpusFS corpusfs.CorpusFS,
	lateChunking bool,
	wg *sync.WaitGroup,
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
			RootDir:      rootDir,
			Corpus:       corpusFS,
			BatchSize:    32,
			Logger:       logger,
			LateChunking: lateChunking,
			OnIndexedChunk: func(label uint64, metadata model.ChunkMetadata) {
				if ret != nil {
					ret.SetChunkMetadataForIndex(workerKind, label, metadata.ToSearchHit())
				}
				if indexingState != nil {
					indexingState.AddEmbeddedOK(1)
				}
			},
		}

		if wg != nil {
			wg.Add(1)
		}
		go func() {
			if wg != nil {
				defer wg.Done()
			}
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

// loadCrossFileDedupHashes wires the rel_path → content_hash map used by
// retrieval-time cross-file de-duplication (SPEC 9.2) onto the retrieval
// service. It is a no-op when dedup is disabled, or when the store does not
// implement model.DocumentHashLister; a load error is non-fatal (dedup simply
// stays a pass-through) since it must never block server startup. A load failure
// is reported as a structured NDJSON warning event (machine-parseable in
// JSON/automation flows), mirroring the embedded-chunk-metadata bootstrap step.
func (a *App) loadCrossFileDedupHashes(ctx context.Context, cfg config.Config, st model.Store, ret *retrieval.Service, emitter *ndjsonEmitter) {
	if !cfg.DedupRetrieval || st == nil || ret == nil {
		return
	}
	lister, ok := st.(model.DocumentHashLister)
	if !ok {
		return
	}
	hashes, err := lister.ListDocumentHashes(ctx)
	if err != nil {
		if emitter != nil {
			emitter.Emit("warning", "bootstrap_cross_file_dedup_hashes", map[string]interface{}{
				"error": err.Error(),
			})
		}
		writef(a.stderr, "warning: load document hashes for retrieval dedup: %v\n", err)
		return
	}
	ret.SetDocumentHashes(hashes)
}

// corpusLanguagesLookupTimeout bounds the query-time store lookup that resolves
// the "auto" cross-lingual target set, so a slow/blocked backend degrades to no
// auto targets rather than hanging retrieval.
const corpusLanguagesLookupTimeout = 2 * time.Second

// configureCrossLingual wires server-side cross-lingual query expansion (#325)
// onto the retrieval service. It reuses the chat generator as the translate
// primitive (SPEC §8.6.2 uses the chat capability for translation) and, for the
// "auto" target set, registers a corpus-languages provider backed by the store's
// optional model.CorpusLanguageLister (resolving to the corpus's detected
// languages, #267). It is a no-op for behavior when disabled in config — the
// service then leaves search unchanged regardless of the wiring. Mirrors how
// SetMinScore / SetCrossFileDedupEnabled are wired from config; config-only, so
// no MCP tool-schema change.
func (a *App) configureCrossLingual(ret *retrieval.Service, cfg config.Config, st model.Store, translator model.Generator) {
	if ret == nil {
		return
	}
	ret.SetCrossLingual(cfg.CrossLingualEnabled, cfg.CrossLingualTargetLangs, translator)
	if lister, ok := st.(model.CorpusLanguageLister); ok && lister != nil {
		ret.SetCorpusLanguagesProvider(func() []string {
			// Bound the store lookup: this runs on the query-time retrieval path,
			// so a slow/blocked backend must not hang retrieval indefinitely.
			ctx, cancel := context.WithTimeout(context.Background(), corpusLanguagesLookupTimeout)
			defer cancel()
			langs, err := lister.ListCorpusLanguages(ctx)
			if err != nil {
				// Avoid logging the raw backend error: it can carry sensitive
				// connection details. The expansion degrades gracefully (no auto
				// targets) on failure.
				writef(a.stderr, "warning: list corpus languages for cross-lingual expansion failed\n")
				return nil
			}
			return langs
		})
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

// startManagedRecognizeBackend launches the recognition backend when the
// ingestor exposes the managed-lifecycle capability (design 0004 §3), tying the
// child to runCtx so shutdown terminates it. The capability is optional and
// discovered by type assertion (hooks may inject other ingestors). Returns
// exitSuccess when there is nothing to start or startup succeeds; on failure it
// writes the CLI error and returns exitIngestionFatal.
func (a *App) startManagedRecognizeBackend(runCtx context.Context, ing model.Ingestor, jsonOutput bool) int {
	rb, ok := ing.(interface {
		StartRecognizeBackend(context.Context) error
	})
	if !ok {
		return exitSuccess
	}
	if err := rb.StartRecognizeBackend(runCtx); err != nil {
		writeCLIError(a.stderr, jsonOutput, exitIngestionFatal, fmt.Sprintf("start recognize backend: %v", err))
		return exitIngestionFatal
	}
	return exitSuccess
}

// setCorpusFSIfSupported hands the resolved corpus filesystem to the ingestor
// when it accepts one (object-store backends); a no-op for ingestors that do
// not implement corpusFSSetter.
func setCorpusFSIfSupported(ing model.Ingestor, corpusFS corpusfs.CorpusFS) {
	if setter, ok := ing.(corpusFSSetter); ok {
		setter.SetCorpusFS(corpusFS)
	}
}

// corpusFSSetter is implemented by ingestors that accept an injected corpus
// filesystem backend (the concrete *ingest.Service does). Asserted so the
// configured backend (local/nfs/s3) drives discovery and reads.
type corpusFSSetter interface {
	SetCorpusFS(fsys corpusfs.CorpusFS)
}

// derivationCacheIdentityProvider is the subset of the ingest pipeline that
// exposes its ACTIVE OCR/transcript derivation identities (SPEC §8.6.7). The
// concrete *ingest.Service implements it. It is asserted on the live ingestor so
// the retriever's open_file cache lookup can be keyed the SAME identity-aware way
// ingest's writer keys the cache it wrote (issue #488) — using the ACTUAL active
// extractor/STT binding the ingestor runs with, not just a config re-derivation.
// A test ingestor that does not implement it simply leaves the retriever on the
// historical bytes-only key.
type derivationCacheIdentityProvider interface {
	ActiveOCRIdentity() string
	ActiveTranscriptIdentity() string
}

// wireDerivationCacheIdentities plumbs the live ingestor's ACTIVE OCR/transcript
// derivation identities (SPEC §8.6.7) into open_file's cache lookup so it keys the
// OCR/transcript cache the SAME identity-aware way ingest's writer does (issue
// #488). Sourced from the live ingestor (not a config re-derivation) so it
// reflects the exact extractor/STT binding this daemon writes cache entries with.
// An ingestor that does not expose the identities (a test fake) leaves the
// retriever on the historical bytes-only key.
func wireDerivationCacheIdentities(ret *retrieval.Service, ing model.Ingestor) {
	ids, ok := ing.(derivationCacheIdentityProvider)
	if !ok {
		return
	}
	ret.SetDerivationCacheIdentities(ids.ActiveOCRIdentity(), ids.ActiveTranscriptIdentity())
}

// sourceIsRemote reports whether the configured corpus source is an object-store
// backend (currently S3) rather than a local/NFS filesystem. Retrieval-time
// reads are routed through the corpus filesystem only for remote backends; local
// and NFS corpora keep the historical local-filesystem read path. Kind matching
// mirrors corpusfs.New (case-insensitive; empty normalizes to local).
func sourceIsRemote(cfg config.Config) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Source.Kind), "s3")
}

// buildCorpusFS constructs the corpus filesystem selected by cfg.Source
// (issue #244). The default kind (local) yields a LocalFS rooted at cfg.RootDir,
// preserving the historical behavior exactly. StateDir is always local and is
// where the S3 backend caches downloads.
func buildCorpusFS(ctx context.Context, cfg config.Config) (corpusfs.CorpusFS, error) {
	return corpusfs.New(ctx, corpusfs.Config{
		Kind:              cfg.Source.Kind,
		RootDir:           cfg.RootDir,
		StateDir:          cfg.StateDir,
		S3Bucket:          cfg.Source.S3Bucket,
		S3Prefix:          cfg.Source.S3Prefix,
		S3Region:          cfg.Source.S3Region,
		S3Endpoint:        cfg.Source.S3Endpoint,
		S3AccessKeyID:     cfg.Source.S3AccessKeyID,
		S3SecretAccessKey: cfg.Source.S3SecretAccessKey,
		S3SessionToken:    cfg.Source.S3SessionToken,
	})
}

// ingestorHooks carries the optional callbacks wireIngestorHooks registers on
// an ingestor. Any field may be nil, in which case that hook is not registered.
type ingestorHooks struct {
	indexingState *appstate.IndexingState
	evict         func([]string)
	onDocError    func(relPath, docType, message string)
	onDocSkip     func(relPath, docType, reason string)
}

// wireIngestorHooks connects the optional indexing-state, document-delete,
// document-error and document-skip notification interfaces on ing, if the
// concrete type supports them.
func wireIngestorHooks(ing model.Ingestor, hooks ingestorHooks) {
	if stateAware, ok := ing.(indexingStateAware); ok {
		stateAware.SetIndexingState(hooks.indexingState)
	}
	if notifier, ok := ing.(documentDeleteNotifier); ok {
		notifier.SetOnDocumentsDeleted(hooks.evict)
	}
	if notifier, ok := ing.(documentErrorNotifier); ok && hooks.onDocError != nil {
		notifier.SetOnDocumentError(hooks.onDocError)
	}
	if notifier, ok := ing.(documentSkipNotifier); ok && hooks.onDocSkip != nil {
		notifier.SetOnDocumentSkip(hooks.onDocSkip)
	}
}

// newFileErrorEmitter returns the per-document error callback handed to the
// ingestor. It emits the spec-required non-fatal `file_error` event (SPEC §3.2)
// carrying the document identity, so an operator tailing `--json` can see which
// document failed and why. The message arrives already secret-redacted by
// ingest.persistNonFatalDocError; it is passed through untouched.
func newFileErrorEmitter(emitter *ndjsonEmitter) func(relPath, docType, message string) {
	return func(relPath, docType, message string) {
		emitter.Emit("error", "file_error", map[string]interface{}{
			"rel_path": relPath,
			"doc_type": docType,
			"message":  message,
		})
	}
}

// newFileSkipEmitter returns the per-document skip callback handed to the
// ingestor. It emits the spec-required `file_skip` event (SPEC §3.2) at
// level=warn — the streaming counterpart of the durable `skip_reasons`
// aggregate: the aggregate says what was not indexed in total, the event says
// which file, as it happens. `reason` is a value of the `skip_reasons` enum.
func newFileSkipEmitter(emitter *ndjsonEmitter) func(relPath, docType, reason string) {
	return func(relPath, docType, reason string) {
		emitter.Emit("warn", "file_skip", map[string]interface{}{
			"rel_path": relPath,
			"doc_type": docType,
			"reason":   reason,
		})
	}
}

// reconcileEmbeddedVectorsAtStartup re-pends embedded chunks whose vectors are
// missing from a crash-recovered vector index (issue #402 A2), for both the text
// and code kinds. It is a no-op when the store does not expose the reconciliation
// surface or the backend is durable (index.ReconcileEmbeddedVectors handles the
// latter). Failures are logged, never fatal: a reconciliation error leaves the
// corpus no worse off than before, so it must not block server startup.
func reconcileEmbeddedVectorsAtStartup(ctx context.Context, readOnly bool, st model.Store, textIx, codeIx model.Index, emitter *ndjsonEmitter, stderr io.Writer) {
	if readOnly {
		return
	}
	source, ok := st.(index.EmbeddedVectorSource)
	if !ok {
		return
	}
	kinds := []struct {
		name string
		ix   model.Index
	}{
		{index.KindText, textIx},
		{index.KindCode, codeIx},
	}
	total := 0
	for _, k := range kinds {
		n, err := index.ReconcileEmbeddedVectors(ctx, source, k.ix, k.name)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Clean shutdown mid-reconciliation: expected, not worth a warning.
				return
			}
			writef(stderr, "warning: embedded-vector reconciliation (%s) failed: %v\n", k.name, err)
			if emitter != nil {
				emitter.Emit("warning", "embedded_vector_reconcile_failed", map[string]interface{}{
					"kind":    k.name,
					"message": err.Error(),
				})
			}
			continue
		}
		total += n
	}
	if total > 0 && emitter != nil {
		emitter.Emit("info", "embedded_vectors_repended", map[string]interface{}{
			"count":   total,
			"message": "re-pending embedded chunks whose vectors were missing after an ungraceful shutdown",
		})
	}
}

// startEmbeddingIfNotReadOnly starts embedding workers when readOnly is false
// and the store exposes the ChunkSource interface. When distributed embedding is
// enabled (issue #248, SPEC §8.7) it instead starts the in-process degenerate
// case of the coordinator+worker topology; otherwise it keeps the historical
// in-process embedding loop unchanged (local-first single-binary default, §1.2).
// It returns an error only for a distributed-mode SETUP failure (broker cannot be
// built, store lacks ChunkTaskByID, no embed identity), which the caller treats as
// fatal — the historical in-process path never errors here.
func startEmbeddingIfNotReadOnly(ctx context.Context, cfg config.Config, readOnly bool, st model.Store, textIx, codeIx model.Index, embedder model.Embedder, ret *retrieval.Service, indexingState *appstate.IndexingState, embedErrCh chan error, stderr io.Writer, jsonOutput bool, embedModelText, embedModelCode, rootDir string, corpusFS corpusfs.CorpusFS, emitter *ndjsonEmitter, wg *sync.WaitGroup) error {
	if readOnly {
		return nil
	}
	embedLogger := pickEmbedLogger(stderr, jsonOutput)
	chunkSource, ok := st.(index.ChunkSource)
	if !ok {
		// The compile-time guard (var _ index.ChunkSource = (*store.SQLiteStore)(nil))
		// keeps the shipped store satisfying ChunkSource, but a future/alternate
		// backend that does not would otherwise disable embedding corpus-wide with
		// no trace. Log loudly (to server.log via embedLogger) and emit a structured
		// NDJSON warning so the silent-failure of issue #364 can never recur.
		embedLogger.Printf("embedding disabled: store %T does not satisfy index.ChunkSource; pending chunks will never be embedded", st)
		if emitter != nil {
			emitter.Emit("warning", "embedding_disabled_no_chunk_source", map[string]interface{}{
				"store_type": fmt.Sprintf("%T", st),
				"message":    "store does not satisfy index.ChunkSource; embedding is disabled and pending chunks will never be embedded",
			})
		}
		return nil
	}
	if cfg.DistributedEmbed.Enabled {
		return startDistributedEmbedding(ctx, cfg, st, chunkSource, textIx, codeIx, embedder, ret, indexingState, embedErrCh, embedLogger, embedModelText, embedModelCode, rootDir, corpusFS, wg)
	}
	startEmbeddingWorkers(ctx, chunkSource, textIx, codeIx, embedder, ret, indexingState, embedErrCh, embedLogger, embedModelText, embedModelCode, rootDir, corpusFS, cfg.IngestLateChunking, wg)
	return nil
}

// bindServerListener binds the MCP server's TCP listener. It prefers the port a
// previous run recorded (via preferredListenAddr, #368) so a restart/upgrade
// re-binds the same ephemeral port and the URL baked into the Claude client
// config keeps working; if that port is now taken it falls back to a fresh
// ephemeral port so startup still succeeds. Returns the listener and exitSuccess,
// or (nil, exit code) after writing a CLI error. Extracted from runUp to keep it
// under the gocyclo budget.
func (a *App) bindServerListener(cfg config.Config, jsonOutput bool) (net.Listener, int) {
	listenAddr := preferredListenAddr(cfg.ListenAddr, cfg.StateDir)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil && listenAddr != cfg.ListenAddr {
		writef(a.stderr, "note: previous port (%s) unavailable, binding a new one (re-run `dir2mcp install claude` if Claude can't connect): %v\n", listenAddr, err)
		ln, err = net.Listen("tcp", cfg.ListenAddr)
	}
	if err != nil {
		writeCLIErrorWithCode(a.stderr, jsonOutput, exitServerBindFailure, protocol.ErrorCodeBindFailed, fmt.Sprintf("bind server: %v", err))
		return nil, exitServerBindFailure
	}
	return ln, exitSuccess
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
	a.emitConfigWarnings(cfg, opts.quiet)
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
	// Apply the validated chunking.* budgets to the chunker before ingestion
	// begins (#405): the chunker reads process-level effective sizes, so this
	// must run before any document is chunked.
	ingest.ConfigureChunking(cfg.ChunkingMaxTokens, cfg.ChunkingOverlapTokens)
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
	auth, err := prepareAuthMaterial(cfg, a.stderr)
	if err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitAuthOrPayment, fmt.Sprintf("auth setup: %v", err))
		return config.Config{}, authMaterial{}, "", "", false, exitAuthOrPayment
	}
	cfg.AuthMode = auth.mode
	cfg.ResolvedAuthToken = auth.token
	warnConfigSnapshotErr(a.stderr, opts.quiet, saveEffectiveConfigSnapshot(cfg, auth, x402TokenSource))
	return cfg, auth, tlsCertFile, tlsKeyFile, nonInteractiveMode, exitSuccess
}

// maybeFirstRunSetup runs the guided setup wizard (SPEC §147) when `up` starts
// interactively but no embedding provider resolves yet. It must be called from
// the launching parent (before any daemon fork) so it still owns the TTY; it
// persists the chosen corpus profile and credentials to disk, which the daemon
// child (or the foreground prepareUpConfig) then reloads. A skipped or aborted
// wizard is not an error — the normal §2.5 preflight reports the missing
// provider as before.
//
// upNonInteractiveMode also covers the daemon child (no TTY), so the wizard only
// ever runs in the interactive parent and is never shown twice.
func (a *App) maybeFirstRunSetup(opts upOptions) int {
	if !requiresMistralAPIKey(opts) || upNonInteractiveMode(opts) || opts.jsonOutput {
		return exitSuccess
	}
	cfg, err := loadConfigWithGlobalOptions(opts.globalOptions)
	if err != nil {
		return exitSuccess // let the standard config-load path report it
	}
	if _, rerr := cfg.Providers().Resolve(provider.CapEmbed); rerr == nil {
		return exitSuccess // already configured — no first-run prompt
	}

	se := a.sty(opts.jsonOutput)
	writef(a.stderr, "%s no embedding provider configured — starting guided setup\n", se.dim("•"))

	configPath := resolveConfigPath(opts.globalOptions)
	envPath := filepath.Join(filepath.Dir(configPath), ".env.local")
	configExisted := false
	if _, statErr := os.Stat(configPath); statErr == nil {
		configExisted = true
	}

	res, rerr := setupwizard.Run(setupwizard.Input{
		ExistingKeys:  setupwizard.DetectExistingKeys(envPath),
		ConfigExisted: configExisted,
	})
	if errors.Is(rerr, huh.ErrUserAborted) {
		return exitSuccess // fall through to the standard preflight error
	}
	if rerr != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("setup wizard: %v", rerr))
		return exitGeneric
	}

	// Persisted to disk (.env.local + .dir2mcp.yaml); the daemon child or the
	// foreground prepareUpConfig reloads config from disk afterward.
	return a.persistFirstRunSetup(opts, configPath, envPath, configExisted, res)
}

// persistFirstRunSetup applies the wizard's corpus profile to the config file
// and writes collected credentials to .env.local (mirroring `config init`), then
// adds .gitignore protection when inside a git repo.
func (a *App) persistFirstRunSetup(opts upOptions, configPath, envPath string, configExisted bool, res setupwizard.Result) int {
	fileCfg := config.Default()
	if configExisted {
		if existing, lerr := config.LoadFile(configPath); lerr == nil {
			fileCfg = existing
		}
	}
	setupwizard.ApplyCorpusProfile(&fileCfg, res.Profile)
	if err := config.SaveFile(configPath, fileCfg); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("save config file: %v", err))
		return exitGeneric
	}
	if _, err := setupwizard.PersistKeys(envPath, res.Keys, secretWriter(res.Destination)); err != nil {
		writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, fmt.Sprintf("save credentials: %v", err))
		return exitGeneric
	}
	if res.Destination == setupwizard.DestFile {
		a.protectSecretsFromGit(filepath.Dir(configPath))
	}
	return exitSuccess
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

// setupServerSingleInstance performs the preconditions that must happen
// BEFORE writing connection.json: the daemon-child SIGTERM handler and,
// in EVERY run mode, the single-instance pid-file lock. The handler is
// installed first (daemon child only) so a SIGTERM in the window between
// here and the event loop becomes a graceful cancel. The pid lock must
// come before connection.json so a second server racing for the same
// state dir loses deterministically and exits before touching anything.
//
// Crucially the lock is taken for foreground/service (`up --foreground`,
// the launchd/systemd target) too, not just the double-fork daemon child
// — two processes holding the same meta.sqlite + HNSW index files with
// divergent in-memory state corrupt the index (#434). The daemon child
// relies on its parent having reconciled a stale pid file first; the
// foreground/service path has no parent, so acquireSingleInstanceLock
// reconciles a dead owner itself before its O_EXCL claim.
//
// Returns the deferred cleanup closure (removes the pid file on exit)
// and an exit code; runUp returns immediately when the code is non-zero
// (the helper has already written the error).
func (a *App) setupServerSingleInstance(stateDir string, opts upOptions, cancel context.CancelFunc) (cleanup func(), code int) {
	cleanup = func() {}
	if isRunningAsDaemonChild() {
		installDaemonChildSignalHandler(cancel)
	}
	release, err := acquireSingleInstanceLock(stateDir)
	if err != nil {
		// Only the genuine already-running/locked case gets the "another
		// server is running" hint. Environmental failures (e.g. no
		// permission to create or write the pid file) surface their real
		// error unadorned so the hint does not mislead (#434).
		if errors.Is(err, errAlreadyRunning) {
			writeCLIError(a.stderr, opts.jsonOutput, exitGeneric,
				err.Error(),
				"Another dir2mcp server is already running for this state directory; check with `dir2mcp status` or stop it with `dir2mcp down`.",
			)
		} else {
			writeCLIError(a.stderr, opts.jsonOutput, exitGeneric, err.Error())
		}
		return cleanup, exitGeneric
	}
	return release, exitSuccess
}

// errAlreadyRunning marks the single-instance lock failure caused by a
// live server already owning the state dir (or winning the O_EXCL race),
// as distinct from environmental failures (permission creating/writing
// the pid file). The caller checks it with errors.Is so only the genuine
// already-running case gets the "another server is running" hint.
var errAlreadyRunning = errors.New("dir2mcp is already running")

// acquireSingleInstanceLock claims the per-state-dir pid file so at most
// one server process ever writes a given corpus's sqlite + index. It
// reconciles a stale pid file (owner process dead) so a clean restart is
// not blocked by a crash, then claims with O_EXCL so two live starters
// racing the same directory cannot both win. A live owner — or losing
// the O_EXCL race — returns an "already running" error the caller
// surfaces; the returned release removes the pid file on shutdown.
func acquireSingleInstanceLock(stateDir string) (release func(), err error) {
	pidPath := pidFilePath(stateDir)
	// Reconcile a stale pid file whose owner is gone so the O_EXCL claim
	// below has a clear field; a LIVE owner means a second writer, which
	// we must refuse.
	if existing, rerr := readPIDFile(pidPath); rerr == nil {
		if processIsAlive(existing) {
			return nil, fmt.Errorf("%w for %s (pid %d)", errAlreadyRunning, stateDir, existing)
		}
		_ = removePIDFile(pidPath)
	} else if !errors.Is(rerr, os.ErrNotExist) {
		// The pid file exists but is empty/corrupt/unparseable
		// (readPIDFile errored with something other than "not exist").
		// Its content names no live process, so treat it as a dead owner
		// and clear it — otherwise a single malformed pid file would
		// permanently brick startup: the O_EXCL claim below would fail
		// and every run would report a bogus "already running" (#434). A
		// live owner is never removed here because a live owner yields a
		// parseable pid (the rerr == nil branch above), not an error.
		_ = removePIDFile(pidPath)
	}
	if cerr := claimPIDFile(pidPath, os.Getpid()); cerr != nil {
		if errors.Is(cerr, os.ErrExist) {
			// Lost the race between the stale check and the claim: another
			// starter won. Surface its pid when we can still read it.
			if existing, rerr := readPIDFile(pidPath); rerr == nil {
				return nil, fmt.Errorf("%w for %s (pid %d)", errAlreadyRunning, stateDir, existing)
			}
			return nil, fmt.Errorf("%w for %s (pid file %s already claimed)", errAlreadyRunning, stateDir, pidPath)
		}
		return nil, fmt.Errorf("claim pid file %s: %w", pidPath, cerr)
	}
	return func() { _ = removePIDFile(pidPath) }, nil
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
