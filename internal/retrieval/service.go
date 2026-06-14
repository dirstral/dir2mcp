package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/dirstral/dir2mcp/internal/model"
)

var (
	// compiled regexes used by looksLikeCodeQuery; moved out of the
	// function to avoid rebuilding on every invocation.
	codeKeywordRe   = regexp.MustCompile(`\b(func|class|package|import|return|if|for|while|switch|case)\b`)
	codePunctRe     = regexp.MustCompile(`[(){}\[\];]`)
	fileExtensionRe = regexp.MustCompile(`\.(js|ts|py|go|java|rb|cpp|c|cs|html|css|json|yaml|yml)\b`)
	timePrefixRe    = regexp.MustCompile(`^\s*\[?(\d{1,2}):(\d{2})(?::(\d{2}))?\]?\s*(.*)$`)
)

var defaultPathExcludes = []string{
	"**/.git/**",
	"**/node_modules/**",
	"**/.dir2mcp/**",
	"**/.env",
	"**/*.pem",
	"**/*.key",
	"**/id_rsa",
}

var defaultSecretPatternLiterals = []string{
	`AKIA[0-9A-Z]{16}`,
	`(?i)(?:aws(?:[_\s.]{0,20})?secret(?:[_\s.]*(?:access[_\s.]*)?key)?|secret[_\s.]*access[_\s.]*key)\s*[:=]\s*[0-9A-Za-z/+=]{20,}`,

	`(?i)(?:authorization\s*[:=]\s*bearer\s+|(?:access|id|refresh)_token\s*[:=]\s*)[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`,
	`(?i)token\s*[:=]\s*[A-Za-z0-9_.-]{20,}`,
	`sk_[a-z0-9]{32}|api_[A-Za-z0-9]{32}`,
}

const (
	defaultOverfetchMultiplier = 5
	maxOverfetchMultiplier     = 100

	defaultRAGSystemPrompt = "Answer the question using only the provided context.\nInclude concise source attributions in the form [rel_path]."
	defaultRAGMaxContext   = 20000
	maxRAGMaxContext       = 200000
)

// Service implements retrieval operations over embedded data.
// It holds necessary components like store, index, embedder and
// supports configurable overfetching during searches. OverfetchMultiplier
// determines how many candidates the underlying index returns per
// requested hit. A multiplier of 1 means no overfetch; the default is 5
// which generally provides enough buffer for downstream filtering.
// Callers may tune the value via SetOverfetchMultiplier, and it is validated
// to be at least 1 (higher values are capped at 100 to avoid runaway work).
//
// NOTE: adjusting the multiplier can help when heavy filtering is applied
// or when `k` is large; a smaller value reduces work at the cost of
// potentially missing some matches.
//
// WARNING: changing this value after the service has been used may
// affect the semantics of subsequent searches.
//
// The field is unexported to encourage use of the setter where
// validation takes place.
//
// See NewService for default initialization details.

type Service struct {
	store               model.Store
	textIndex           model.Index
	codeIndex           model.Index
	embedder            model.Embedder
	gen                 model.Generator
	logger              *log.Logger
	indexingStateFn     func() bool
	textModel           string
	codeModel           string
	overfetchMultiplier int
	ragSystemPrompt     string
	ragMaxContextChars  int
	metaMu              sync.RWMutex
	chunkByLabel        map[uint64]model.SearchHit
	chunkByIndex        map[string]map[uint64]model.SearchHit
	rootDir             string
	stateDir            string
	protocolVersion     string
	pathExcludes        []string
	// cached compiled regexps for exclude patterns; keys are normalized patterns
	excludeRegexps map[string]*regexp.Regexp
	secretPatterns []*regexp.Regexp
	// hybridEnabled toggles BM25+vector RRF fusion in Search. Defaults to
	// true; the engine can disable it via SetHybridEnabled when the operator
	// sets retrieval.hybrid.enabled=false.
	hybridEnabled bool
	// reranker, when set and rerankEnabled, re-scores the fused candidate
	// pool before truncation to k (see SPEC 9.1.1). Fail-open: any error
	// falls back to the pre-rerank order.
	reranker            model.Reranker
	rerankEnabled       bool
	rerankModel         string
	rerankCandidatePool int
}

// compile-time assertion that Service implements model.Retriever.  This
// will fail to compile if the interface changes without updating this type.
var _ model.Retriever = (*Service)(nil)

func NewService(store model.Store, index model.Index, embedder model.Embedder, gen model.Generator) *Service {
	compiledPatterns := make([]*regexp.Regexp, 0, len(defaultSecretPatternLiterals))
	for _, pattern := range defaultSecretPatternLiterals {
		re, err := regexp.Compile(pattern)
		if err != nil {
			panic(fmt.Errorf("invalid default secret pattern %q: %w", pattern, err))
		}
		compiledPatterns = append(compiledPatterns, re)
	}
	// overfetchMultiplier defaults to 5; callers may override it with
	// SetOverfetchMultiplier to tune for their workload.  Values less than
	// 1 are silently bumped to 1, and values above 100 are capped.
	return &Service{
		store:               store,
		textIndex:           index,
		codeIndex:           index,
		embedder:            embedder,
		gen:                 gen,
		logger:              log.Default(),
		textModel:           "mistral-embed",
		codeModel:           "codestral-embed",
		overfetchMultiplier: defaultOverfetchMultiplier,
		ragSystemPrompt:     defaultRAGSystemPrompt,
		ragMaxContextChars:  defaultRAGMaxContext,
		chunkByLabel:        make(map[uint64]model.SearchHit),
		chunkByIndex: map[string]map[uint64]model.SearchHit{
			"text": make(map[uint64]model.SearchHit),
			"code": make(map[uint64]model.SearchHit),
		},
		rootDir:             ".",
		stateDir:            filepath.Join(".", ".dir2mcp"),
		protocolVersion:     "2025-11-25",
		excludeRegexps:      make(map[string]*regexp.Regexp),
		pathExcludes:        append([]string(nil), defaultPathExcludes...),
		secretPatterns:      compiledPatterns,
		hybridEnabled:       true,
		rerankCandidatePool: defaultRerankCandidatePool,
	}
}

// SetHybridEnabled toggles BM25+vector hybrid retrieval. The engine wires this
// from config.RetrievalHybridEnabled at construction time.
func (s *Service) SetHybridEnabled(enabled bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.hybridEnabled = enabled
}

// SetReranker wires an optional rerank provider. modelName is the
// provider model (empty = provider default); pool caps how many fused
// candidates are re-scored. pool <= 0 keeps the currently-configured
// value (default-initialized in NewService) so callers can swap the
// reranker without resetting an operator-tuned pool. Mirrors how the
// engine wires SetHybridEnabled from config.
func (s *Service) SetReranker(r model.Reranker, modelName string, pool int) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.reranker = r
	s.rerankModel = strings.TrimSpace(modelName)
	if pool > 0 {
		s.rerankCandidatePool = pool
	}
}

// SetRerankEnabled toggles the optional rerank stage. The engine wires
// this from config.RerankEnabled at construction time.
func (s *Service) SetRerankEnabled(enabled bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.rerankEnabled = enabled
}

func (s *Service) SetLogger(l *log.Logger) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	if l == nil {
		s.logger = log.Default()
		return
	}
	s.logger = l
}

// SetIndexingCompleteProvider sets a callback used to populate AskResult.IndexingComplete.
// The callback should return true when indexing is complete.
func (s *Service) SetIndexingCompleteProvider(fn func() bool) {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.indexingStateFn = fn
}

func (s *Service) logf(format string, args ...interface{}) {
	s.metaMu.RLock()
	logger := s.logger
	s.metaMu.RUnlock()
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}

// truncateQuestion returns a shortened representation of the question
// suitable for logging. If the original string is longer than 64
// characters it is trimmed and an ellipsis appended.  Empty input yields
// a placeholder so callers don't accidentally log an empty quoted string.
func truncateQuestion(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "<empty>"
	}
	const max = 64
	r := []rune(q)
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}

func (s *Service) SetQueryEmbeddingModel(modelName string) {
	if strings.TrimSpace(modelName) == "" {
		return
	}
	s.metaMu.Lock()
	s.textModel = modelName
	s.metaMu.Unlock()
}

func (s *Service) SetCodeEmbeddingModel(modelName string) {
	if strings.TrimSpace(modelName) == "" {
		return
	}
	s.metaMu.Lock()
	s.codeModel = modelName
	s.metaMu.Unlock()
}

func (s *Service) SetCodeIndex(index model.Index) {
	if index == nil {
		return
	}
	s.metaMu.Lock()
	s.codeIndex = index
	s.metaMu.Unlock()
}

func (s *Service) SetRootDir(root string) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "."
	}
	s.metaMu.Lock()
	s.rootDir = root
	s.metaMu.Unlock()
}

func (s *Service) SetStateDir(stateDir string) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		stateDir = filepath.Join(".", ".dir2mcp")
	}
	s.metaMu.Lock()
	s.stateDir = stateDir
	s.metaMu.Unlock()
}

func (s *Service) SetProtocolVersion(protocolVersion string) {
	protocolVersion = strings.TrimSpace(protocolVersion)
	if protocolVersion == "" {
		protocolVersion = "2025-11-25"
	}
	s.metaMu.Lock()
	s.protocolVersion = protocolVersion
	s.metaMu.Unlock()
}

func (s *Service) SetPathExcludes(patterns []string) {
	// merge defaults with caller-provided patterns so that hardcoded
	// security exclusions (.git, node_modules, .env, key/pem files) are
	// never silently dropped when the caller supplies custom patterns.
	merged := make([]string, 0, len(defaultPathExcludes)+len(patterns))
	merged = append(merged, defaultPathExcludes...)
	merged = append(merged, patterns...)
	compiled := make(map[string]*regexp.Regexp, len(merged))
	for _, pat := range merged {
		norm := strings.TrimSpace(filepath.ToSlash(pat))
		if norm == "" {
			continue
		}
		re, err := regexp.Compile(globToRegexp(norm))
		if err != nil {
			// ignore invalid pattern, it'll simply never match
			continue
		}
		compiled[norm] = re
	}

	s.metaMu.Lock()
	// record the merged set of exclusions (defaults + caller patterns) in
	// s.pathExcludes. this no longer reflects just the caller-provided
	// values but the full list used for matching; compiled regexps are
	// still held in s.excludeRegexps and matchExcludePattern will normalize
	// and consult the merged patterns when performing lookups.
	s.pathExcludes = merged
	s.excludeRegexps = compiled
	s.metaMu.Unlock()
}

func (s *Service) SetSecretPatterns(patterns []string) error {
	// start with compiled defaults so that baseline secret-detection
	// patterns (AWS keys, JWT tokens, etc.) are never dropped when callers
	// add custom patterns.
	compiled := make([]*regexp.Regexp, 0, len(defaultSecretPatternLiterals)+len(patterns))
	for _, pattern := range defaultSecretPatternLiterals {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("failed to compile default secret pattern %q: %w", pattern, err)
		}
		compiled = append(compiled, re)
	}
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return err
		}
		compiled = append(compiled, re)
	}
	s.metaMu.Lock()
	s.secretPatterns = compiled
	s.metaMu.Unlock()
	return nil
}

func (s *Service) SetChunkMetadata(label uint64, metadata model.SearchHit) {
	s.metaMu.Lock()
	s.chunkByLabel[label] = metadata
	s.chunkByIndex["text"][label] = metadata
	s.chunkByIndex["code"][label] = metadata
	s.metaMu.Unlock()
}

// EvictDocument removes all in-memory chunk metadata for one document.
func (s *Service) EvictDocument(relPath string) {
	s.EvictDocuments([]string{relPath})
}

// EvictDocuments removes all in-memory chunk metadata for the given
// documents. It is called when documents are tombstoned in the store so that
// their chunks no longer appear in search results for the remainder of the
// server session. The HNSW vector index has no delete support, so evicted
// labels will still be returned by the ANN search, but matchFilters will
// discard them because searchHitForLabel falls back to a stub with an empty
// RelPath.
func (s *Service) EvictDocuments(relPaths []string) {
	if len(relPaths) == 0 {
		return
	}
	normalized := make(map[string]struct{}, len(relPaths))
	for _, relPath := range relPaths {
		norm := strings.TrimSpace(filepath.ToSlash(relPath))
		if norm == "" {
			continue
		}
		normalized[norm] = struct{}{}
	}
	if len(normalized) == 0 {
		return
	}

	// First, scan under a read lock to find labels to delete without blocking
	// concurrent readers for the duration of the O(totalChunks) scan.
	s.metaMu.RLock()
	var labelsToDelete []uint64
	for label, hit := range s.chunkByLabel {
		if _, ok := normalized[strings.TrimSpace(filepath.ToSlash(hit.RelPath))]; ok {
			labelsToDelete = append(labelsToDelete, label)
		}
	}
	s.metaMu.RUnlock()

	if len(labelsToDelete) == 0 {
		return
	}

	// Now take the write lock only for the actual deletions.
	s.metaMu.Lock()
	for _, label := range labelsToDelete {
		delete(s.chunkByLabel, label)
		for _, byIndex := range s.chunkByIndex {
			delete(byIndex, label)
		}
	}
	s.metaMu.Unlock()
}

func (s *Service) SetChunkMetadataForIndex(indexName string, label uint64, metadata model.SearchHit) {
	kind := strings.ToLower(strings.TrimSpace(indexName))
	if kind != "text" && kind != "code" {
		s.SetChunkMetadata(label, metadata)
		return
	}

	s.metaMu.Lock()
	s.chunkByLabel[label] = metadata
	s.chunkByIndex[kind][label] = metadata
	s.metaMu.Unlock()
}

func (s *Service) SetRAGSystemPrompt(prompt string) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = defaultRAGSystemPrompt
	}
	s.metaMu.Lock()
	s.ragSystemPrompt = prompt
	s.metaMu.Unlock()
}

func (s *Service) SetMaxContextChars(maxChars int) {
	if maxChars <= 0 {
		maxChars = defaultRAGMaxContext
	}
	if maxChars > maxRAGMaxContext {
		maxChars = maxRAGMaxContext
	}
	s.metaMu.Lock()
	s.ragMaxContextChars = maxChars
	s.metaMu.Unlock()
}

// SetOversampleFactor changes retrieval fanout used for index search.
func (s *Service) SetOversampleFactor(factor int) {
	if factor < 1 {
		factor = 1
	}
	if factor > maxOverfetchMultiplier {
		factor = maxOverfetchMultiplier
	}
	s.metaMu.Lock()
	s.overfetchMultiplier = factor
	s.metaMu.Unlock()
}

// SetOverfetchMultiplier changes the multiplier used when querying the
// underlying vector index.  The service will ask for `k * multiplier`
// neighbors for a request that originally asked for `k` hits.  Values
// lower than 1 are bumped to 1 (no overfetch) and values greater than
// 100 are capped to prevent unreasonable work.  This method is safe to
// call concurrently.
func (s *Service) SetOverfetchMultiplier(m int) {
	s.SetOversampleFactor(m)
}

func (s *Service) Search(ctx context.Context, query model.SearchQuery) ([]model.SearchHit, error) {
	s.metaMu.RLock()
	textModel := s.textModel
	codeModel := s.codeModel
	textIndex := s.textIndex
	codeIndex := s.codeIndex
	s.metaMu.RUnlock()

	k := query.K
	if k <= 0 {
		k = 15
	}

	mode := strings.ToLower(strings.TrimSpace(query.Index))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "text":
		return s.searchSingleIndex(ctx, query.Query, k, textModel, textIndex, "text", query, true)
	case "code":
		return s.searchSingleIndex(ctx, query.Query, k, codeModel, codeIndex, "code", query, true)
	case "both":
		return s.searchBothIndices(ctx, query.Query, k, textModel, codeModel, textIndex, codeIndex, query)
	case "auto":
		if looksLikeCodeQuery(query.Query) {
			return s.searchSingleIndex(ctx, query.Query, k, codeModel, codeIndex, "code", query, true)
		}
		return s.searchSingleIndex(ctx, query.Query, k, textModel, textIndex, "text", query, true)
	default:
		return s.searchSingleIndex(ctx, query.Query, k, textModel, textIndex, "text", query, true)
	}
}

func (s *Service) Ask(ctx context.Context, question string, query model.SearchQuery) (model.AskResult, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return model.AskResult{}, errors.New("question is required")
	}

	if strings.TrimSpace(query.Query) == "" {
		query.Query = question
	}
	if query.K <= 0 {
		query.K = 15
	}

	hits, err := s.Search(ctx, query)
	if err != nil {
		return model.AskResult{}, err
	}

	citations := make([]model.Citation, 0, len(hits))
	for _, hit := range hits {
		citations = append(citations, model.Citation{
			ChunkID: hit.ChunkID,
			RelPath: hit.RelPath,
			Title:   hit.Title,
			Span:    hit.Span,
		})
	}

	answer := buildFallbackAnswer(question, hits)
	if s.gen != nil && len(hits) > 0 {
		s.metaMu.RLock()
		systemPrompt := s.ragSystemPrompt
		maxContextChars := s.ragMaxContextChars
		s.metaMu.RUnlock()
		prompt := buildRAGPrompt(question, hits, systemPrompt, maxContextChars)
		generated, genErr := s.gen.Generate(ctx, prompt)
		if genErr != nil {
			// log the error so callers have visibility; fall back to the
			// precomputed answer when generation fails.  avoid recording the
			// entire question in logs since it may contain sensitive data.
			safeQuestion := truncateQuestion(question)
			s.logf("generator error for question %q: %v", safeQuestion, genErr)
		} else {
			if trimmed := strings.TrimSpace(generated); trimmed != "" {
				answer = trimmed
			}
		}
	}
	answer = ensureAnswerAttributions(answer, citations)

	// use the shared accessor to determine whether indexing is complete;
	// this centralizes locking and nil-handling logic and avoids duplicating
	// the callback lookup that was previously done here.
	indexingComplete, _ := s.IndexingComplete(ctx)

	return model.AskResult{
		Question:         question,
		Answer:           answer,
		Citations:        citations,
		Hits:             hits,
		IndexingComplete: indexingComplete,
	}, nil
}

func (s *Service) OpenFile(ctx context.Context, relPath string, span model.Span, maxChars int) (string, error) {
	content, _, err := s.openFile(ctx, relPath, span, maxChars)
	return content, err
}

// IndexingComplete returns the current indexing state using the callback
// previously registered via SetIndexingCompleteProvider.  If no callback is
// available we conservatively report true (i.e. indexing complete) so that
// callers do not stall waiting for an event that cannot be delivered.
func (s *Service) IndexingComplete(ctx context.Context) (bool, error) {
	// grab the callback under lock, then release before doing any work.
	// this mirrors the pattern used elsewhere in the package and keeps the
	// critical section small.  We also respect the incoming context by
	// checking for cancellation before invoking the callback (which may
	// itself do expensive work or block).

	s.metaMu.RLock()
	indexingFn := s.indexingStateFn
	s.metaMu.RUnlock()

	if err := ctx.Err(); err != nil {
		// context already cancelled or expired; report that to caller rather
		// than potentially running the provider.
		return false, err
	}

	if indexingFn == nil {
		return true, nil
	}
	return indexingFn(), nil
}

func (s *Service) OpenFileWithMeta(ctx context.Context, relPath string, span model.Span, maxChars int) (string, bool, error) {
	return s.openFile(ctx, relPath, span, maxChars)
}

func normalizeOpenFileMaxChars(maxChars int) int {
	if maxChars <= 0 {
		return 20000
	}
	if maxChars > 50000 {
		return 50000
	}
	return maxChars
}

func isMetaSpanKind(kind string) bool {
	return kind == "page" || kind == "time"
}

func (s *Service) openFile(ctx context.Context, relPath string, span model.Span, maxChars int) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	relPath = strings.TrimSpace(relPath)
	if relPath == "" {
		return "", false, model.ErrForbidden
	}

	maxChars = normalizeOpenFileMaxChars(maxChars)

	s.metaMu.RLock()
	rootDir := s.rootDir
	stateDir := s.stateDir
	pathExcludes := append([]string(nil), s.pathExcludes...)
	secretPatterns := append([]*regexp.Regexp(nil), s.secretPatterns...)
	s.metaMu.RUnlock()
	if strings.TrimSpace(rootDir) == "" {
		rootDir = "."
	}

	normalizedRel, realRoot, targetAbs, err := s.resolveFilePath(relPath, rootDir, pathExcludes)
	if err != nil {
		return "", false, err
	}

	// A replace-mode media-only document (SPEC 8.1.7) has direct media chunks
	// but no text representation — a permanent condition. open_file returns the
	// non-retryable MEDIA_NO_TEXT (§15.4), never raw bytes and never the
	// retryable OCR_NOT_READY. Gated to media extensions so text files skip the
	// store lookup entirely.
	if isMediaDocExt(normalizedRel) && s.isMediaOnlyDoc(ctx, normalizedRel) {
		return "", false, model.ErrMediaNoText
	}

	kind := strings.ToLower(strings.TrimSpace(span.Kind))
	if content, truncated, handled, err := s.openFileFromMetadata(normalizedRel, span, maxChars, secretPatterns, kind); handled {
		return content, truncated, err
	}

	resolvedAbs, err := resolveSymlinkInRoot(targetAbs, realRoot, pathExcludes, s)
	if err != nil {
		return "", false, err
	}

	// For binary documents (PDF, audio) with no explicit span, return the
	// cached OCR/transcript markdown rather than the raw bytes. Without this,
	// callers that don't pass page=N or start_ms/end_ms get an unusable text
	// payload made of PDF stream bytes (see issue #177). Text-native types
	// (md, txt, code, html) keep the existing default of returning file bytes.
	if kind == "" && isBinaryDocType(normalizedRel) {
		content, truncated, err := s.openFileFromOCRCache(stateDir, resolvedAbs, normalizedRel, secretPatterns, maxChars)
		if err != nil {
			return "", false, err
		}
		return content, truncated, nil
	}

	return s.openFileFromResolvedPath(resolvedAbs, secretPatterns, kind, span, maxChars)
}

// chunkModalityChecker is the optional store capability used to classify a
// document as media-only (SPEC 8.1.7). Stores that don't implement it (e.g.
// test fakes) simply opt out, leaving open_file behavior unchanged.
type chunkModalityChecker interface {
	ChunkModalityPresence(ctx context.Context, relPath string) (hasMedia, hasText bool, err error)
}

// isMediaOnlyDoc reports whether relPath has direct media chunks but no
// text-bearing chunk — the permanent MEDIA_NO_TEXT condition. It is
// conservative: any lookup error or a store without the capability returns
// false, so open_file falls back to its existing behavior rather than
// misreporting MEDIA_NO_TEXT.
func (s *Service) isMediaOnlyDoc(ctx context.Context, relPath string) bool {
	s.metaMu.RLock()
	store := s.store
	s.metaMu.RUnlock()
	checker, ok := store.(chunkModalityChecker)
	if !ok {
		return false
	}
	hasMedia, hasText, err := checker.ChunkModalityPresence(ctx, relPath)
	if err != nil {
		s.logf("open_file: media-only check for %q failed: %v", relPath, err)
		return false
	}
	return hasMedia && !hasText
}

// Media extension predicates. These keep the media-only gate (isMediaDocExt),
// the text-serving binary classification (isBinaryDocType), and the cache
// candidate selector (openFileOCRCacheCandidates) over a single, consistent set
// so a format is never classified as media yet served as raw bytes.
func isImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".tif", ".tiff", ".svg":
		return true
	default:
		return false
	}
}

func isAudioExt(ext string) bool {
	switch ext {
	case ".mp3", ".wav", ".m4a", ".flac", ".aac", ".ogg", ".opus":
		return true
	default:
		return false
	}
}

func isVideoExt(ext string) bool {
	switch ext {
	case ".mp4", ".mov":
		return true
	default:
		return false
	}
}

// isMediaDocExt reports whether relPath's extension is an embeddable media type
// (SPEC 8.1.7). Used to gate the media-only store lookup so text/code files keep
// their fast path. Covers every media format the pipeline can ingest; video has
// no text fallback and is handled exclusively by the media-only guard
// (MEDIA_NO_TEXT), while images/PDF/audio also have a text-serving path below.
func isMediaDocExt(relPath string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	return ext == ".pdf" || isImageExt(ext) || isAudioExt(ext) || isVideoExt(ext)
}

// isBinaryDocType reports whether relPath has an extension whose contents are
// not human-readable as raw bytes but DO have a document-text representation
// (PDF/image OCR, audio transcript). For these the default open_file response
// serves the OCR/transcript cache rather than raw bytes. Video is excluded: it
// has no text representation, so a media-only video resolves to MEDIA_NO_TEXT.
func isBinaryDocType(relPath string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	return ext == ".pdf" || isImageExt(ext) || isAudioExt(ext)
}

// openFileFromOCRCache reads the precomputed OCR (or transcript) representation
// for a binary document. The cache layout mirrors what
// internal/ingest.Service.readOrComputeOCR / readOrComputeTranscript write:
// <stateDir>/cache/ocr/<sha256-of-source-bytes>.md for OCR, and
// <stateDir>/cache/transcribe/<sha256-of-source-bytes>*.txt for transcripts.
// When no cache file exists (e.g. ingest is still running), the function
// returns model.ErrOCRNotReady so callers can surface an actionable error
// rather than fall back to raw bytes.
func (s *Service) openFileFromOCRCache(stateDir, resolvedAbs, relPath string, secretPatterns []*regexp.Regexp, maxChars int) (string, bool, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		stateDir = filepath.Join(".", ".dir2mcp")
	}

	// Reject directories explicitly, mirroring openFileFromResolvedPath. Without
	// this guard os.Open succeeds on a directory and io.Copy on a directory file
	// descriptor surfaces as an opaque OS error that the MCP layer would map to
	// INTERNAL_ERROR; DOC_TYPE_UNSUPPORTED is the correct, actionable mapping.
	info, err := os.Stat(resolvedAbs)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, model.ErrDocTypeUnsupported
	}

	sourceFile, err := os.Open(resolvedAbs)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = sourceFile.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, sourceFile); err != nil {
		return "", false, err
	}
	hashHex := hex.EncodeToString(hasher.Sum(nil))

	candidates := openFileOCRCacheCandidates(stateDir, hashHex, relPath)
	for _, candidate := range candidates {
		data, readErr := os.ReadFile(candidate)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return "", false, readErr
		}
		text := string(data)
		if hasSecretMatch(secretPatterns, text) {
			return "", false, model.ErrForbidden
		}
		out, truncated := truncateRunesWithFlag(text, maxChars)
		return out, truncated, nil
	}
	return "", false, model.ErrOCRNotReady
}

// openFileOCRCacheCandidates returns the set of cache paths to consult, in
// preference order, for a given source document. Audio transcripts are stored
// with an optional language suffix (or none), so we glob for any matching
// file. PDFs use a single .md path.
func openFileOCRCacheCandidates(stateDir, hashHex, relPath string) []string {
	ext := strings.ToLower(filepath.Ext(relPath))
	switch {
	case ext == ".pdf" || isImageExt(ext):
		// PDF and image extraction both write extracted markdown to cache/ocr.
		return []string{filepath.Join(stateDir, "cache", "ocr", hashHex+".md")}
	case isAudioExt(ext):
		// transcripts are written as <hash>[<-lang>].txt; default-language
		// transcripts have no suffix, so the unsuffixed file is preferred.
		out := []string{filepath.Join(stateDir, "cache", "transcribe", hashHex+".txt")}
		matches, err := filepath.Glob(filepath.Join(stateDir, "cache", "transcribe", hashHex+"-*.txt"))
		if err == nil {
			sort.Strings(matches)
			out = append(out, matches...)
		}
		return out
	default:
		return nil
	}
}

func (s *Service) openFileFromMetadata(normalizedRel string, span model.Span, maxChars int, secretPatterns []*regexp.Regexp, kind string) (content string, truncated bool, handled bool, err error) {
	if !isMetaSpanKind(kind) || (kind == "time" && span.EndMS <= 0) {
		return "", false, false, nil
	}
	normalizedSpan := span
	normalizedSpan.Kind = kind
	fromMeta, ok := s.sliceFromMetadata(normalizedRel, normalizedSpan)
	if !ok {
		return "", false, false, nil
	}
	if hasSecretMatch(secretPatterns, fromMeta) {
		return "", false, true, model.ErrForbidden
	}
	out, truncated := truncateRunesWithFlag(fromMeta, maxChars)
	return out, truncated, true, nil
}

func (s *Service) openFileFromResolvedPath(resolvedAbs string, secretPatterns []*regexp.Regexp, kind string, span model.Span, maxChars int) (string, bool, error) {
	info, err := os.Stat(resolvedAbs)
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, model.ErrDocTypeUnsupported
	}

	raw, readTruncated, err := readFileBounded(resolvedAbs, 0)
	if err != nil {
		return "", false, err
	}
	content := string(raw)

	if hasSecretMatch(secretPatterns, content) {
		return "", false, model.ErrForbidden
	}

	selected, err := sliceContentBySpan(content, kind, span)
	if err != nil {
		return "", false, err
	}

	out, outTruncated := truncateRunesWithFlag(selected, maxChars)
	return out, readTruncated || outTruncated, nil
}

// hasSecretMatch reports whether any of the compiled secret patterns matches s.
func hasSecretMatch(patterns []*regexp.Regexp, s string) bool {
	for _, re := range patterns {
		if re != nil && re.MatchString(s) {
			return true
		}
	}
	return false
}

// resolveFilePath validates and resolves relPath relative to rootDir.
// It returns the normalized relative path, the real root (with symlinks
// resolved), the absolute target path, and any validation error.
func (s *Service) resolveFilePath(relPath, rootDir string, pathExcludes []string) (normalizedRel, realRoot, targetAbs string, err error) {
	normalizedRel = filepath.ToSlash(filepath.Clean(relPath))
	isTraversal := normalizedRel == "." || strings.HasPrefix(normalizedRel, "../") || normalizedRel == ".."
	if isTraversal || filepath.IsAbs(relPath) {
		return "", "", "", model.ErrPathOutsideRoot
	}
	for _, pattern := range pathExcludes {
		if s.matchExcludePattern(pattern, normalizedRel) {
			return "", "", "", model.ErrForbidden
		}
	}

	rootAbs, absErr := filepath.Abs(rootDir)
	if absErr != nil {
		return "", "", "", absErr
	}
	realRoot = rootAbs
	if resolvedRoot, rootErr := filepath.EvalSymlinks(rootAbs); rootErr != nil {
		if !errors.Is(rootErr, os.ErrNotExist) {
			return "", "", "", model.ErrForbidden
		}
	} else {
		realRoot = resolvedRoot
	}

	targetAbs = filepath.Join(realRoot, filepath.FromSlash(normalizedRel))
	relFromRoot, relErr := filepath.Rel(realRoot, targetAbs)
	if relErr != nil || relFromRoot == ".." || strings.HasPrefix(relFromRoot, ".."+string(os.PathSeparator)) {
		return "", "", "", model.ErrPathOutsideRoot
	}
	return normalizedRel, realRoot, targetAbs, nil
}

// resolveSymlinkInRoot follows symlinks on targetAbs and validates the result
// is still within realRoot and not excluded by pathExcludes.
func resolveSymlinkInRoot(targetAbs, realRoot string, pathExcludes []string, s *Service) (resolvedAbs string, err error) {
	resolvedAbs, err = filepath.EvalSymlinks(targetAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		// fail closed: any error other than not-exist means we cannot
		// confirm the path stays within root, so deny the request.
		return "", model.ErrForbidden
	}
	resolvedRel, relErr := filepath.Rel(realRoot, resolvedAbs)
	if relErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(os.PathSeparator)) {
		return "", model.ErrPathOutsideRoot
	}
	resolvedRel = filepath.ToSlash(filepath.Clean(resolvedRel))
	for _, pattern := range pathExcludes {
		if s.matchExcludePattern(pattern, resolvedRel) {
			return "", model.ErrForbidden
		}
	}
	return resolvedAbs, nil
}

// sliceContentBySpan extracts the portion of content identified by the given
// span kind. An empty kind or "lines" applies line-range slicing; "page" and
// "time" select the relevant page/time segment.
func sliceContentBySpan(content, kind string, span model.Span) (string, error) {
	switch kind {
	case "", "lines":
		if kind == "lines" || span.StartLine > 0 || span.EndLine > 0 {
			return sliceLines(content, span.StartLine, span.EndLine), nil
		}
		return content, nil
	case "page":
		page := span.Page
		if page <= 0 {
			page = 1
		}
		// metadata-backed OCR handled above; fall back to slicing pages directly
		paged, ok := slicePage(content, page)
		if !ok {
			return "", model.ErrDocTypeUnsupported
		}
		return paged, nil
	case "time":
		return sliceTimeSpan(content, span)
	default:
		return "", model.ErrDocTypeUnsupported
	}
}

// sliceTimeSpan normalises the time boundaries and extracts the matching
// transcript segment from content.
func sliceTimeSpan(content string, span model.Span) (string, error) {
	startMS := span.StartMS
	endMS := span.EndMS
	if startMS < 0 {
		startMS = 0
	}
	if endMS < 0 {
		endMS = 0
	}
	if endMS > 0 && endMS < startMS {
		endMS = startMS
	}
	// metadata-backed slices for time spans are handled earlier; just extract
	timeSlice, ok := sliceTime(content, startMS, endMS)
	if !ok {
		return "", model.ErrDocTypeUnsupported
	}
	return timeSlice, nil
}

func (s *Service) Stats(ctx context.Context) (model.Stats, error) {
	if err := ctx.Err(); err != nil {
		return model.Stats{}, err
	}

	s.metaMu.RLock()
	rootDir := strings.TrimSpace(s.rootDir)
	stateDir := strings.TrimSpace(s.stateDir)
	protocolVersion := strings.TrimSpace(s.protocolVersion)
	st := s.store
	s.metaMu.RUnlock()

	if rootDir == "" {
		rootDir = "."
	}
	if stateDir == "" {
		stateDir = filepath.Join(rootDir, ".dir2mcp")
	}
	if protocolVersion == "" {
		protocolVersion = "2025-11-25"
	}

	out := model.Stats{
		Root:            rootDir,
		StateDir:        stateDir,
		ProtocolVersion: protocolVersion,
		CorpusStats:     model.CorpusStats{DocCounts: map[string]int64{}},
	}
	if st == nil {
		return out, nil
	}

	if agg, ok := st.(interface {
		CorpusStats(context.Context) (model.CorpusStats, error)
	}); ok {
		corpusStats, err := agg.CorpusStats(ctx)
		if err == nil {
			return applyCorpusStats(out, corpusStats), nil
		}
		if !errors.Is(err, model.ErrNotImplemented) {
			return model.Stats{}, err
		}
	}

	docStatus, docCounts, totalDocs, err := collectStoreStatsFallback(ctx, st)
	if err != nil {
		return model.Stats{}, err
	}
	out.DocCounts = docCounts
	out.TotalDocs = totalDocs
	out.Scanned = docStatus.scanned
	out.Indexed = docStatus.indexed
	out.Skipped = docStatus.skipped
	out.Deleted = docStatus.deleted
	out.Errors = docStatus.errors
	return out, nil
}

type docStatusCounts struct {
	scanned int64
	indexed int64
	skipped int64
	deleted int64
	errors  int64
}

func applyCorpusStats(base model.Stats, corpus model.CorpusStats) model.Stats {
	base.Scanned = corpus.Scanned
	base.Indexed = corpus.Indexed
	base.Skipped = corpus.Skipped
	base.Deleted = corpus.Deleted
	base.Representations = corpus.Representations
	base.ChunksTotal = corpus.ChunksTotal
	base.EmbeddedOK = corpus.EmbeddedOK
	base.Errors = corpus.Errors
	base.TotalDocs = corpus.TotalDocs
	if len(corpus.DocCounts) == 0 {
		base.DocCounts = map[string]int64{}
	} else {
		base.DocCounts = make(map[string]int64, len(corpus.DocCounts))
		for docType, count := range corpus.DocCounts {
			base.DocCounts[docType] = count
		}
	}
	return base
}

func collectStoreStatsFallback(ctx context.Context, st model.Store) (docStatusCounts, map[string]int64, int64, error) {
	const pageSize = 500
	offset := 0
	counts := make(map[string]int64)
	status := docStatusCounts{}
	var totalDocs int64

	for {
		docs, total, err := st.ListFiles(ctx, "", "", pageSize, offset)
		if err != nil {
			return docStatusCounts{}, nil, 0, err
		}
		for _, doc := range docs {
			status.scanned++
			if doc.Deleted {
				status.deleted++
				continue
			}

			docType := strings.TrimSpace(doc.DocType)
			if docType == "" {
				docType = "unknown"
			}
			counts[docType]++
			totalDocs++

			switch strings.ToLower(strings.TrimSpace(doc.Status)) {
			case "skipped":
				status.skipped++
			case "error":
				status.errors++
			default:
				status.indexed++
			}
		}

		offset += len(docs)
		if len(docs) == 0 || int64(offset) >= total {
			break
		}
	}

	return status, counts, totalDocs, nil
}

func (s *Service) searchHitForLabel(indexName string, label uint64) model.SearchHit {
	s.metaMu.RLock()
	if byIndex, ok := s.chunkByIndex[indexName]; ok {
		if meta, exists := byIndex[label]; exists {
			s.metaMu.RUnlock()
			meta.ChunkID = label
			return meta
		}
	}
	meta, ok := s.chunkByLabel[label]
	s.metaMu.RUnlock()

	if ok {
		meta.ChunkID = label
		return meta
	}

	return model.SearchHit{
		ChunkID: label,
		RelPath: "",
		DocType: "unknown",
		RepType: "unknown",
		Snippet: "",
		Span:    model.Span{Kind: "lines"},
	}
}

// defaultRerankCandidatePool bounds how many fused candidates are sent
// to the rerank provider before truncation to k. 50 matches the
// hybrid candidate pool and keeps latency/cost predictable.
const defaultRerankCandidatePool = 50

// ErrMissingEmbedder is returned when the service was created without
// a configured embedder and a search attempt is made. This prevents a
// nil-pointer panic in searchSingleIndex while giving callers a clear
// failure reason.
var ErrMissingEmbedder = errors.New("embedder not configured")

func (s *Service) searchSingleIndex(ctx context.Context, query string, k int, modelName string, idx model.Index, indexName string, filters model.SearchQuery, allowRerank bool) ([]model.SearchHit, error) {
	if s.embedder == nil {
		// caller should have provided an embedder via NewService or
		// SetEmbedder (not currently available).  Return an explicit
		// error rather than letting the nil dereference panic later.
		return nil, ErrMissingEmbedder
	}
	vectors, err := s.embedder.Embed(ctx, modelName, model.EmbedQuery, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return []model.SearchHit{}, nil
	}
	if idx == nil {
		return []model.SearchHit{}, nil
	}

	vectorCandidateLimit := s.hybridVectorCandidateLimit(k, idx)
	filtered, err := s.collectVectorCandidates(ctx, vectors[0], idx, indexName, filters, vectorCandidateLimit)
	if err != nil {
		return nil, err
	}
	if fused, ok := s.runHybridSearch(ctx, query, k, indexName, filtered); ok {
		fused = dedupMediaCandidates(fused)
		if allowRerank {
			return s.rerankPool(ctx, query, fused, k), nil
		}
		return truncateSearchHits(fused, k), nil
	}
	filtered = dedupMediaCandidates(filtered)
	if allowRerank {
		return s.rerankPool(ctx, query, filtered, k), nil
	}
	return truncateSearchHits(filtered, k), nil
}

// isMediaHit reports whether a candidate is a direct media chunk (SPEC 8.1.7).
func isMediaHit(h model.SearchHit) bool {
	switch strings.ToLower(strings.TrimSpace(h.Modality)) {
	case "image", "audio", "video", "pdf":
		return true
	default:
		return false
	}
}

// hitPage returns the document page a hit localizes to, if any. Page spans carry
// it directly; region spans (structured extraction) carry it in the region.
func hitPage(h model.SearchHit) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(h.Span.Kind)) {
	case "page":
		if h.Span.Page > 0 {
			return h.Span.Page, true
		}
	case "region":
		if h.Span.Region != nil && h.Span.Region.StartPage > 0 {
			return h.Span.Region.StartPage, true
		}
	}
	return 0, false
}

func mediaPageKey(relPath string, page int) string {
	return relPath + "\x00" + strconv.Itoa(page)
}

// dedupMediaCandidates implements the SPEC 8.1.7 page-image dedup: drop a media
// page-image candidate for (rel_path, page) only when a text/region candidate
// for that same page survives, so a page is not double-counted. It runs BEFORE
// truncation/rerank. Audio/video time-window media chunks, and media on pages
// with no competing text/region candidate, are kept; distinct text/region
// chunks are never collapsed into each other. Order is preserved.
func dedupMediaCandidates(hits []model.SearchHit) []model.SearchHit {
	textPages := make(map[string]struct{})
	for _, h := range hits {
		if isMediaHit(h) {
			continue
		}
		if p, ok := hitPage(h); ok {
			textPages[mediaPageKey(h.RelPath, p)] = struct{}{}
		}
	}
	if len(textPages) == 0 {
		return hits
	}
	out := make([]model.SearchHit, 0, len(hits))
	for _, h := range hits {
		if isMediaHit(h) {
			if p, ok := hitPage(h); ok {
				if _, dup := textPages[mediaPageKey(h.RelPath, p)]; dup {
					continue
				}
			}
		}
		out = append(out, h)
	}
	return out
}

// hybridVectorCandidateLimit returns the number of vector candidates to
// request before fusion. When hybrid retrieval is active and the store
// supports BM25, we widen the vector pool to match hybridCandidatePoolSize so
// that vector candidates outside the top-k can still contribute via RRF.
// Otherwise we request only k candidates (the legacy vector-only path).
func (s *Service) hybridVectorCandidateLimit(k int, idx model.Index) int {
	if k <= 0 || idx == nil {
		return k
	}
	s.metaMu.RLock()
	enabled := s.hybridEnabled
	store := s.store
	s.metaMu.RUnlock()
	if !enabled {
		return k
	}
	if _, ok := store.(model.LexicalSearcher); !ok {
		return k
	}
	if hybridCandidatePoolSize > k {
		return hybridCandidatePoolSize
	}
	return k
}

// rerankPool applies the optional rerank stage to a fused candidate
// pool and returns the best k. When rerank is disabled or no reranker
// is configured it is exactly truncateSearchHits(hits, k). It is
// fail-open: any provider error keeps the pre-rerank order. Output is
// deterministically ordered (relevance desc, then chunk_id asc) so ties
// don't depend on provider response ordering (SPEC 9.1.1). Extracted
// from the search paths to keep their cyclomatic complexity in budget.
func (s *Service) rerankPool(ctx context.Context, query string, hits []model.SearchHit, k int) []model.SearchHit {
	s.metaMu.RLock()
	enabled := s.rerankEnabled
	rr := s.reranker
	rmodel := s.rerankModel
	pool := s.rerankCandidatePool
	s.metaMu.RUnlock()

	if !enabled || rr == nil || len(hits) <= 1 {
		return truncateSearchHits(hits, k)
	}
	if pool <= 0 {
		pool = defaultRerankCandidatePool
	}
	// fused keeps the full pre-rerank order for fail-open fallback;
	// cand is the (capped) slice actually sent to the provider. Never
	// shrink the fallback to the pool: a misconfigured pool < k must
	// still return up to k results on a rerank failure (and on success
	// the un-reranked tail is appended below).
	fused := hits
	cand := fused
	if len(cand) > pool {
		cand = cand[:pool]
	}
	docs := make([]string, len(cand))
	for i, h := range cand {
		docs[i] = h.Snippet
	}
	results, err := rr.Rerank(ctx, rmodel, query, docs, k)
	if err != nil || len(results) == 0 {
		if err != nil {
			s.logf("rerank: provider error, falling back to fused order: %v", err)
		}
		return truncateSearchHits(fused, k)
	}
	out := make([]model.SearchHit, 0, len(fused))
	for _, r := range results {
		if r.Index < 0 || r.Index >= len(cand) {
			s.logf("rerank: out-of-range index %d, falling back to fused order", r.Index)
			return truncateSearchHits(fused, k)
		}
		h := cand[r.Index]
		h.Score = r.RelevanceScore
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	// Preserve fused candidates beyond the reranked pool so rerank only
	// reorders and never returns fewer than k when more were available.
	if len(fused) > len(cand) {
		out = append(out, fused[len(cand):]...)
	}
	return truncateSearchHits(out, k)
}

func truncateSearchHits(hits []model.SearchHit, k int) []model.SearchHit {
	if k < 0 {
		k = 0
	}
	if len(hits) <= k {
		return hits
	}
	return hits[:k]
}

// filterFromQuery projects the retrieval-level SearchQuery predicates into the
// backend-agnostic model.Filter (issue #247). ExcludeOrphans is intentionally
// NOT pushed down: orphan/eviction state lives in the retrieval service's
// in-memory chunk metadata, not in the backend payload, so it is enforced by
// the post-materialization matchFilters re-check instead (a backend payload
// always carries a rel_path).
func filterFromQuery(q model.SearchQuery) model.Filter {
	return model.Filter{
		PathPrefix: q.PathPrefix,
		PathGlob:   q.FileGlob,
		DocTypes:   q.DocTypes,
	}
}

// collectVectorCandidates gathers up to k filtered dense-vector candidates. When
// the index can evaluate the filter itself (FilteringIndex + CanFilter) the
// filter is pushed down to narrow the backend's candidate pool; otherwise an
// empty filter is passed and the predicates are evaluated in Go. In both cases
// the same widening overfetch loop runs and matchFilters is re-applied so the
// in-memory orphan/eviction state is honoured. Extracted from searchSingleIndex
// so the outer function stays under the cyclomatic-complexity budget after
// hybrid fusion was layered on top.
func (s *Service) collectVectorCandidates(
	ctx context.Context,
	vector []float32,
	idx model.Index,
	indexName string,
	filters model.SearchQuery,
	k int,
) ([]model.SearchHit, error) {
	filter := filterFromQuery(filters)
	// Only push the filter down when the backend can evaluate it itself;
	// otherwise pass an empty filter so payload-blind backends behave as
	// before. Either way we run the same widening overfetch loop and re-apply
	// matchFilters in Go: pushing the filter down narrows the candidate pool
	// the backend returns, but the in-memory orphan/eviction state (which the
	// backend payload does not know about, see searchHitFromIndexHit) is still
	// enforced by the post-materialization re-check. Without the widening loop a
	// pushed-down search would under-return whenever the re-check drops an
	// evicted-but-still-indexed chunk.
	backendFilter := model.Filter{}
	if fi, ok := idx.(model.FilteringIndex); ok && fi.CanFilter(filter) {
		backendFilter = filter
	}
	return s.collectFilteredCandidates(ctx, vector, idx, indexName, filters, backendFilter, k)
}

// collectFilteredCandidates runs the widening overfetch loop: it requests a
// widening candidate pool from the backend (with backendFilter pushed down when
// the backend supports it, or an empty filter otherwise) and applies
// matchFilters in Go until k results are gathered or the backend is exhausted.
func (s *Service) collectFilteredCandidates(
	ctx context.Context,
	vector []float32,
	idx model.Index,
	indexName string,
	filters model.SearchQuery,
	backendFilter model.Filter,
	k int,
) ([]model.SearchHit, error) {
	// Read overfetch under lock to avoid races with SetOverfetchMultiplier.
	// The multiplier is clamped to [1,100] at construction time so no further
	// defensive checks are needed here.
	s.metaMu.RLock()
	overfetchMultiplier := s.overfetchMultiplier
	s.metaMu.RUnlock()
	capHint := k
	if capHint > 1024 {
		capHint = 1024
	}
	filtered := make([]model.SearchHit, 0, capHint)
	seen := make(map[uint64]struct{}, capHint)
	for n := initialSearchFanout(k, overfetchMultiplier); len(filtered) < k; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hits, err := idx.Search(ctx, vector, n, backendFilter)
		if err != nil {
			return nil, err
		}
		s.collectFilteredHits(hits, indexName, filters, k, seen, &filtered)
		if searchExhausted(len(filtered), k, len(hits), n) {
			break
		}
		n = nextSearchFanout(n)
	}
	return filtered, nil
}

// searchHitFromIndexHit materialises a SearchHit from a backend IndexHit. It
// prefers the in-memory chunk metadata (searchHitForLabel) when present so
// existing fakes and the HNSW path keep their established behavior, and falls
// back to the backend-supplied payload for fields the in-memory metadata lacks
// (a non-default backend may be the only source of the rel_path/doc_type).
func (s *Service) searchHitFromIndexHit(indexName string, h model.IndexHit) model.SearchHit {
	hit := s.searchHitForLabel(indexName, h.ChunkID)
	// The in-memory chunk metadata is authoritative for the default HNSW path:
	// a missing entry there is the eviction/orphan signal (EvictDocuments
	// deletes the entry), so we must NOT resurrect such a chunk from the backend
	// payload. Only fall back to the payload when the service holds no in-memory
	// metadata at all — i.e. a non-default backend is the sole source of truth.
	if strings.TrimSpace(hit.RelPath) == "" && !s.hasInMemoryMetadata() {
		if payloadHit := h.Payload.ToSearchHit(); strings.TrimSpace(payloadHit.RelPath) != "" {
			hit = payloadHit
			hit.ChunkID = h.ChunkID
		}
	}
	hit.Score = float64(h.Score)
	return hit
}

// hasInMemoryMetadata reports whether any chunk metadata has been registered in
// the service (via SetChunkMetadata*). When true the in-memory maps are the
// authoritative source for materialising hits; when false (e.g. an external
// backend that never populated them) hits are materialised from the backend
// payload instead.
func (s *Service) hasInMemoryMetadata() bool {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	if len(s.chunkByLabel) > 0 {
		return true
	}
	for _, byIndex := range s.chunkByIndex {
		if len(byIndex) > 0 {
			return true
		}
	}
	return false
}

// collectFilteredHits walks one batch of index hits and appends matching hits to
// dst until k results are gathered. Hits already represented in `seen` are
// skipped to avoid duplicates across fanout iterations.
func (s *Service) collectFilteredHits(
	hits []model.IndexHit,
	indexName string,
	filters model.SearchQuery,
	k int,
	seen map[uint64]struct{},
	dst *[]model.SearchHit,
) {
	for _, h := range hits {
		if _, ok := seen[h.ChunkID]; ok {
			continue
		}
		seen[h.ChunkID] = struct{}{}
		hit := s.searchHitFromIndexHit(indexName, h)
		if !matchFilters(hit, filters) {
			continue
		}
		*dst = append(*dst, hit)
		if len(*dst) >= k {
			return
		}
	}
}

func initialSearchFanout(k, overfetchMultiplier int) int {
	// Protect multiplication k * overfetchMultiplier against overflow.
	if k > math.MaxInt/overfetchMultiplier {
		return math.MaxInt
	}
	return k * overfetchMultiplier
}

func nextSearchFanout(current int) int {
	if current >= math.MaxInt/2 {
		return math.MaxInt
	}
	return current * 2
}

func searchExhausted(filteredLen, k, labelsLen, n int) bool {
	return filteredLen >= k || labelsLen < n || n == math.MaxInt
}

func (s *Service) searchBothIndices(ctx context.Context, query string, k int, textModel, codeModel string, textIndex, codeIndex model.Index, filters model.SearchQuery) ([]model.SearchHit, error) {
	// each single-index call will apply the overfetch multiplier internally
	textHits, err := s.searchSingleIndex(ctx, query, k, textModel, textIndex, "text", filters, false)
	if err != nil {
		return nil, err
	}
	codeHits, err := s.searchSingleIndex(ctx, query, k, codeModel, codeIndex, "code", filters, false)
	if err != nil {
		return nil, err
	}

	normalizeScores(textHits)
	normalizeScores(codeHits)

	merged := make(map[uint64]model.SearchHit)
	for _, hit := range textHits {
		merged[hit.ChunkID] = hit
	}
	for _, hit := range codeHits {
		if existing, ok := merged[hit.ChunkID]; ok {
			if hit.Score > existing.Score {
				merged[hit.ChunkID] = hit
			}
			continue
		}
		merged[hit.ChunkID] = hit
	}

	out := make([]model.SearchHit, 0, len(merged))
	for _, hit := range merged {
		out = append(out, hit)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].ChunkID < out[j].ChunkID
		}
		return out[i].Score > out[j].Score
	})
	// index=both: rerank once on the merged, normalized pool (SPEC 9.1.1).
	return s.rerankPool(ctx, query, out, k), nil
}

func normalizeScores(hits []model.SearchHit) {
	if len(hits) == 0 {
		return
	}

	minScore := math.Inf(1)
	maxScore := math.Inf(-1)
	for _, hit := range hits {
		if hit.Score < minScore {
			minScore = hit.Score
		}
		if hit.Score > maxScore {
			maxScore = hit.Score
		}
	}
	if maxScore == minScore {
		for i := range hits {
			hits[i].Score = 1
		}
		return
	}

	denom := maxScore - minScore
	for i := range hits {
		hits[i].Score = (hits[i].Score - minScore) / denom
	}
}

func looksLikeCodeQuery(query string) bool {
	return LooksLikeCodeQuery(query)
}

// LooksLikeCodeQuery reports whether the query appears code-oriented.
func LooksLikeCodeQuery(query string) bool {
	q := strings.ToLower(query)

	// keyword pattern with word boundaries to avoid matching substrings.
	hasKw := codeKeywordRe.MatchString(q)
	// punctuation tokens commonly found in code
	hasPunct := codePunctRe.MatchString(q)
	// fenced code blocks or backticks
	hasFenced := strings.Contains(q, "```")
	hasBacktick := strings.Contains(q, "`")
	// file extension-like indicator – restrict to common code extensions and ensure a word boundary
	hasFileExt := fileExtensionRe.MatchString(q)

	// a strong signal: keyword + punctuation nearby
	if hasKw && hasPunct {
		return true
	}

	// otherwise count independent indicators
	indicators := 0
	if hasKw {
		indicators++
	}
	if hasPunct {
		indicators++
	}
	if hasFenced {
		indicators++
	}
	if hasBacktick {
		indicators++
	}
	if hasFileExt {
		indicators++
	}
	return indicators >= 2
}

func buildFallbackAnswer(question string, hits []model.SearchHit) string {
	if len(hits) == 0 {
		return "No relevant context found in the indexed corpus."
	}

	lines := make([]string, 0, len(hits)+1)
	lines = append(lines, fmt.Sprintf("Question: %s", question))
	lines = append(lines, "Top context:")
	limit := len(hits)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		h := hits[i]
		snippet := truncateSnippet(strings.TrimSpace(h.Snippet), 300)
		if snippet == "" {
			snippet = "(no snippet)"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", h.RelPath, snippet))
	}
	return strings.Join(lines, "\n")
}

func buildRAGPrompt(question string, hits []model.SearchHit, systemPrompt string, maxContextChars int) string {
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" {
		systemPrompt = defaultRAGSystemPrompt
	}
	if maxContextChars <= 0 {
		maxContextChars = defaultRAGMaxContext
	}
	if maxContextChars > maxRAGMaxContext {
		maxContextChars = maxRAGMaxContext
	}

	var b strings.Builder
	b.WriteString(systemPrompt)
	b.WriteString("\n\n")
	b.WriteString("Question:\n")
	b.WriteString(question)
	b.WriteString("\n\nContext:\n")

	remaining := maxContextChars
	limit := len(hits)
	if limit > 8 {
		limit = 8
	}
	for i := 0; i < limit && remaining > 0; i++ {
		h := hits[i]
		// Keep the bracketed [rel_path] tag stable for the answering model
		// (ensureAnswerAttributions relies on it for canonical citation
		// matching). When a human-readable Title is available, surface it
		// alongside the path as a parenthetical hint so the model has the
		// document name in addition to its path.
		line := "- [" + h.RelPath + "]"
		if title := strings.TrimSpace(h.Title); title != "" {
			line += " (" + title + ")"
		}
		line += " "
		snippet := truncateSnippet(strings.TrimSpace(h.Snippet), 300)
		switch {
		case snippet != "":
			// Available text (incl. an augment media hit's OCR/transcript)
			// grounds the answer normally.
		case isMediaHit(h):
			// A replace-mode media-only hit has no text: cite it without quoted
			// context rather than as a missing snippet (SPEC 8.1.7).
			snippet = "(" + strings.ToLower(strings.TrimSpace(h.Modality)) + " media; cited without quoted text)"
		default:
			snippet = "(no snippet)"
		}
		line += snippet + "\n"

		lineLen := len([]rune(line))
		if lineLen <= remaining {
			b.WriteString(line)
			remaining -= lineLen
			continue
		}

		truncated := truncateRunes(line, remaining)
		if strings.TrimSpace(truncated) != "" {
			b.WriteString(truncated)
		}
		remaining = 0
	}
	return b.String()
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-3]) + "..."
}

func truncateSnippet(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return strings.TrimSpace(string(r[:maxRunes])) + "..."
}

func ensureAnswerAttributions(answer string, citations []model.Citation) string {
	answer = strings.TrimSpace(answer)
	if answer == "" || len(citations) == 0 {
		return answer
	}

	type sourceEntry struct {
		rel   string
		title string
	}
	ordered := make([]sourceEntry, 0, len(citations))
	seen := make(map[string]struct{}, len(citations))
	for _, c := range citations {
		rel := strings.TrimSpace(c.RelPath)
		if rel == "" {
			continue
		}
		if _, ok := seen[rel]; ok {
			continue
		}
		seen[rel] = struct{}{}
		ordered = append(ordered, sourceEntry{rel: rel, title: strings.TrimSpace(c.Title)})
	}
	if len(ordered) == 0 {
		return answer
	}

	missing := make([]string, 0, len(ordered))
	for _, src := range ordered {
		// The canonical [rel_path] tag is what the model emits inline; we
		// only need to add a Sources line for paths it failed to mention.
		// When a human-readable title is present we surface it next to the
		// path so the appended block is more readable than bare paths.
		tag := "[" + src.rel + "]"
		if strings.Contains(answer, tag) {
			continue
		}
		display := tag
		if src.title != "" {
			display = tag + " (" + src.title + ")"
		}
		missing = append(missing, display)
	}
	if len(missing) == 0 {
		return answer
	}

	limit := len(missing)
	if limit > 5 {
		limit = 5
	}
	return answer + "\n\nSources: " + strings.Join(missing[:limit], ", ")
}

func matchFilters(hit model.SearchHit, query model.SearchQuery) bool {
	// Orphaned or evicted chunks have an empty RelPath and must never be
	// surfaced to callers regardless of any other filter criteria.
	if strings.TrimSpace(hit.RelPath) == "" {
		return false
	}

	if query.PathPrefix != "" && !strings.HasPrefix(hit.RelPath, query.PathPrefix) {
		return false
	}

	if query.FileGlob != "" {
		matched, err := path.Match(query.FileGlob, hit.RelPath)
		if err != nil || !matched {
			return false
		}
	}

	if len(query.DocTypes) > 0 {
		docTypeMatch := false
		for _, docType := range query.DocTypes {
			if strings.EqualFold(strings.TrimSpace(docType), strings.TrimSpace(hit.DocType)) {
				docTypeMatch = true
				break
			}
		}
		if !docTypeMatch {
			return false
		}
	}

	return true
}

func (s *Service) matchExcludePattern(pattern, relPath string) bool {
	pattern = strings.TrimSpace(filepath.ToSlash(pattern))
	relPath = strings.TrimSpace(filepath.ToSlash(relPath))
	if pattern == "" || relPath == "" {
		return false
	}

	// look up precompiled regexp
	s.metaMu.RLock()
	re := s.excludeRegexps[pattern]
	s.metaMu.RUnlock()
	if re == nil {
		// compile lazily in case cache was missed; store for future
		regex, err := regexp.Compile(globToRegexp(pattern))
		if err != nil {
			return false
		}
		// another goroutine may have stored the compiled regexp while we
		// were working; grab write lock and re-check before inserting.
		s.metaMu.Lock()
		if s.excludeRegexps == nil {
			s.excludeRegexps = make(map[string]*regexp.Regexp)
		}
		if existing := s.excludeRegexps[pattern]; existing != nil {
			// use the one already in cache instead of overwriting
			re = existing
		} else {
			s.excludeRegexps[pattern] = regex
			re = regex
		}
		s.metaMu.Unlock()
	}
	return re.MatchString(relPath)
}

// MatchExcludePattern reports whether relPath matches an exclude glob.
func (s *Service) MatchExcludePattern(pattern, relPath string) bool {
	return s.matchExcludePattern(pattern, relPath)
}

func globToRegexp(glob string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i += 2
				if i < len(glob) && glob[i] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			if strings.ContainsRune(`.+()|[]{}^$\`, rune(c)) {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		}
		i++
	}
	b.WriteString("$")
	return b.String()
}

func sliceLines(content string, start, end int) string {
	lines := strings.Split(content, "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 {
		end = start
	}
	if start > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	if end < start {
		end = start
	}
	return strings.Join(lines[start-1:end], "\n")
}

func truncateRunesWithFlag(s string, max int) (string, bool) {
	if max <= 0 {
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

func readFileBounded(path string, maxBytes int) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()

	if maxBytes <= 0 {
		data, readErr := io.ReadAll(f)
		return data, false, readErr
	}

	lim := io.LimitReader(f, int64(maxBytes))
	data, readErr := io.ReadAll(lim)
	if readErr != nil {
		return nil, false, readErr
	}
	return data, len(data) == maxBytes, nil
}

func (s *Service) sliceFromMetadata(relPath string, requested model.Span) (string, bool) {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()

	type candidate struct {
		start int
		page  int
		text  string
	}
	matches := make([]candidate, 0, 8)

	for _, hit := range s.chunkByLabel {
		if strings.TrimSpace(filepath.ToSlash(hit.RelPath)) != strings.TrimSpace(filepath.ToSlash(relPath)) {
			continue
		}
		if strings.TrimSpace(hit.Snippet) == "" {
			continue
		}
		span := hit.Span
		switch requested.Kind {
		case "page":
			if strings.EqualFold(span.Kind, "page") && span.Page == requested.Page {
				matches = append(matches, candidate{page: span.Page, text: hit.Snippet})
			}
		case "time":
			if !strings.EqualFold(span.Kind, "time") {
				continue
			}
			if overlapsTime(span.StartMS, span.EndMS, requested.StartMS, requested.EndMS) {
				matches = append(matches, candidate{start: span.StartMS, text: hit.Snippet})
			}
		}
	}

	if len(matches) == 0 {
		return "", false
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].page != matches[j].page {
			return matches[i].page < matches[j].page
		}
		return matches[i].start < matches[j].start
	})

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.text)
	}
	return strings.Join(out, "\n"), true
}

func overlapsTime(aStart, aEnd, bStart, bEnd int) bool {
	if aEnd <= 0 {
		aEnd = aStart
	}
	if bEnd <= 0 {
		bEnd = bStart
	}
	if aEnd < aStart {
		aEnd = aStart
	}
	if bEnd < bStart {
		bEnd = bStart
	}
	return aStart <= bEnd && bStart <= aEnd
}

func slicePage(content string, page int) (string, bool) {
	if page <= 0 {
		page = 1
	}
	parts := strings.Split(content, "\f")
	if len(parts) > 1 {
		if page > len(parts) {
			return "", false
		}
		return strings.Trim(parts[page-1], "\n"), true
	}
	if page == 1 {
		return content, true
	}
	return "", false
}

func sliceTime(content string, startMS, endMS int) (string, bool) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	foundTimestamp := false

	for _, line := range lines {
		m := timePrefixRe.FindStringSubmatch(line)
		if len(m) == 0 {
			continue
		}
		foundTimestamp = true
		tsMS := parseTimestampMS(m[1], m[2], m[3])
		if tsMS < startMS {
			continue
		}
		if endMS > 0 && tsMS > endMS {
			continue
		}
		out = append(out, line)
	}

	if !foundTimestamp {
		return "", false
	}
	if len(out) == 0 {
		return "", true
	}
	return strings.Join(out, "\n"), true
}

func parseTimestampMS(a, b, c string) int {
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	if c == "" {
		return (x*60 + y) * 1000
	}
	z, _ := strconv.Atoi(c)
	return (x*3600 + y*60 + z) * 1000
}
