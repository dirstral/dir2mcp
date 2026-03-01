package retrieval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dir2mcp/internal/config"
	"dir2mcp/internal/index"
	"dir2mcp/internal/mistral"
	"dir2mcp/internal/model"
	"dir2mcp/internal/store"
)

type engineRetriever interface {
	Ask(ctx context.Context, question string, query model.SearchQuery) (model.AskResult, error)
	SetRAGSystemPrompt(prompt string)
	SetMaxContextChars(maxChars int)
	SetOversampleFactor(factor int)
}

type embeddedChunkMetadataSource interface {
	ListEmbeddedChunkMetadata(ctx context.Context, indexKind string, limit, offset int) ([]model.ChunkTask, error)
}

const (
	defaultEngineAskTimeout     = 120 * time.Second
	defaultEnginePreloadTimeout = 30 * time.Second
)

// Engine provides a convenience wrapper around retrieval.Service for callers
// that still rely on the legacy Engine API.
type Engine struct {
	retriever  engineRetriever
	closeFns   []func()
	closeOnce  sync.Once
	askTimeout time.Duration
}

// NewEngineForTesting creates an Engine with a caller-supplied retriever.
// It is primarily used by black-box tests in tests/retrieval to assert
// runtime behavior without requiring full on-disk state bootstrap.
func NewEngineForTesting(retriever interface {
	Ask(ctx context.Context, question string, query model.SearchQuery) (model.AskResult, error)
	SetRAGSystemPrompt(prompt string)
	SetMaxContextChars(maxChars int)
	SetOversampleFactor(factor int)
}) *Engine {
	return &Engine{
		retriever:  retriever,
		askTimeout: defaultEngineAskTimeout,
	}
}

// NewEngine creates a retrieval engine backed by the on-disk state.
//
// ctx must be non-nil; pass context.Background() or context.TODO() if no
// cancellation or deadline is needed. NewEngine returns an error immediately
// if ctx is nil.
func NewEngine(ctx context.Context, stateDir, rootDir string, cfg *config.Config) (*Engine, error) {
	if ctx == nil {
		return nil, fmt.Errorf("nil context passed to NewEngine")
	}

	effective := mergeEngineConfig(config.Default(), cfg)
	if trimmed := strings.TrimSpace(stateDir); trimmed != "" {
		effective.StateDir = trimmed
	}
	if trimmed := strings.TrimSpace(rootDir); trimmed != "" {
		effective.RootDir = trimmed
	}
	if strings.TrimSpace(effective.StateDir) == "" {
		effective.StateDir = filepath.Join(".", ".dir2mcp")
	}
	if strings.TrimSpace(effective.RootDir) == "" {
		effective.RootDir = "."
	}

	metadataStore := store.NewSQLiteStore(filepath.Join(effective.StateDir, "meta.sqlite"))
	if err := metadataStore.Init(ctx); err != nil && !errors.Is(err, model.ErrNotImplemented) {
		_ = metadataStore.Close()
		return nil, fmt.Errorf("initialize metadata store: %w", err)
	}

	textIndexPath := filepath.Join(effective.StateDir, "vectors_text.hnsw")
	textIndex := index.NewHNSWIndex(textIndexPath)
	if err := textIndex.Load(textIndexPath); err != nil && !errors.Is(err, model.ErrNotImplemented) && !errors.Is(err, os.ErrNotExist) {
		_ = metadataStore.Close()
		_ = textIndex.Close()
		return nil, fmt.Errorf("load text index: %w", err)
	}

	codeIndexPath := filepath.Join(effective.StateDir, "vectors_code.hnsw")
	codeIndex := index.NewHNSWIndex(codeIndexPath)
	if err := codeIndex.Load(codeIndexPath); err != nil && !errors.Is(err, model.ErrNotImplemented) && !errors.Is(err, os.ErrNotExist) {
		_ = metadataStore.Close()
		_ = textIndex.Close()
		_ = codeIndex.Close()
		return nil, fmt.Errorf("load code index: %w", err)
	}

	client := mistral.NewClient(effective.MistralBaseURL, effective.MistralAPIKey)
	if strings.TrimSpace(effective.ChatModel) != "" {
		client.DefaultChatModel = strings.TrimSpace(effective.ChatModel)
	}

	svc := NewService(metadataStore, textIndex, client, client)
	svc.SetCodeIndex(codeIndex)
	svc.SetRootDir(effective.RootDir)
	svc.SetStateDir(effective.StateDir)
	svc.SetProtocolVersion(effective.ProtocolVersion)
	svc.SetRAGSystemPrompt(effective.RAGSystemPrompt)
	svc.SetMaxContextChars(effective.RAGMaxContextChars)
	svc.SetOversampleFactor(effective.RAGOversampleFactor)

	if source, ok := interface{}(metadataStore).(embeddedChunkMetadataSource); ok {
		preloadCtx, cancel := context.WithTimeout(ctx, defaultEnginePreloadTimeout)
		// ensure the cancel function is always called, even if
		// preloadEngineChunkMetadata panics or returns early.
		defer cancel()
		total, err := preloadEngineChunkMetadata(preloadCtx, source, svc)
		if err != nil {
			_ = metadataStore.Close()
			_ = textIndex.Close()
			_ = codeIndex.Close()
			return nil, fmt.Errorf("preload chunk metadata: %w", err)
		}
		svc.logf("preloaded engine chunk metadata: %d", total)
	}

	return &Engine{
		retriever: svc,
		closeFns: []func(){
			func() { _ = metadataStore.Close() },
			func() { _ = textIndex.Close() },
			func() { _ = codeIndex.Close() },
		},
		askTimeout: defaultEngineAskTimeout,
	}, nil
}

func mergeEngineConfig(base config.Config, override *config.Config) config.Config {
	if override == nil {
		return base
	}

	merged := base
	if v := strings.TrimSpace(override.RootDir); v != "" {
		merged.RootDir = v
	}
	if v := strings.TrimSpace(override.StateDir); v != "" {
		merged.StateDir = v
	}
	if v := strings.TrimSpace(override.ListenAddr); v != "" {
		merged.ListenAddr = v
	}
	if v := strings.TrimSpace(override.MCPPath); v != "" {
		merged.MCPPath = v
	}
	if v := strings.TrimSpace(override.ProtocolVersion); v != "" {
		merged.ProtocolVersion = v
	}
	if override.Public {
		merged.Public = true
	}
	if v := strings.TrimSpace(override.AuthMode); v != "" {
		merged.AuthMode = v
	}
	if override.RateLimitRPS > 0 {
		merged.RateLimitRPS = override.RateLimitRPS
	}
	if override.RateLimitBurst > 0 {
		merged.RateLimitBurst = override.RateLimitBurst
	}
	if len(override.TrustedProxies) > 0 {
		merged.TrustedProxies = append([]string(nil), override.TrustedProxies...)
	}
	if len(override.PathExcludes) > 0 {
		merged.PathExcludes = append([]string(nil), override.PathExcludes...)
	}
	if len(override.SecretPatterns) > 0 {
		merged.SecretPatterns = append([]string(nil), override.SecretPatterns...)
	}
	if v := strings.TrimSpace(override.ResolvedAuthToken); v != "" {
		merged.ResolvedAuthToken = v
	}
	if v := strings.TrimSpace(override.MistralAPIKey); v != "" {
		merged.MistralAPIKey = v
	}
	if v := strings.TrimSpace(override.MistralBaseURL); v != "" {
		merged.MistralBaseURL = v
	}
	if v := strings.TrimSpace(override.ElevenLabsAPIKey); v != "" {
		merged.ElevenLabsAPIKey = v
	}
	if v := strings.TrimSpace(override.ElevenLabsBaseURL); v != "" {
		merged.ElevenLabsBaseURL = v
	}
	if v := strings.TrimSpace(override.ElevenLabsTTSVoiceID); v != "" {
		merged.ElevenLabsTTSVoiceID = v
	}
	if len(override.AllowedOrigins) > 0 {
		merged.AllowedOrigins = append([]string(nil), override.AllowedOrigins...)
	}
	if v := strings.TrimSpace(override.EmbedModelText); v != "" {
		merged.EmbedModelText = v
	}
	if v := strings.TrimSpace(override.EmbedModelCode); v != "" {
		merged.EmbedModelCode = v
	}
	if v := strings.TrimSpace(override.ChatModel); v != "" {
		merged.ChatModel = v
	}
	if v := strings.TrimSpace(override.RAGSystemPrompt); v != "" {
		merged.RAGSystemPrompt = v
	}
	if override.RAGMaxContextChars > 0 {
		merged.RAGMaxContextChars = override.RAGMaxContextChars
	}
	if override.RAGOversampleFactor > 0 {
		merged.RAGOversampleFactor = override.RAGOversampleFactor
	}

	return merged
}

// Close releases resources.
func (e *Engine) Close() {
	if e == nil {
		return
	}
	e.closeOnce.Do(func() {
		// execute in reverse order (LIFO) so resources are torn down
		// opposite the order in which they were added. nil checks are
		// retained from earlier implementation.
		for i := len(e.closeFns) - 1; i >= 0; i-- {
			if closeFn := e.closeFns[i]; closeFn != nil {
				closeFn()
			}
		}
	})
}

// AskOptions for Ask.
type AskOptions struct {
	K int
}

// AskResult is the result of Ask.
type AskResult struct {
	Answer    string
	Citations []Citation
}

// Citation references a source span.
type Citation struct {
	RelPath string
	Span    model.Span
}

// AskWithContext runs retrieval + generation and returns answer/citations using a caller-provided
// context. The context is wrapped with the engine's configured timeout so callers may also
// cancel early. This method replaces the old `Ask` which created its own background context
// and therefore ignored client cancellation.
//
// The function is lenient about the provided context: if `ctx` is nil it will be
// replaced with context.Background(), mirroring the policy used by NewEngine.
// Callers are encouraged to pass a non-nil context (e.g. context.Background() or
// context.TODO()) when possible.
func (e *Engine) AskWithContext(ctx context.Context, question string, opts AskOptions) (*AskResult, error) {
	if e == nil || e.retriever == nil {
		return nil, fmt.Errorf("retrieval engine not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, fmt.Errorf("question is required")
	}

	k := opts.K
	if k <= 0 {
		k = 10
	}

	timeout := e.askTimeout
	if timeout <= 0 {
		timeout = defaultEngineAskTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := e.retriever.Ask(ctx, question, model.SearchQuery{
		Query: question,
		K:     k,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("ask timed out after %s: %w", timeout, context.DeadlineExceeded)
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			// return a clearer cancellation error that wraps the sentinel
			// value so callers can reliably use errors.Is(...,
			// context.Canceled).
			return nil, fmt.Errorf("ask canceled: %w", context.Canceled)
		}
		return nil, err
	}

	citations := make([]Citation, 0, len(res.Citations))
	for _, citation := range res.Citations {
		citations = append(citations, Citation{
			RelPath: citation.RelPath,
			Span:    citation.Span,
		})
	}

	return &AskResult{
		Answer:    res.Answer,
		Citations: citations,
	}, nil
}

// Ask is maintained for backwards compatibility. It simply invokes
// AskWithContext with a background context so behaviour remains identical to
// previous versions (timeout-only semantics).
func (e *Engine) Ask(question string, opts AskOptions) (*AskResult, error) {
	return e.AskWithContext(context.Background(), question, opts)
}

// SetSystemPrompt updates the generation system prompt used by Ask.
func (e *Engine) SetSystemPrompt(prompt string) {
	if e == nil || e.retriever == nil {
		return
	}
	e.retriever.SetRAGSystemPrompt(prompt)
}

// SetMaxContextChars updates the context budget used in prompt assembly.
func (e *Engine) SetMaxContextChars(maxChars int) {
	if e == nil || e.retriever == nil {
		return
	}
	e.retriever.SetMaxContextChars(maxChars)
}

// SetOversampleFactor updates retrieval fanout during index search.
func (e *Engine) SetOversampleFactor(factor int) {
	if e == nil || e.retriever == nil {
		return
	}
	e.retriever.SetOversampleFactor(factor)
}

func preloadEngineChunkMetadata(ctx context.Context, source embeddedChunkMetadataSource, ret *Service) (int, error) {
	if source == nil || ret == nil {
		return 0, nil
	}
	const pageSize = 500
	total := 0
	for _, kind := range []string{"text", "code"} {
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
