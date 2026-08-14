package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/embedqueue"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

// embedWorkerOptions holds the parsed flags for the standalone embed-worker
// run mode. The lease/poll/retry knobs are optional tuning overrides; when zero
// embedqueue.Run applies its own defaults (30s / 500ms / 2s).
type embedWorkerOptions struct {
	leaseDuration time.Duration
	pollInterval  time.Duration
	retryAfter    time.Duration
}

// runEmbedWorker is the standalone distributed embed-worker run mode (issue
// #249, SPEC §8.7.1 — the compute-plane / embed-worker role packaged without
// serving). It is NOT a full daemon: it loads config, requires the distributed
// prerequisites (Tier-C shared store §8.7.4, a resolvable embed identity §8.1.4,
// a configured broker), opens the shared store + Tier-C index + CorpusFS +
// broker, builds per-axis embedders that reuse the in-process embed/index/mark
// path, and runs embedqueue.Run until interrupted. It deliberately starts NO MCP
// serving, NO coordinator, and NO discovery/ingest — it joins the pool, pulls
// jobs, reads corpus bytes via CorpusFS (§7.10), embeds, and writes vectors +
// chunk status back to the shared store.
func (a *App) runEmbedWorker(ctx context.Context, global globalOptions, args []string) int {
	opts, err := parseEmbedWorkerOptions(args)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("invalid embed-worker flags: %v", err))
		return exitConfigInvalid
	}

	cfg, code := a.loadEmbedWorkerConfig(global)
	if code != exitSuccess {
		return code
	}

	// Fail fast on missing distributed prerequisites BEFORE opening any handle:
	// a worker that cannot legally join the pool must not silently sit idle.
	if code := a.requireDistributedPrereqs(cfg, global); code != exitSuccess {
		return code
	}

	return a.runEmbedWorkerLoop(ctx, cfg, global, opts)
}

// parseEmbedWorkerOptions parses the embed-worker subcommand flags. All three
// tuning knobs are optional; an unset (or non-positive) value defers to the
// embedqueue.Run defaults.
func parseEmbedWorkerOptions(args []string) (embedWorkerOptions, error) {
	opts := embedWorkerOptions{}
	fs := flag.NewFlagSet("embed-worker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.DurationVar(&opts.leaseDuration, "lease-duration", 0, "visibility timeout for a leased job (default 30s)")
	fs.DurationVar(&opts.pollInterval, "poll-interval", 0, "wait after an empty queue before leasing again (default 500ms)")
	fs.DurationVar(&opts.retryAfter, "retry-after", 0, "delay before a transiently-failed job is redelivered (default 2s)")
	if err := fs.Parse(args); err != nil {
		return embedWorkerOptions{}, err
	}
	if fs.NArg() > 0 {
		return embedWorkerOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return opts, nil
}

// loadEmbedWorkerConfig loads the layered config and normalises the state
// directory, mirroring the other subcommands. Note config.Load already runs the
// distributed-embedding invariants (validateDistributedEmbed), so a config that
// enables the mode without a Tier-C store fails here with a remediable error.
func (a *App) loadEmbedWorkerConfig(global globalOptions) (config.Config, int) {
	cfg, err := loadConfigWithGlobalOptions(global)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("load config: %v", err))
		return config.Config{}, exitConfigInvalid
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = ".dir2mcp"
	}
	return cfg, exitSuccess
}

// requireDistributedPrereqs enforces the standalone-worker preconditions and
// FAILS FAST with a remediable error when any is unmet (SPEC §8.7.1/§8.7.4):
//
//   - distributed mode MUST be enabled — the worker has no role otherwise;
//   - the index backend MUST be a shared Tier-C store (qdrant/pgvector), since
//     the embedded Tier-A/B backends are single-node and unshareable (§8.7.4);
//   - the embed identity MUST resolve, so the worker can reject jobs from a
//     different vector space rather than mis-write (§8.1.4, §8.7.3);
//   - the broker MUST be cross-process (sqlite/external), NOT the in-process
//     "memory" default — a MemBroker is a process-local queue, so a standalone
//     worker on it creates its OWN empty queue, never sees the daemon's jobs,
//     and silently no-ops while printing "pulling jobs" (#434).
//
// The broker URL is a runtime-only secret (§16.1.1) and is never logged here.
func (a *App) requireDistributedPrereqs(cfg config.Config, global globalOptions) int {
	if !cfg.DistributedEmbed.Enabled {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"CONFIG_INVALID: embed-worker requires distributed embedding to be enabled",
			"Set distributed_embed.enabled=true (or DIR2MCP_DISTRIBUTED_EMBED_ENABLED=1) and configure a shared Tier-C store + broker.")
		return exitConfigInvalid
	}
	backend := strings.ToLower(strings.TrimSpace(cfg.IndexBackend))
	if backend != index.BackendQdrant && backend != index.BackendPgvector {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("CONFIG_INVALID: embed-worker requires a shared Tier-C vector store; index.backend=%q is single-node", cfg.IndexBackend),
			"Set index.backend=qdrant or index.backend=pgvector — a worker pool must write to a store reachable by all participants (SPEC §8.7.4).")
		return exitConfigInvalid
	}
	if broker := strings.ToLower(strings.TrimSpace(cfg.DistributedEmbed.Broker)); broker == "" || broker == "memory" {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			fmt.Sprintf("CONFIG_INVALID: embed-worker cannot use the in-process %q broker; its job queue is process-local, so a standalone worker never sees the daemon's jobs and would silently do nothing", embedWorkerBrokerLabel(cfg)),
			"Set distributed_embed.broker=sqlite (a queue db shared with the daemon) or an external broker so the worker and daemon share one queue (SPEC §8.7.4).")
		return exitConfigInvalid
	}
	if strings.TrimSpace(cfg.Providers().EmbedIdentity()) == "" {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"CONFIG_INVALID: embed-worker could not resolve an embed identity",
			"Configure an embedding provider (e.g. MISTRAL_API_KEY / OPENAI_API_KEY or providers: in .dir2mcp.yaml) so the worker can reject jobs from another vector space.")
		return exitConfigInvalid
	}
	return exitSuccess
}

// requireSharedStoreCapabilities asserts the two optional store capabilities a
// distributed worker cannot run without: read-by-id (ChunkTaskByID) so it can
// load the authoritative chunk for a leased job (§8.7.4), and the chunk source
// so EmbedAndIndex — and the terminal-failure path (#709) — can write status
// back. Both are satisfied by the SQLite-backed metadata store.
func (a *App) requireSharedStoreCapabilities(st model.Store, global globalOptions) (embedqueue.TaskFetcher, index.ChunkSource, int) {
	fetcher, ok := st.(embedqueue.TaskFetcher)
	if !ok {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"CONFIG_INVALID: metadata store does not support ChunkTaskByID (required for distributed embedding)")
		return nil, nil, exitConfigInvalid
	}
	chunkSource, ok := st.(index.ChunkSource)
	if !ok {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"CONFIG_INVALID: metadata store does not support the chunk source interface (required for distributed embedding)")
		return nil, nil, exitConfigInvalid
	}
	return fetcher, chunkSource, exitSuccess
}

// runEmbedWorkerLoop opens the shared store, the Tier-C indices, the corpus
// filesystem and the broker, builds per-axis embedders, and runs embedqueue.Run
// until the context is cancelled. Setup failures are remediable config errors;
// a clean interrupt (context cancellation) is exitSuccess.
func (a *App) runEmbedWorkerLoop(ctx context.Context, cfg config.Config, global globalOptions, opts embedWorkerOptions) int {
	st, textBuilt, codeBuilt, code := a.initStoreAndIndices(ctx, &cfg, global.jsonOutput)
	if code != exitSuccess {
		return code
	}
	defer func() { _ = st.Close() }()
	// A single-axis config (text-only or code-only) leaves the other index nil
	// (buildAxisEmbedders skips a nil axis), so guard before closing — calling
	// Close on a nil model.Index interface would panic.
	textIx, codeIx := textBuilt.index, codeBuilt.index
	defer func() {
		if textIx != nil {
			_ = textIx.Close()
		}
	}()
	defer func() {
		if codeIx != nil {
			_ = codeIx.Close()
		}
	}()

	fetcher, chunkSource, code := a.requireSharedStoreCapabilities(st, global)
	if code != exitSuccess {
		return code
	}

	embedder, _, textModel, codeModel := a.resolveModelClients(cfg)
	if embedder == nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid,
			"CONFIG_INVALID: embed-worker could not build an embedder client",
			"Verify the embedding provider credential is present and the provider is usable.")
		return exitConfigInvalid
	}

	corpusFS, err := buildCorpusFS(ctx, cfg)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("initialize corpus source: %v", err))
		return exitConfigInvalid
	}

	broker, err := buildEmbedBroker(ctx, cfg)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("build broker: %v", err))
		return exitConfigInvalid
	}
	defer func() { _ = broker.Close() }()

	logger := pickEmbedLogger(a.stderr, global.jsonOutput)
	// The standalone embed-worker (#249) has no local IndexingState, so it needs no
	// embedded_ok guard (the count hook is inert without a state to increment).
	embedders := buildAxisEmbedders(chunkSource, textIx, codeIx, embedder, nil, (*appstate.IndexingState)(nil), (*embedqueue.EmbeddedGuard)(nil), textModel, codeModel, cfg.RootDir, corpusFS, logger, ingest.ResolvedMaxFileBytes(cfg))
	if len(embedders) == 0 {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, "CONFIG_INVALID: no index axis configured for embed-worker")
		return exitConfigInvalid
	}

	// The corpus binding comes from the SHARED metadata store this worker reads
	// chunks out of, not from its own config, so a worker's identity and its data
	// plane are the same fact. A standalone worker is the case #708 is really
	// about: it is deployed against a broker built to be shared, and without a
	// binding it will execute whatever it is handed against whichever corpus its
	// store happens to be.
	corpusID, err := resolveCorpusID(ctx, cfg, st)
	if err != nil {
		writeCLIError(a.stderr, global.jsonOutput, exitConfigInvalid, fmt.Sprintf("embed-worker: %v", err))
		return exitConfigInvalid
	}

	identityStr := cfg.Providers().EmbedIdentity()
	workerCfg := embedqueue.Config{
		Broker:        broker,
		Fetcher:       fetcher,
		Embedders:     embedders,
		CorpusID:      corpusID,
		Status:        chunkSource,
		EmbedIdentity: identityStr,
		LeaseDuration: opts.leaseDuration,
		PollInterval:  opts.pollInterval,
		RetryAfter:    opts.retryAfter,
		Logger:        logger,
	}

	if !global.quiet && !global.jsonOutput {
		writef(a.stderr, "embed-worker: joining pool (backend=%s broker=%s); pulling jobs until interrupted\n",
			cfg.IndexBackend, embedWorkerBrokerLabel(cfg))
	}

	if rerr := embedqueue.Run(ctx, workerCfg); rerr != nil &&
		!errors.Is(rerr, context.Canceled) && !errors.Is(rerr, context.DeadlineExceeded) {
		writeCLIError(a.stderr, global.jsonOutput, exitGeneric, fmt.Sprintf("embed worker: %v", rerr))
		return exitGeneric
	}
	return exitSuccess
}

// embedWorkerBrokerLabel returns the non-sensitive broker selector for the
// startup line. It deliberately never includes BrokerURL, which is a runtime-only
// secret (§16.1.1) and must not be logged.
func embedWorkerBrokerLabel(cfg config.Config) string {
	if b := strings.TrimSpace(cfg.DistributedEmbed.Broker); b != "" {
		return b
	}
	return "memory"
}
