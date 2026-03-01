package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dir2mcp/internal/appstate"
	"dir2mcp/internal/config"
	"dir2mcp/internal/elevenlabs"
	"dir2mcp/internal/mistral"
	"dir2mcp/internal/model"
)

// annotationChunk* constants mirror the hardcoded parameters previously
// used when splitting annotation JSON/text into segments for indexing. They
// centralize tuning values and improve readability of call sites in this
// file. The defaults were 1200, 200 and 120 respectively.
const (
	annotationChunkSize    = 1200
	annotationChunkOverlap = 200
	annotationChunkMinSize = 120

	transcriberProviderAuto       = "auto"
	transcriberProviderMistral    = "mistral"
	transcriberProviderElevenLabs = "elevenlabs"
	transcriberProviderOff        = "off"
)

type Service struct {
	cfg           config.Config
	store         model.Store
	indexingState *appstate.IndexingState
	repGen        *RepresentationGenerator
	ocr           model.OCR
	transcriber   model.Transcriber

	// optional logger for diagnostics; defaults to log.Default() when nil.
	// Tests can provide their own logger to avoid mutating global state.
	// Access must go through the logger() helper or SetLogger; the field
	// itself is private and guarded by loggerMu.
	logger *log.Logger
	// protects reads/writes of logger when set during runtime.
	loggerMu sync.RWMutex

	// optional cache policy for OCR results. maxBytes bounds the total
	// bytes of files kept in the on‑disk cache; zero disables size pruning.
	// ttl, if non‑zero, causes files older than the duration to be removed.
	ocrCacheMaxBytes int64
	ocrCacheTTL      time.Duration

	// optional hook used primarily by tests. if non‑nil the function is used
	// in place of DirEntry.Info() when scanning the cache. this allows the
	// tests to simulate stat errors without fiddling with the real filesystem.
	ocrCacheStat func(os.DirEntry) (os.FileInfo, error)

	// hook invoked instead of enforceOCRCachePolicy; useful for tests that
	// want to simulate a failure without touching the filesystem. nil means
	// use the normal method.
	//
	// Despite the OCR-prefixed names, these cache controls are intentionally
	// shared by OCR and transcript caches. Transcript writes call the same
	// write counter/hook so both cache trees follow one policy and cadence.
	ocrCacheEnforce func(string) error

	// mutex protecting all of the OCR cache configuration fields and the
	// related bookkeeping state.  In particular it guards access to
	// ocrCacheMaxBytes, ocrCacheTTL (and the associated hooks
	// ocrCacheStat/ocrCacheEnforce), as well as the write counter
	// ocrCacheWrites and the pruning interval ocrCachePruneEvery.  The cache
	// enforcement routine (enforceOCRCachePolicy or a test hook) may run
	// concurrently with calls to SetOCRCacheLimits/SetOCRCachePruneEvery, so
	// readers and writers of those shared fields must hold the lock.
	ocrCacheMu sync.RWMutex

	// enforcement bookkeeping. Instead of scanning the cache on every write we
	// maintain a simple counter of cache writes and only run
	// enforceOCRCachePolicy() once every ocrCachePruneEvery writes. A value of
	// zero is treated as "run every time" to preserve existing behaviour and is
	// convenient for tests.
	ocrCacheWrites     int
	ocrCachePruneEvery int
}

// ErrTranscriptProviderFailure marks failures originating from the transcript
// provider call itself (as opposed to persistence/cache write failures).
var ErrTranscriptProviderFailure = errors.New("transcript provider failure")

type documentDeleteMarker interface {
	MarkDocumentDeleted(ctx context.Context, relPath string) error
}

func NewService(cfg config.Config, store model.Store) *Service {
	svc := &Service{
		cfg:    cfg,
		store:  store,
		logger: log.Default(),
	}
	if transcriber, err := TranscriberFromConfig(cfg); err == nil {
		svc.transcriber = transcriber
	} else {
		svc.getLogger().Printf("transcriber config skipped: %v", err)
	}
	if rs, ok := store.(model.RepresentationStore); ok {
		svc.repGen = NewRepresentationGenerator(rs)
	}
	return svc
}

// DiscoverOptionsFromConfig resolves ingest discovery behavior from config.
// Defaults remain strict: no .gitignore support and no symlink following.
func DiscoverOptionsFromConfig(cfg config.Config) DiscoverOptions {
	options := DefaultDiscoverOptions()
	options.UseGitIgnore = cfg.IngestGitignore
	options.FollowSymlinks = cfg.IngestFollowSymlinks
	if cfg.IngestMaxFileMB > 0 {
		options.MaxSizeBytes = int64(cfg.IngestMaxFileMB) * 1024 * 1024
	}
	return options
}

// TranscriberFromConfig resolves the configured STT provider instance.
func TranscriberFromConfig(cfg config.Config) (model.Transcriber, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.STTProvider))
	if provider == "" {
		provider = transcriberProviderAuto
	}

	switch provider {
	case transcriberProviderOff, "none", "disabled":
		return nil, nil
	case transcriberProviderMistral:
		if strings.TrimSpace(cfg.MistralAPIKey) == "" {
			return nil, nil
		}
		client := mistral.NewClient(cfg.MistralBaseURL, cfg.MistralAPIKey)
		if modelName := strings.TrimSpace(cfg.STTMistralModel); modelName != "" {
			client.DefaultTranscribeModel = modelName
		}
		return client, nil
	case transcriberProviderAuto:
		if strings.TrimSpace(cfg.MistralAPIKey) != "" {
			client := mistral.NewClient(cfg.MistralBaseURL, cfg.MistralAPIKey)
			if modelName := strings.TrimSpace(cfg.STTMistralModel); modelName != "" {
				client.DefaultTranscribeModel = modelName
			}
			return client, nil
		}
		if strings.TrimSpace(cfg.ElevenLabsAPIKey) == "" {
			return nil, nil
		}
		provider = transcriberProviderElevenLabs
	case transcriberProviderElevenLabs:
		// handled below
	default:
		return nil, fmt.Errorf("unsupported transcriber provider %q", provider)
	}

	if provider != transcriberProviderElevenLabs {
		return nil, nil
	}

	client := elevenlabs.NewClient(cfg.ElevenLabsAPIKey, cfg.ElevenLabsTTSVoiceID)
	if baseURL := strings.TrimSpace(cfg.ElevenLabsBaseURL); baseURL != "" {
		client.BaseURL = strings.TrimRight(baseURL, "/")
	}
	if modelName := strings.TrimSpace(cfg.STTElevenLabsModel); modelName != "" {
		client.TranscribeModel = modelName
	}
	if languageCode := strings.TrimSpace(cfg.STTElevenLabsLanguageCode); languageCode != "" {
		client.TranscribeLanguageCode = languageCode
	}
	return client, nil
}

// healthCheckInterval returns the configured base poll interval for connector
// health probes. It mirrors the behaviour described in VISION.md: when the
// configuration value is zero (or the receiver is nil) the default from
// config.Default().HealthCheckInterval is returned. Actual polling routines
// should call this method to obtain a duration rather than hardcoding any fixed
// interval.
func (s *Service) healthCheckInterval() time.Duration {
	if s == nil {
		return config.Default().HealthCheckInterval
	}
	if s.cfg.HealthCheckInterval > 0 {
		return s.cfg.HealthCheckInterval
	}
	return config.Default().HealthCheckInterval
}

// SetLogger sets a custom logger on the service. Passing nil restores the
// default logger.
func (s *Service) SetLogger(l *log.Logger) {
	s.loggerMu.Lock()
	defer s.loggerMu.Unlock()
	s.logger = l
}

// getLogger returns the active logger, defaulting to the package global if
// none has been set. The name avoids colliding with the private field.
func (s *Service) getLogger() *log.Logger {
	if s == nil {
		return log.Default()
	}
	s.loggerMu.RLock()
	defer s.loggerMu.RUnlock()
	if s.logger == nil {
		return log.Default()
	}
	return s.logger
}

func (s *Service) SetIndexingState(state *appstate.IndexingState) {
	s.indexingState = state
}

func (s *Service) SetOCR(ocr model.OCR) {
	s.ocr = ocr
}

func (s *Service) SetTranscriber(transcriber model.Transcriber) {
	s.transcriber = transcriber
}

// ProcessDocument exposes single-document processing for external tests.
func (s *Service) ProcessDocument(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool) error {
	return s.processDocument(ctx, f, secretPatterns, forceReindex, nil)
}

// SetOCRCacheLimits configures in‑memory limits that the service will enforce
// when writing to the OCR cache. A maxBytes value of zero disables size
// pruning; a ttl value of zero disables age‑based pruning. Both limits can be
// applied simultaneously. These are primarily useful for tests or for
// embedding the service in environments where disk usage must be bounded.
func (s *Service) SetOCRCacheLimits(maxBytes int64, ttl time.Duration) {
	s.ocrCacheMu.Lock()
	defer s.ocrCacheMu.Unlock()
	s.ocrCacheMaxBytes = maxBytes
	s.ocrCacheTTL = ttl
}

// SetOCRCachePruneEvery configures how often the cache policy is enforced on
// writes. The service counts writes and only runs the full scan when the
// counter reaches this value. A value of zero (the default) means "run every
// time", which preserves the original behaviour and makes tests simpler.
func (s *Service) SetOCRCachePruneEvery(n int) {
	s.ocrCacheMu.Lock()
	defer s.ocrCacheMu.Unlock()
	s.ocrCachePruneEvery = n
}

// SetOCRCacheStatHook sets a stat hook for cache enforcement.
func (s *Service) SetOCRCacheStatHook(fn func(os.DirEntry) (os.FileInfo, error)) {
	s.ocrCacheMu.Lock()
	defer s.ocrCacheMu.Unlock()
	s.ocrCacheStat = fn
}

// SetOCRCacheEnforceHook sets a cache policy enforcement hook.
func (s *Service) SetOCRCacheEnforceHook(fn func(string) error) {
	s.ocrCacheMu.Lock()
	defer s.ocrCacheMu.Unlock()
	s.ocrCacheEnforce = fn
}

// markOCRCacheWrite increments the shared cache write counter (used by both
// OCR and transcript caches) and reports whether policy enforcement should run
// for this write. When enforcement is due, the counter is reset so the next N
// writes are free of scans.
func (s *Service) markOCRCacheWrite() bool {
	s.ocrCacheMu.Lock()
	defer s.ocrCacheMu.Unlock()
	s.ocrCacheWrites++
	if s.ocrCachePruneEvery <= 0 || s.ocrCacheWrites >= s.ocrCachePruneEvery {
		s.ocrCacheWrites = 0
		return true
	}
	return false
}

// ClearOCRCache deletes any cached OCR data.  The caller may use this to
// forcibly reset state (e.g. during tests).
func (s *Service) ClearOCRCache() error {
	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "ocr")
	return os.RemoveAll(cacheDir)
}

func (s *Service) Run(ctx context.Context) error {
	if s.indexingState != nil {
		s.indexingState.SetMode(appstate.ModeIncremental)
		s.indexingState.SetRunning(true)
		defer s.indexingState.SetRunning(false)
	}
	return s.runScan(ctx)
}

func (s *Service) Reindex(ctx context.Context) error {
	if s.indexingState != nil {
		s.indexingState.SetMode(appstate.ModeFull)
		s.indexingState.SetRunning(true)
		defer s.indexingState.SetRunning(false)
	}
	return s.runScan(ctx)
}

func (s *Service) runScan(ctx context.Context) error {
	if s.store == nil {
		return errors.New("ingest store is not configured")
	}

	discovered, err := DiscoverFilesWithOptions(ctx, s.cfg.RootDir, DiscoverOptionsFromConfig(s.cfg))
	if err != nil {
		return err
	}

	compiledSecrets, err := compileSecretPatterns(s.cfg.SecretPatterns)
	if err != nil {
		return err
	}

	existing, err := s.listActiveDocuments(ctx)
	if err != nil {
		return err
	}

	forceReindex := s.indexingState != nil && s.indexingState.Snapshot().Mode == appstate.ModeFull

	seen := make(map[string]struct{}, len(discovered))
	for _, f := range discovered {
		if err := ctx.Err(); err != nil {
			return err
		}

		s.addScanned(1)
		if matchesAnyPathExclude(f.RelPath, s.cfg.PathExcludes) {
			s.addSkipped(1)
			continue
		}

		if err := s.processDocument(ctx, f, compiledSecrets, forceReindex, seen); err != nil {
			s.addErrors(1)
			// record that we saw the file even if processing failed so
			// markMissingAsDeleted does not treat it as removed
			seen[f.RelPath] = struct{}{}
			continue
		}
		seen[f.RelPath] = struct{}{}
	}

	return s.markMissingAsDeleted(ctx, existing, seen)
}

func (s *Service) processDocument(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}) error {
	doc, content, buildErr := s.buildDocumentWithContent(f, secretPatterns)
	if buildErr != nil {
		doc = model.Document{
			RelPath:   f.RelPath,
			DocType:   ClassifyDocType(f.RelPath),
			SizeBytes: f.SizeBytes,
			MTimeUnix: f.MTimeUnix,
			Status:    "error",
			Deleted:   false,
		}
		if err := s.store.UpsertDocument(ctx, doc); err != nil {
			return fmt.Errorf("upsert error document: %w", err)
		}
		// s.addErrors(1) is intentionally omitted here; runScan already
		// increments the error counter for any non-nil return value.
		return buildErr
	}

	existingDoc, err := s.store.GetDocumentByPath(ctx, doc.RelPath)
	if err != nil && !isNotFoundError(err) {
		return fmt.Errorf("get existing document: %w", err)
	}

	needsProcessing := needsReprocessing(existingDoc.ContentHash, doc.ContentHash, forceReindex)
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}

	// after upsert we need the persisted DocID for downstream
	// representation creation.  The store implementation assigns the ID,
	// but UpsertDocument only returns an error, so query the record again
	// and update the local copy.  We ignore not-found errors because that
	// would be surprising immediately after a successful upsert and is
	// already handled by the store implementation.
	if updated, err := s.store.GetDocumentByPath(ctx, doc.RelPath); err == nil {
		doc.DocID = updated.DocID
	} else if !isNotFoundError(err) {
		return fmt.Errorf("fetch document after upsert: %w", err)
	}

	switch doc.Status {
	case "ok":
		s.addIndexed(1)
	case "skipped", "secret_excluded":
		s.addSkipped(1)
	case "error":
		// although buildDocumentWithContent will never return a document with
		// Status="error" (the error case returns early above), we leave this
		// branch in place as a defensive measure. future changes to document
		// construction might introduce new terminal statuses and it's nicer
		// to handle them explicitly here rather than silently falling through.
		s.addErrors(1)
		return nil
	}

	// Archive containers: extract and ingest each member as its own document.
	// The archive document itself remains "skipped" (no direct text content).
	if doc.DocType == "archive" {
		if needsProcessing {
			if err := s.processArchiveMembers(ctx, f, secretPatterns, forceReindex, seen); err != nil {
				return fmt.Errorf("process archive members: %w", err)
			}
		} else if seen != nil {
			// Archive content unchanged: retain existing members in seen.
			s.retainArchiveMembers(ctx, f.RelPath, seen)
		}
		return nil
	}

	if !needsProcessing || doc.Status != "ok" {
		return nil
	}

	if err := s.generateRepresentations(ctx, doc, content); err != nil {
		return fmt.Errorf("generate representations: %w", err)
	}
	return nil
}

// processArchiveMembers extracts members from an archive and ingests each one
// as an independent document. One bad member is logged and skipped without
// aborting the rest.
func (s *Service) processArchiveMembers(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}) error {
	members, err := extractArchiveMembers(f.AbsPath, f.RelPath)
	if err != nil {
		s.getLogger().Printf("archive extract %s: %v", f.RelPath, err)
		return nil // extraction failure is non-fatal; archive stays "skipped"
	}
	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.processDocumentFromContent(ctx, m.RelPath, m.Content, f.MTimeUnix, secretPatterns, forceReindex); err != nil {
			s.getLogger().Printf("archive member %s: %v", m.RelPath, err)
			// continue with next member
		}
		if seen != nil {
			seen[m.RelPath] = struct{}{}
		}
	}
	return nil
}

// retainArchiveMembers adds all existing members of an unchanged archive to
// the seen map so that markMissingAsDeleted does not tombstone them.
func (s *Service) retainArchiveMembers(ctx context.Context, archiveRelPath string, seen map[string]struct{}) {
	prefix := archiveRelPath + "/"
	const pageSize = 500
	offset := 0
	for {
		docs, total, err := s.store.ListFiles(ctx, prefix, "", pageSize, offset)
		if err != nil {
			s.getLogger().Printf("retainArchiveMembers(%s): %v", archiveRelPath, err)
			return
		}
		for _, doc := range docs {
			seen[doc.RelPath] = struct{}{}
		}
		pageLen := len(docs)
		offset += pageLen
		if pageLen == 0 {
			break
		}
		if pageLen < pageSize {
			if int64(offset) < total {
				s.getLogger().Printf(
					"retainArchiveMembers(%s): pagination inconsistency (offset=%d page=%d total=%d); stopping on short page",
					archiveRelPath,
					offset-pageLen,
					pageLen,
					total,
				)
			}
			break
		}
		if int64(offset) >= total {
			break
		}
	}
}

// processDocumentFromContent ingests a document whose content is already in
// memory (e.g. an archive member). relPath is the virtual path stored in the
// documents table; mtimeUnix is inherited from the parent archive.
func (s *Service) processDocumentFromContent(ctx context.Context, relPath string, content []byte, mtimeUnix int64, secretPatterns []*regexp.Regexp, forceReindex bool) error {
	docType := ClassifyDocType(relPath)
	// Never ingest binary or ignored artifacts from inside archives.
	if docType == "binary_ignored" || docType == "ignore" {
		return nil
	}
	// Nested archive files are persisted as skipped document rows, but are not
	// recursively extracted.
	skipExtraction := docType == "archive"

	doc := model.Document{
		RelPath:     relPath,
		DocType:     docType,
		SourceType:  "archive_member",
		SizeBytes:   int64(len(content)),
		MTimeUnix:   mtimeUnix,
		ContentHash: computeContentHash(content),
		Status:      "ok",
	}
	if skipExtraction {
		doc.Status = "skipped"
	}

	if !skipExtraction && hasSecretMatch(contentSample(content), secretPatterns) {
		doc.Status = "secret_excluded"
	}

	existingDoc, err := s.store.GetDocumentByPath(ctx, relPath)
	if err != nil && !isNotFoundError(err) {
		return fmt.Errorf("get existing document: %w", err)
	}
	needsProcessing := needsReprocessing(existingDoc.ContentHash, doc.ContentHash, forceReindex)

	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}
	if updated, err := s.store.GetDocumentByPath(ctx, relPath); err == nil {
		doc.DocID = updated.DocID
	} else if !isNotFoundError(err) {
		return fmt.Errorf("fetch document after upsert: %w", err)
	}

	if !needsProcessing || doc.Status != "ok" {
		return nil
	}
	if err := s.generateRepresentations(ctx, doc, content); err != nil {
		return fmt.Errorf("generate representations: %w", err)
	}
	return nil
}

func (s *Service) buildDocumentWithContent(f DiscoveredFile, secretPatterns []*regexp.Regexp) (model.Document, []byte, error) {
	docType := ClassifyDocType(f.RelPath)
	doc := model.Document{
		RelPath:   f.RelPath,
		DocType:   docType,
		SizeBytes: f.SizeBytes,
		MTimeUnix: f.MTimeUnix,
		Status:    "ok",
		Deleted:   false,
	}

	content, err := os.ReadFile(f.AbsPath)
	if err != nil {
		return doc, nil, fmt.Errorf("read %s: %w", f.RelPath, err)
	}
	doc.ContentHash = computeContentHash(content)

	// certain document types we don't want to ingest at all.
	// "archive" and "binary_ignored" were already skipped.
	// newly, the "ignore" category (used for sensitive files like
	// .env variants) is also treated as skipped so that they never
	// enter the pipeline.
	if docType == "archive" || docType == "binary_ignored" || docType == "ignore" {
		doc.Status = "skipped"
		return doc, content, nil
	}

	if hasSecretMatch(contentSample(content), secretPatterns) {
		doc.Status = "secret_excluded"
	}

	return doc, content, nil
}

func contentSample(content []byte) []byte {
	if int64(len(content)) <= secretScanSampleBytes {
		return content
	}
	return content[:secretScanSampleBytes]
}

func (s *Service) listActiveDocuments(ctx context.Context) (map[string]struct{}, error) {
	active := make(map[string]struct{})
	const pageSize = 500

	offset := 0
	for {
		docs, total, err := s.store.ListFiles(ctx, "", "", pageSize, offset)
		if err != nil {
			if errors.Is(err, model.ErrNotImplemented) {
				return active, nil
			}
			return nil, err
		}
		for _, doc := range docs {
			if doc.Deleted {
				continue
			}
			active[doc.RelPath] = struct{}{}
		}

		offset += len(docs)
		if len(docs) == 0 || int64(offset) >= total {
			break
		}
	}
	return active, nil
}

func (s *Service) markMissingAsDeleted(ctx context.Context, existing, seen map[string]struct{}) error {
	deleter, ok := s.store.(documentDeleteMarker)
	if !ok {
		return nil
	}

	paths := make([]string, 0, len(existing))
	for relPath := range existing {
		if _, found := seen[relPath]; found {
			continue
		}
		paths = append(paths, relPath)
	}
	sort.Strings(paths)

	for _, relPath := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := deleter.MarkDocumentDeleted(ctx, relPath); err != nil {
			s.addErrors(1)
			continue
		}
		s.addDeleted(1)
	}
	return nil
}

func (s *Service) addScanned(delta int64) {
	if s.indexingState != nil {
		s.indexingState.AddScanned(delta)
	}
}

func (s *Service) addIndexed(delta int64) {
	if s.indexingState != nil {
		s.indexingState.AddIndexed(delta)
	}
}

func (s *Service) addSkipped(delta int64) {
	if s.indexingState != nil {
		s.indexingState.AddSkipped(delta)
	}
}

func (s *Service) addDeleted(delta int64) {
	if s.indexingState != nil {
		s.indexingState.AddDeleted(delta)
	}
}

func (s *Service) addErrors(delta int64) {
	if s.indexingState != nil {
		s.indexingState.AddErrors(delta)
	}
}

func (s *Service) addRepresentations(delta int64) {
	if s.indexingState != nil {
		s.indexingState.AddRepresentations(delta)
	}
}

func (s *Service) generateRepresentations(ctx context.Context, doc model.Document, content []byte) error {
	if s.repGen == nil {
		return nil
	}

	if ShouldGenerateRawText(doc.DocType) {
		// we already loaded the file contents earlier in processDocument,
		// avoid re-reading it by using the new helper method.
		if err := s.repGen.GenerateRawTextFromContent(ctx, doc, content); err != nil {
			return err
		}
		s.addRepresentations(1)
		return nil
	}

	if (doc.DocType == "pdf" || doc.DocType == "image") && s.ocr != nil {
		if err := s.generateOCRMarkdownRepresentation(ctx, doc, content); err != nil {
			return err
		}
		s.addRepresentations(1)
	}
	if doc.DocType == "audio" && s.transcriber != nil {
		if err := s.generateTranscriptRepresentation(ctx, doc, content); err != nil {
			// Provider/transient failures should not fail the entire ingest run.
			// Persistence/cache failures should still propagate.
			if errors.Is(err, ErrTranscriptProviderFailure) {
				s.getLogger().Printf("transcription skipped for %s: %v", doc.RelPath, err)
				s.addErrors(1)
				return nil
			}
			return err
		}
		s.addRepresentations(1)
	}
	return nil
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	// common filesystem sentinel
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	// sqlite/sql driver returns sql.ErrNoRows for missing rows
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	// some store implementations may define their own sentinel error
	// for a missing row/document/representation.  Add a clause here to
	// avoid treating those as fatal.
	if errors.Is(err, model.ErrNotFound) {
		return true
	}
	return false
}

func (s *Service) generateOCRMarkdownRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	if s.repGen == nil || s.ocr == nil {
		return nil
	}

	ocrText, err := s.readOrComputeOCR(ctx, doc, content)
	if err != nil {
		return err
	}

	ocrText = strings.TrimSpace(ocrText)
	if ocrText == "" {
		return nil
	}

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeOCRMarkdown,
		RepHash:     computeRepHash([]byte(ocrText)),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert ocr representation: %w", err)
	}

	segments := chunkOCRByPages(ocrText)
	if len(segments) == 0 {
		return nil
	}
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments); err != nil {
		return fmt.Errorf("persist ocr chunks: %w", err)
	}
	return nil
}

// GenerateOCRMarkdownRepresentation exposes OCR representation generation for tests.
func (s *Service) GenerateOCRMarkdownRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	return s.generateOCRMarkdownRepresentation(ctx, doc, content)
}

func (s *Service) generateTranscriptRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	if s.repGen == nil || s.transcriber == nil {
		return nil
	}

	transcriptText, err := s.readOrComputeTranscript(ctx, doc, content)
	if err != nil {
		return err
	}

	transcriptText = strings.TrimSpace(transcriptText)
	if transcriptText == "" {
		return nil
	}

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeTranscript,
		RepHash:     computeRepHash([]byte(transcriptText)),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert transcript representation: %w", err)
	}

	segments := chunkTranscriptByTime(transcriptText)
	if len(segments) == 0 {
		return nil
	}
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments); err != nil {
		return fmt.Errorf("persist transcript chunks: %w", err)
	}
	return nil
}

// GenerateTranscriptRepresentation exposes transcript generation for tests.
func (s *Service) GenerateTranscriptRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	return s.generateTranscriptRepresentation(ctx, doc, content)
}

// StoreAnnotationRepresentations persists a structured annotation JSON payload
// for a document and optionally stores a flattened text representation to make
// annotation fields retrievable through semantic search.
func (s *Service) StoreAnnotationRepresentations(ctx context.Context, doc model.Document, annotation map[string]interface{}, indexFlattenedText bool) (string, error) {
	if s.repGen == nil {
		return "", errors.New("representation generator not configured")
	}
	if doc.DocID <= 0 {
		return "", errors.New("document id is required")
	}
	if annotation == nil {
		return "", errors.New("annotation json is required")
	}

	jsonBytes, err := json.Marshal(annotation)
	if err != nil {
		return "", fmt.Errorf("marshal annotation json: %w", err)
	}
	jsonText := string(jsonBytes)

	flattened := s.flattenJSONForIndexing(annotation)
	trimmed := strings.TrimSpace(flattened)
	preview := flattened
	if runes := []rune(preview); len(runes) > 240 {
		preview = string(runes[:240]) + "..."
	}

	err = s.repGen.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		jsonRep := model.Representation{
			DocID:       doc.DocID,
			RepType:     RepTypeAnnotationJSON,
			RepHash:     computeRepHash(jsonBytes),
			CreatedUnix: time.Now().Unix(),
			Deleted:     false,
		}
		jsonRepID, upsertErr := tx.UpsertRepresentation(ctx, jsonRep)
		if upsertErr != nil {
			return fmt.Errorf("upsert annotation json representation: %w", upsertErr)
		}
		if upsertErr := s.repGen.upsertChunksForRepresentationWithStore(ctx, tx, jsonRepID, "text", chunkTextByChars(jsonText, annotationChunkSize, annotationChunkOverlap, annotationChunkMinSize)); upsertErr != nil {
			return fmt.Errorf("persist annotation json chunks: %w", upsertErr)
		}

		if !indexFlattenedText {
			return nil
		}

		// don't index an empty or whitespace-only flattened string; it only
		// creates useless representations/chunks.
		if trimmed == "" {
			return nil
		}

		textRep := model.Representation{
			DocID:       doc.DocID,
			RepType:     RepTypeAnnotationText,
			RepHash:     computeRepHash([]byte(flattened)),
			CreatedUnix: time.Now().Unix(),
			Deleted:     false,
		}
		textRepID, upsertErr := tx.UpsertRepresentation(ctx, textRep)
		if upsertErr != nil {
			return fmt.Errorf("upsert annotation text representation: %w", upsertErr)
		}
		if upsertErr := s.repGen.upsertChunksForRepresentationWithStore(ctx, tx, textRepID, "text", chunkTextByChars(flattened, annotationChunkSize, annotationChunkOverlap, annotationChunkMinSize)); upsertErr != nil {
			return fmt.Errorf("persist annotation text chunks: %w", upsertErr)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// Count JSON representation; count text representation only if created.
	s.addRepresentations(1)
	if indexFlattenedText && trimmed != "" {
		s.addRepresentations(1)
	}
	return preview, nil
}

// enforceCachePolicy scans cacheDir and removes entries that violate
// the configured size or age limits.  It's safe to call even if neither
// policy is enabled; in that case it is a no-op.
func (s *Service) enforceCachePolicy(cacheDir string) error {
	// read the limits and any associated hooks under a read lock. we copy them
	// to locals so the rest of the logic can run without holding the lock for
	// the entire scan, which could be slow.
	s.ocrCacheMu.RLock()
	maxBytes := s.ocrCacheMaxBytes
	ttl := s.ocrCacheTTL
	statHook := s.ocrCacheStat
	s.ocrCacheMu.RUnlock()
	if maxBytes <= 0 && ttl <= 0 {
		return nil
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cache dir %s: %w", cacheDir, err)
	}

	type fileInfo struct {
		path string
		info os.FileInfo
	}
	var files []fileInfo
	var total int64
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(cacheDir, e.Name())
		// use test hook if provided; otherwise fall back to the real call
		var info os.FileInfo
		var err error
		if statHook != nil {
			info, err = statHook(e)
		} else {
			info, err = e.Info()
		}
		if err != nil {
			// log failure so that operators can investigate; include the
			// entry name since that is the only identifier available here.
			s.getLogger().Printf("enforceCachePolicy: failed to stat %s: %v", e.Name(), err)
			// a stat error typically means the entry is corrupt or otherwise
			// unreadable. retaining such files in the cache is unhelpful and
			// may prevent enforcement from making progress (e.g. if the file is
			// continuously failing). drop the entry outright, which also keeps
			// the total size calculation conservative and avoids evicting good
			// data because of a stuck bad entry. this mirrors the behaviour of
			// the original pre-optimization implementation and matches the
			// expectations of our regression tests.
			if rmErr := os.Remove(p); rmErr != nil && !os.IsNotExist(rmErr) {
				// removal failure is unfortunate but not fatal; log and continue.
				s.getLogger().Printf("enforceCachePolicy: failed to remove stat-error file %s: %v", p, rmErr)
			}
			continue
		}
		files = append(files, fileInfo{path: p, info: info})
		total += info.Size()
	}

	// age-based eviction first
	if ttl > 0 {
		cutoff := now.Add(-ttl)
		kept := make([]fileInfo, 0, len(files))
		for _, f := range files {
			if f.info.ModTime().Before(cutoff) {
				if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("prune cache ttl: remove %s in %s: %w", f.path, cacheDir, err)
				}
				total -= f.info.Size()
				continue
			}
			kept = append(kept, f)
		}
		files = kept
	}

	// size-based eviction
	if maxBytes > 0 && total > maxBytes {
		sort.Slice(files, func(i, j int) bool {
			return files[i].info.ModTime().Before(files[j].info.ModTime())
		})
		for _, f := range files {
			if total <= maxBytes {
				break
			}
			if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("prune cache size: remove %s in %s: %w", f.path, cacheDir, err)
			}
			total -= f.info.Size()
		}
	}

	return nil
}

// EnforceOCRCachePolicy exposes cache policy enforcement for tests.
func (s *Service) EnforceOCRCachePolicy(cacheDir string) error {
	return s.enforceCachePolicy(cacheDir)
}

func (s *Service) readOrComputeOCR(ctx context.Context, doc model.Document, content []byte) (string, error) {
	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create ocr cache dir: %w", err)
	}

	cachePath := filepath.Join(cacheDir, computeContentHash(content)+".md")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), nil
	}

	// enforce any configured cache policy around writes. A full directory scan
	// can be expensive, so we only run it on a configurable write interval.
	// The counter increments only when we are about to perform a real write
	// (cache miss), not for cache hits.
	ocrText, err := s.ocr.Extract(ctx, doc.RelPath, content)
	if err != nil {
		return "", fmt.Errorf("ocr extract %s: %w", doc.RelPath, err)
	}

	ocrBytes := []byte(strings.ReplaceAll(strings.ReplaceAll(ocrText, "\r\n", "\n"), "\r", "\n"))
	if err := os.WriteFile(cachePath, ocrBytes, 0o644); err != nil {
		return "", fmt.Errorf("write ocr cache: %w", err)
	}
	shouldEnforceAfterWrite := s.markOCRCacheWrite()
	if shouldEnforceAfterWrite {
		// read the hook under lock and execute it outside the lock to avoid
		// races. fallback to the real enforcement method if no hook is set.
		s.ocrCacheMu.RLock()
		enforceHook := s.ocrCacheEnforce
		s.ocrCacheMu.RUnlock()
		var err error
		if enforceHook != nil {
			err = enforceHook(cacheDir)
		} else {
			err = s.enforceCachePolicy(cacheDir)
		}
		if err != nil {
			// enforcement failure should not prevent the caller from
			// receiving the OCR result. log and continue instead of
			// returning an error; the cache write has already succeeded.
			s.getLogger().Printf("enforceCachePolicy(%s) failed: %v", cacheDir, err)
		}
	}
	return string(ocrBytes), nil
}

// ReadOrComputeOCR exposes OCR cache lookup/computation for tests.
func (s *Service) ReadOrComputeOCR(ctx context.Context, doc model.Document, content []byte) (string, error) {
	return s.readOrComputeOCR(ctx, doc, content)
}

func (s *Service) readOrComputeTranscript(ctx context.Context, doc model.Document, content []byte) (string, error) {
	if s.transcriber == nil {
		return "", errors.New("transcriber not configured")
	}

	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "transcribe")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create transcript cache dir: %w", err)
	}

	cachePath := filepath.Join(cacheDir, computeContentHash(content)+".txt")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), nil
	}

	transcript, err := s.transcriber.Transcribe(ctx, doc.RelPath, content)
	if err != nil {
		return "", fmt.Errorf("%w: transcribe %s: %w", ErrTranscriptProviderFailure, doc.RelPath, err)
	}

	transcriptBytes := []byte(strings.ReplaceAll(strings.ReplaceAll(transcript, "\r\n", "\n"), "\r", "\n"))
	if err := os.WriteFile(cachePath, transcriptBytes, 0o644); err != nil {
		return "", fmt.Errorf("write transcript cache: %w", err)
	}
	shouldEnforceAfterWrite := s.markOCRCacheWrite()
	if shouldEnforceAfterWrite {
		// Reuse the same cache-policy limits/hooks as OCR for now so transcript
		// cache growth is bounded under the same operational policy.
		s.ocrCacheMu.RLock()
		enforceHook := s.ocrCacheEnforce
		s.ocrCacheMu.RUnlock()
		var err error
		if enforceHook != nil {
			err = enforceHook(cacheDir)
		} else {
			err = s.enforceCachePolicy(cacheDir)
		}
		if err != nil {
			s.getLogger().Printf("enforceCachePolicy(%s) failed: %v", cacheDir, err)
		}
	}
	return string(transcriptBytes), nil
}

// ReadOrComputeTranscript exposes transcript cache lookup/computation for tests.
func (s *Service) ReadOrComputeTranscript(ctx context.Context, doc model.Document, content []byte) (string, error) {
	return s.readOrComputeTranscript(ctx, doc, content)
}

// flattenJSONForIndexing walks an arbitrary JSON-like structure and
// builds a string suitable for indexing. When a value cannot be marshaled we
// log the failure to the provided logger and continue; previously this helper
// used the package-global log.Printf which made testing and customization
// difficult.
func (s *Service) flattenJSONForIndexing(v interface{}) string {
	var lines []string
	var walk func(prefix string, value interface{})
	walk = func(prefix string, value interface{}) {
		switch typed := value.(type) {
		case map[string]interface{}:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				next := key
				if prefix != "" {
					next = prefix + "." + key
				}
				walk(next, typed[key])
			}
		case []interface{}:
			for i, item := range typed {
				next := fmt.Sprintf("%s[%d]", prefix, i)
				walk(next, item)
			}
		case string:
			if text := strings.TrimSpace(typed); text != "" {
				if prefix != "" {
					lines = append(lines, fmt.Sprintf("%s: %s", prefix, text))
				} else {
					lines = append(lines, text)
				}
			}
		default:
			b, err := json.Marshal(typed)
			if err != nil {
				// log the marshaling failure with context but continue processing
				// so that other entries aren't dropped. include prefix, the
				// value being marshaled and a reference to json.Marshal in the
				// message so the source is obvious when debugging.
				s.getLogger().Printf("flattenJSONForIndexing: json.Marshal failed for prefix=%q type=%T error=%v (lines so far=%d)",
					prefix, typed, err, len(lines))
				return
			}
			str := string(b)
			if prefix != "" {
				lines = append(lines, fmt.Sprintf("%s: %s", prefix, str))
			} else {
				lines = append(lines, str)
			}
		}
	}

	walk("", v)
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if out == "" {
		raw, err := json.Marshal(v)
		if err != nil {
			// log marshal failure so that debugging can surface problematic
			// values; returning early avoids converting a nil slice to string
			// avoid logging raw value contents in case they contain sensitive data.
			s.getLogger().Printf("flattenJSONForIndexing: fallback json.Marshal failed error=%v type=%T", err, v)
			return ""
		}
		return string(raw)
	}
	return out
}
