package retrieval

import (
	"context"
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

	"dir2mcp/internal/model"
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
	`(?i)(?:aws(?:.{0,20})?secret|(?:secret|aws|token|key)\s*[:=]\s*[0-9a-zA-Z/+=]{40})`,

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
		rootDir:         ".",
		stateDir:        filepath.Join(".", ".dir2mcp"),
		protocolVersion: "2025-11-25",
		excludeRegexps:  make(map[string]*regexp.Regexp),
		pathExcludes:    append([]string(nil), defaultPathExcludes...),
		secretPatterns:  compiledPatterns,
	}
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
		k = 10
	}

	mode := strings.ToLower(strings.TrimSpace(query.Index))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "text":
		return s.searchSingleIndex(ctx, query.Query, k, textModel, textIndex, "text", query)
	case "code":
		return s.searchSingleIndex(ctx, query.Query, k, codeModel, codeIndex, "code", query)
	case "both":
		return s.searchBothIndices(ctx, query.Query, k, textModel, codeModel, textIndex, codeIndex, query)
	case "auto":
		if looksLikeCodeQuery(query.Query) {
			return s.searchSingleIndex(ctx, query.Query, k, codeModel, codeIndex, "code", query)
		}
		return s.searchSingleIndex(ctx, query.Query, k, textModel, textIndex, "text", query)
	default:
		return s.searchSingleIndex(ctx, query.Query, k, textModel, textIndex, "text", query)
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
		query.K = 10
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

func (s *Service) openFile(ctx context.Context, relPath string, span model.Span, maxChars int) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	relPath = strings.TrimSpace(relPath)
	if relPath == "" {
		return "", false, model.ErrForbidden
	}

	if maxChars <= 0 {
		maxChars = 20000
	}
	if maxChars > 50000 {
		maxChars = 50000
	}

	s.metaMu.RLock()
	rootDir := s.rootDir
	pathExcludes := append([]string(nil), s.pathExcludes...)
	secretPatterns := append([]*regexp.Regexp(nil), s.secretPatterns...)
	s.metaMu.RUnlock()
	if strings.TrimSpace(rootDir) == "" {
		rootDir = "."
	}

	normalizedRel := filepath.ToSlash(filepath.Clean(relPath))
	if normalizedRel == "." || strings.HasPrefix(normalizedRel, "../") || normalizedRel == ".." || filepath.IsAbs(relPath) {
		return "", false, model.ErrPathOutsideRoot
	}
	for _, pattern := range pathExcludes {
		if s.matchExcludePattern(pattern, normalizedRel) {
			return "", false, model.ErrForbidden
		}
	}

	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return "", false, err
	}
	realRoot := rootAbs
	if resolvedRoot, rootErr := filepath.EvalSymlinks(rootAbs); rootErr == nil {
		realRoot = resolvedRoot
	}

	targetAbs := filepath.Join(realRoot, filepath.FromSlash(normalizedRel))
	relFromRoot, err := filepath.Rel(realRoot, targetAbs)
	if err != nil || relFromRoot == ".." || strings.HasPrefix(relFromRoot, ".."+string(os.PathSeparator)) {
		return "", false, model.ErrPathOutsideRoot
	}

	kind := strings.ToLower(strings.TrimSpace(span.Kind))
	if kind == "page" || kind == "time" {
		if fromMeta, ok := s.sliceFromMetadata(normalizedRel, span); ok {
			for _, re := range secretPatterns {
				if re != nil && re.MatchString(fromMeta) {
					return "", false, model.ErrForbidden
				}
			}
			out, truncated := truncateRunesWithFlag(fromMeta, maxChars)
			return out, truncated, nil
		}
	}

	resolvedAbs, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
		// if eval fails for other reasons, continue with direct target path check
		resolvedAbs = targetAbs
	}
	resolvedRel, err := filepath.Rel(realRoot, resolvedAbs)
	if err != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(os.PathSeparator)) {
		return "", false, model.ErrPathOutsideRoot
	}
	resolvedRel = filepath.ToSlash(filepath.Clean(resolvedRel))
	for _, pattern := range pathExcludes {
		if s.matchExcludePattern(pattern, resolvedRel) {
			return "", false, model.ErrForbidden
		}
	}

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

	for _, re := range secretPatterns {
		if re != nil && re.MatchString(content) {
			return "", false, model.ErrForbidden
		}
	}

	selected := content
	switch kind {
	case "", "lines":
		if kind == "lines" || span.StartLine > 0 || span.EndLine > 0 {
			selected = sliceLines(content, span.StartLine, span.EndLine)
		}
	case "page":
		page := span.Page
		if page <= 0 {
			page = 1
		}
		// metadata-backed OCR handled above; fall back to slicing pages directly
		paged, ok := slicePage(content, page)
		if !ok {
			return "", false, model.ErrDocTypeUnsupported
		}
		selected = paged
	case "time":
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
			return "", false, model.ErrDocTypeUnsupported
		}
		selected = timeSlice
	default:
		return "", false, model.ErrDocTypeUnsupported
	}

	out, outTruncated := truncateRunesWithFlag(selected, maxChars)
	return out, readTruncated || outTruncated, nil
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

// ErrMissingEmbedder is returned when the service was created without
// a configured embedder and a search attempt is made. This prevents a
// nil-pointer panic in searchSingleIndex while giving callers a clear
// failure reason.
var ErrMissingEmbedder = errors.New("embedder not configured")

func (s *Service) searchSingleIndex(ctx context.Context, query string, k int, modelName string, idx model.Index, indexName string, filters model.SearchQuery) ([]model.SearchHit, error) {
	if s.embedder == nil {
		// caller should have provided an embedder via NewService or
		// SetEmbedder (not currently available).  Return an explicit
		// error rather than letting the nil dereference panic later.
		return nil, ErrMissingEmbedder
	}
	vectors, err := s.embedder.Embed(ctx, modelName, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return []model.SearchHit{}, nil
	}
	if idx == nil {
		return []model.SearchHit{}, nil
	}

	// compute number of neighbors to request from index according to the
	// current overfetch multiplier. Read under lock to avoid races with
	// SetOverfetchMultiplier. The multiplier is initialized to 5 in the
	// constructor and SetOverfetchMultiplier already clamps values to the
	// [1,100] range, so it is guaranteed to be at least 1 and no further
	// defensive adjustment is necessary.
	s.metaMu.RLock()
	overfetchMultiplier := s.overfetchMultiplier
	s.metaMu.RUnlock()
	// protect multiplication k * overfetchMultiplier against overflow
	// by checking against MaxInt. If the caller supplied a huge k value we
	// simply clamp the request size rather than allow wraparound.  An
	// alternative would be to return an error; at present callers only ever
	// pass reasonably small k's so clamping is acceptable.
	var n int
	if k > math.MaxInt/overfetchMultiplier {
		// avoid overflow and also prevent asking the index for more
		// neighbors than an int can represent; this keeps downstream code
		// consistent (e.g. fakeIndex in tests) and mirrors the behavior of
		// capping the multiplier itself via SetOverfetchMultiplier.
		n = math.MaxInt
	} else {
		n = k * overfetchMultiplier
	}
	labels, scores, err := idx.Search(vectors[0], n)
	if err != nil {
		return nil, err
	}

	// avoid trying to preallocate a gigantic slice when k is absurdly large
	cap := k
	if cap > len(labels) {
		cap = len(labels)
	}
	filtered := make([]model.SearchHit, 0, cap)
	for i, label := range labels {
		hit := s.searchHitForLabel(indexName, label)
		hit.Score = float64(scores[i])
		if !matchFilters(hit, filters) {
			continue
		}
		filtered = append(filtered, hit)
		if len(filtered) >= k {
			break
		}
	}
	return filtered, nil
}

func (s *Service) searchBothIndices(ctx context.Context, query string, k int, textModel, codeModel string, textIndex, codeIndex model.Index, filters model.SearchQuery) ([]model.SearchHit, error) {
	// each single-index call will apply the overfetch multiplier internally
	textHits, err := s.searchSingleIndex(ctx, query, k, textModel, textIndex, "text", filters)
	if err != nil {
		return nil, err
	}
	codeHits, err := s.searchSingleIndex(ctx, query, k, codeModel, codeIndex, "code", filters)
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
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
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
		line := "- [" + h.RelPath + "] "
		snippet := truncateSnippet(strings.TrimSpace(h.Snippet), 300)
		if snippet == "" {
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

	orderedSources := make([]string, 0, len(citations))
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
		orderedSources = append(orderedSources, rel)
	}
	if len(orderedSources) == 0 {
		return answer
	}

	missing := make([]string, 0, len(orderedSources))
	for _, rel := range orderedSources {
		tag := "[" + rel + "]"
		if !strings.Contains(answer, tag) {
			missing = append(missing, tag)
		}
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
