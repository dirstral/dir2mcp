package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/mistral"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/pdfutil"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/providerfactory"
	"github.com/dirstral/dir2mcp/internal/quality"
	"github.com/dirstral/dir2mcp/internal/scancache"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/internal/subtitle"
)

// annotationChunk* constants mirror the hardcoded parameters previously
// used when splitting annotation JSON/text into segments for indexing. They
// centralize tuning values and improve readability of call sites in this
// file. The defaults were 1200, 200 and 120 respectively.
const (
	annotationChunkSize    = 1200
	annotationChunkOverlap = 200
	annotationChunkMinSize = 120

	defaultOnDocumentDeletedMaxConcurrency = 8

	transcriberProviderAuto       = "auto"
	transcriberProviderMistral    = "mistral"
	transcriberProviderElevenLabs = "elevenlabs"
	transcriberProviderWhisper    = "whisper"
	transcriberProviderOff        = "off"
)

type Service struct {
	cfg           config.Config
	store         model.Store
	indexingState *appstate.IndexingState
	repGen        *RepresentationGenerator
	extractor     model.DocumentExtractor
	transcriber   model.Transcriber

	// fsys is the corpus filesystem abstraction used for discovery and byte
	// reads. Nil means "use a local filesystem rooted at cfg.RootDir" — the
	// default that preserves the historical local-corpus behavior. It is
	// resolved lazily via corpusFS() so callers never observe a nil backend.
	fsys corpusfs.CorpusFS

	// qualityGate screens generated transcript/OCR text for degenerate
	// output before it is chunked and embedded (spec 0.16.0). Nil when the
	// QualityGatesEnabled master switch is off — callers must treat a nil
	// gate as "skip screening", proceeding exactly as before the gate existed.
	qualityGate *quality.Gate

	// transcriptLanguage is the active STT provider profile's language tag
	// (SPEC 8.1.3), resolved once at construction and fed to the quality
	// gate's language detector. Empty when STT is off or no language is
	// configured, in which case the language detector self-skips.
	transcriptLanguage string

	// sttProvider/sttModel record the resolved STT provider profile name and
	// model so each machine-transcribed transcript representation can carry its
	// STT derivation identity in meta_json (SPEC §5.2/§8.6.7) and the re-ingest
	// gate can invalidate a stale transcript when the active STT model changes.
	// Empty when STT is off or no STT-capable profile resolves.
	sttProvider string
	sttModel    string

	// diarizeActive reports whether speaker diarization is active for
	// model-derived transcripts (SPEC §8.6.8): true only when diarization is
	// enabled (tri-state) AND the active STT backend advertises CapDiarize.
	// When active, diarizeProvider/diarizeModel record the diarization-capable
	// backend's profile name and STT model so a model-derived transcript carries
	// its diarize derivation identity (§8.6.7) and a backend change re-derives.
	// All three are zero when diarization is off (the default), making the STT
	// transcript path byte-identical to today.
	diarizeActive   bool
	diarizeProvider string
	diarizeModel    string

	// diarizer is the optional, injectable model-driven diarization seam
	// (SPEC §8.6.8). dir2mcp ships NO default diarizer: the capable backend
	// (self-hosted WhisperX/pyannote) is integrated out-of-band and wired in via
	// SetDiarizer. When nil and diarization is active, model-derived transcripts
	// remain un-attributed (sidecar <v> attribution still works); the config gate
	// guarantees diarize:true without a capable backend is CONFIG_INVALID, so a
	// nil diarizer here is never a silent partial-config.
	diarizer Diarizer

	// translator is the chat/generation client used to translate transcripts
	// into the configured target language(s) (SPEC §8.6.2). It is the chat
	// capability binding resolved at construction, or nil when translation is
	// disabled or no chat-capable provider resolves (in which case the optional
	// translate step self-skips). It is a model.Generator so the same prompt
	// path works for any chat provider (mistral/openai/gemini/cohere).
	translator model.Generator

	// translateTargetLangs holds the normalized target language tags transcripts
	// are translated into when translation is enabled (SPEC §8.6.2). Empty when
	// translation is off. Validation guarantees a non-empty list whenever
	// translation is enabled (enabling with an empty list is CONFIG_INVALID).
	translateTargetLangs []string

	// translateProvider/translateModel record the resolved chat provider/model
	// used for translation so each translated transcript representation can carry
	// its translation derivation identity in meta_json (SPEC §5.2/§8.6.7).
	translateProvider string
	translateModel    string

	// embedMultimodal is the resolved multimodal embedding mode (SPEC
	// 8.1.7): "off" (default), "augment", or "replace". When augment/
	// replace, media documents additionally (or exclusively) get a media
	// chunk embedded directly from their bytes.
	embedMultimodal string

	// ProbeDurationFunc overrides the audio/video duration probe (SPEC 8.1.7
	// time-window chunking). Defaults to avutil.Duration (ffprobe) when nil;
	// tests set it to supply a deterministic duration without requiring the
	// ffprobe binary.
	ProbeDurationFunc func(ctx context.Context, path string) (time.Duration, error)

	// DetectLeadingSilenceFunc overrides leading-silence detection used by the
	// optional transcript trim (dir2mcp#258, config media.trim_leading_silence).
	// Defaults to a thin wrapper over avutil.DetectLeadingSilence (ffmpeg
	// silencedetect) when nil; tests set it to supply a deterministic offset
	// without requiring the ffmpeg binary. It MUST be graceful: returning
	// (0, err) is treated as "do not trim".
	DetectLeadingSilenceFunc func(ctx context.Context, path string) (time.Duration, error)

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

	// optional callback invoked after a document is successfully tombstoned.
	// Used by the CLI to evict the document's chunks from the retrieval
	// service's in-memory maps so deleted files are no longer searchable.
	onDocumentsDeleted   func(relPaths []string)
	onDocumentsDeletedMu sync.RWMutex
	// bounds the compatibility wrapper used by SetOnDocumentDeleted so large
	// deletion batches do not spawn an unbounded number of goroutines.
	onDocumentDeletedMaxConcurrency int

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

	// sidecarIndex maps every discovered file's rel_path to its mtime, built once
	// per scan (setSidecarIndex). The transcript path uses it to detect subtitle
	// sidecars next to a media file and to mtime-gate their ingestion (§8.6.4).
	// Nil until a scan sets it; direct callers fall back to a one-shot walk.
	sidecarIndex map[string]int64
	sidecarMu    sync.RWMutex
}

// ErrTranscriptProviderFailure marks failures originating from the transcript
// provider call itself (as opposed to persistence/cache write failures).
var ErrTranscriptProviderFailure = errors.New("transcript provider failure")

type documentDeleteMarker interface {
	MarkDocumentDeleted(ctx context.Context, relPath string) error
}

// sidecarTranscriptRetirer is the optional store capability used on a forced STT
// reindex to tombstone a document's stale sidecar-sourced transcript
// representations before the fresh machine transcript is written (spec §8.6.4).
type sidecarTranscriptRetirer interface {
	SoftDeleteSidecarTranscripts(ctx context.Context, relPath string) (int, error)
}

// representationMetaReader is the optional store capability used by the
// derivation-identity re-ingest gate (spec §8.6.7): it reads the recorded
// meta_json of a document's active representation of a given rep_type so the
// ingest service can compare its STT/OCR derivation identity to the active
// model's identity. A store that does not implement it disables identity-driven
// re-derivation (content_hash remains the only reprocess trigger), preserving
// prior behaviour.
type representationMetaReader interface {
	RepresentationMetaByType(ctx context.Context, relPath, repType string) (string, error)
}

func NewService(cfg config.Config, store model.Store) (*Service, error) {
	svc := &Service{
		cfg:                             cfg,
		store:                           store,
		logger:                          log.Default(),
		onDocumentDeletedMaxConcurrency: defaultOnDocumentDeletedMaxConcurrency,
	}
	transcriber, err := TranscriberFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("configure transcriber: %w", err)
	}
	svc.transcriber = transcriber
	if rs, ok := store.(model.RepresentationStore); ok {
		svc.repGen = NewRepresentationGenerator(rs)
	}
	// Construct the output quality gate from config (spec 0.16.0). When the
	// master switch is on we use the package defaults (per-threshold config is
	// a follow-up); when off, the field stays nil and screening is skipped.
	if cfg.QualityGatesEnabled {
		svc.qualityGate = quality.New(quality.DefaultConfig())
	}
	svc.transcriptLanguage = sttExpectedLanguage(cfg)
	// Resolve the STT derivation identity (SPEC §8.6.7) from the same profile the
	// transcriber uses, so a recorded transcript identity can be compared against
	// the active one to detect a model swap. Empty when STT is off.
	if prof, ok := resolveSTTProfile(cfg); ok {
		svc.sttProvider = strings.TrimSpace(prof.Name)
		svc.sttModel = strings.TrimSpace(prof.STTModel)
		// Resolve the diarization binding (SPEC §8.6.8) from the SAME STT profile:
		// diarization is active when enabled (tri-state) AND the backend advertises
		// CapDiarize. The diarize derivation identity (§8.6.7) is the backend's
		// provider name + STT model, so a backend/model swap re-derives. When
		// inactive the fields stay empty and the STT path is unchanged.
		if config.DiarizationActive(cfg, prof) {
			svc.diarizeActive = true
			svc.diarizeProvider = strings.TrimSpace(prof.Name)
			svc.diarizeModel = strings.TrimSpace(prof.STTModel)
		}
	}
	// Resolve the optional transcript-translation binding (SPEC §8.6.2). When
	// translation is enabled we resolve the chat capability and build a
	// generator; when off (default), or no chat provider resolves, the field
	// stays nil and the translate step self-skips so behaviour is unchanged.
	svc.translateTargetLangs = append([]string(nil), cfg.MediaTranslateTargetLangs...)
	if cfg.MediaTranslateEnabled && len(svc.translateTargetLangs) > 0 {
		if tr, prof, terr := translatorFromConfig(cfg); terr == nil && tr != nil {
			svc.translator = tr
			svc.translateProvider = prof.Name
			svc.translateModel = strings.TrimSpace(prof.ChatModel)
		} else if terr != nil {
			svc.getLogger().Printf("transcript translation disabled: %v", terr)
		}
	}
	// Resolve the multimodal embedding mode once (SPEC 8.1.7); a missing or
	// unresolvable embed profile leaves it off (text-only).
	if ep, err := cfg.Providers().Resolve(provider.CapEmbed); err == nil {
		svc.embedMultimodal = provider.NormalizeEmbedMultimodal(ep.EmbedMultimodal)
	}
	return svc, nil
}

// SetOnDocumentDeleted registers a compatibility wrapper for callers that still
// expect per-document callbacks. New code should prefer SetOnDocumentsDeleted.
func (s *Service) SetOnDocumentDeleted(fn func(relPath string)) {
	if fn == nil {
		s.SetOnDocumentsDeleted(nil)
		return
	}
	s.SetOnDocumentsDeleted(func(relPaths []string) {
		maxConcurrency := s.onDocumentDeletedMaxConcurrency
		if maxConcurrency <= 0 {
			maxConcurrency = defaultOnDocumentDeletedMaxConcurrency
		}
		sem := make(chan struct{}, maxConcurrency)
		var wg sync.WaitGroup
		for _, relPath := range relPaths {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					<-sem
				}()
				defer func() {
					if r := recover(); r != nil {
						s.addErrors(1)
						s.getLogger().Printf("onDocumentDeleted panic for path %q (%s)", relPath, safePanicValue(r))
					}
				}()
				fn(relPath)
			}()
		}
		wg.Wait()
	})
}

// SetOnDocumentDeletedMaxConcurrency bounds the compatibility wrapper used by
// SetOnDocumentDeleted. Values less than 1 restore the default.
func (s *Service) SetOnDocumentDeletedMaxConcurrency(n int) {
	if n < 1 {
		n = defaultOnDocumentDeletedMaxConcurrency
	}
	s.onDocumentDeletedMaxConcurrency = n
}

// SetOnDocumentsDeleted registers a callback that is invoked once after
// markMissingAsDeleted tombstones one or more documents. The callback receives
// the set of deleted relative paths so callers can evict retrieval metadata in
// a single pass without rescanning for each individual document.
func (s *Service) SetOnDocumentsDeleted(fn func(relPaths []string)) {
	s.onDocumentsDeletedMu.Lock()
	defer s.onDocumentsDeletedMu.Unlock()
	s.onDocumentsDeleted = fn
}

// DiscoverOptionsFromConfig resolves ingest discovery behavior from config.
// Defaults mirror config.Config defaults: .gitignore support is enabled by
// default (IngestGitignore=true), and symlink following is disabled by default
// (IngestFollowSymlinks=false).
func DiscoverOptionsFromConfig(cfg config.Config) DiscoverOptions {
	options := DefaultDiscoverOptions()
	options.UseGitIgnore = cfg.IngestGitignore
	options.FollowSymlinks = cfg.IngestFollowSymlinks
	if cfg.IngestMaxFileMB > 0 {
		options.MaxSizeBytes = int64(cfg.IngestMaxFileMB) * 1024 * 1024
	}
	options.MediaVariants = MediaVariantOptionsFromConfig(cfg)
	return options
}

// mistralExtractor resolves the OCR provider (the bespoke mistral kind)
// via the provider model and adapts it to model.DocumentExtractor.
// Returns nil when no Mistral OCR credential is available (matching the
// prior "no key -> no extractor" behavior). Docling is handled
// separately below — it is a local tool, not a provider profile.
func mistralExtractor(cfg config.Config) model.DocumentExtractor {
	prof, err := cfg.Providers().ResolveExplicit(provider.CapOCR, "mistral-ocr", true)
	if err != nil {
		return nil
	}
	ex, err := providerfactory.Extractor(prof)
	if err != nil {
		return nil
	}
	return ex
}

// buildTranscriber adapts a resolved profile to a model.Transcriber.
// ElevenLabs voice/model/language/base-url are carried on the profile.
func buildTranscriber(prof provider.Profile) (model.Transcriber, error) {
	return providerfactory.Transcriber(prof)
}

// DocumentExtractorFromConfig resolves document extraction provider selection.
// Priority: configured docling command, auto-detected docling binary, then
// Mistral OCR (via the provider model). Selection mirrors
// DescribeDocumentExtractor so the diagnostic banner is always in sync.
//
// It uses a background context for any reachability probe; callers on hot
// paths that hold a request context should use
// DocumentExtractorFromConfigContext so the probe is cancellable.
func DocumentExtractorFromConfig(cfg config.Config) model.DocumentExtractor {
	return DocumentExtractorFromConfigContext(context.Background(), cfg)
}

// DocumentExtractorFromConfigContext is DocumentExtractorFromConfig with a
// caller-provided context, threaded into the docling-serve reachability probe.
func DocumentExtractorFromConfigContext(ctx context.Context, cfg config.Config) model.DocumentExtractor {
	decision := DescribeDocumentExtractorContext(ctx, cfg)
	switch decision.Name {
	case "docling":
		tpl := strings.TrimSpace(cfg.DoclingCommand)
		return NewDoclingExtractor(tpl)
	case "docling-serve":
		return NewDoclingServeExtractor(cfg.IngestDoclingServeURL)
	case "mistral-ocr":
		return mistralExtractor(cfg)
	default:
		return nil
	}
}

func TranscriberFromConfig(cfg config.Config) (model.Transcriber, error) {
	return TranscriberFromConfigWithLanguage(cfg, "")
}

// TranscriberFromConfigWithLanguage is TranscriberFromConfig with a
// per-request language override (the `language` arg of the transcribe
// MCP tool). A non-empty language is applied onto the resolved profile
// before the adapter is built so it reaches the wire (e.g. Mistral's
// `language` form field); empty leaves the profile's configured
// STTLanguage untouched.
func TranscriberFromConfigWithLanguage(cfg config.Config, language string) (model.Transcriber, error) {
	sel := strings.ToLower(strings.TrimSpace(cfg.STTProvider))
	if sel == "" {
		sel = transcriberProviderAuto
	}
	if sel == transcriberProviderOff || sel == "none" || sel == "disabled" {
		return nil, nil
	}

	// STT now resolves entirely through the provider model (SPEC
	// 8.1.3). The legacy stt.provider selector maps onto a profile:
	// explicit mistral/elevenlabs -> required (a missing credential is
	// a hard error); auto -> deterministic precedence among STT-capable
	// profiles, or STT off when none is eligible. ElevenLabs voice/
	// model/language/base-url are preserved on the profile (seedLegacy).
	r := cfg.Providers()
	langOverride := strings.TrimSpace(language)
	// build wraps BOTH resolver and factory/build failures with the
	// selected provider so the error contract is consistent regardless
	// of where the failure occurred.
	build := func(prof provider.Profile, err error) (model.Transcriber, error) {
		if err != nil {
			return nil, fmt.Errorf("stt provider %q: %w", sel, err)
		}
		if langOverride != "" {
			prof.STTLanguage = langOverride
		}
		// media.vad (dir2mcp#258) is a global toggle, applied onto whichever STT
		// profile resolves; providers without VAD support ignore it.
		prof.STTVAD = cfg.MediaVAD
		tr, berr := buildTranscriber(prof)
		if berr != nil {
			return nil, fmt.Errorf("stt provider %q: %w", sel, berr)
		}
		return tr, nil
	}
	switch sel {
	case transcriberProviderMistral:
		prof, err := r.ResolveExplicit(provider.CapSTT, "mistral-ocr", true)
		return build(prof, err)
	case transcriberProviderElevenLabs:
		prof, err := r.ResolveExplicit(provider.CapSTT, "elevenlabs", true)
		return build(prof, err)
	case transcriberProviderWhisper:
		// Self-hosted OpenAI-compatible STT (dir2mcp#240). The profile is
		// credential-less, so selection succeeds even without an api_key;
		// a missing base_url surfaces as a provider error at first use.
		prof, err := r.ResolveExplicit(provider.CapSTT, "whisper", true)
		return build(prof, err)
	case transcriberProviderAuto:
		prof, err := r.Resolve(provider.CapSTT)
		if errors.Is(err, provider.ErrNoProvider) {
			return nil, nil // nothing eligible -> STT off
		}
		return build(prof, err)
	default:
		return nil, fmt.Errorf("unsupported transcriber provider %q", sel)
	}
}

// sttExpectedLanguage resolves the active STT provider profile's language tag
// (SPEC 8.1.3), mirroring the provider selection in
// TranscriberFromConfigWithLanguage. It returns "" when STT is off, when no
// STT-capable profile resolves, or when the profile carries no language — in
// all of which cases the quality gate's language detector self-skips. Resolving
// from the profile (rather than the legacy stt.elevenlabs_language_code field)
// ensures Mistral/provider-profile setups still feed the gate a language.
// captionWordFilter builds the shared caption word filter from
// media.filter_words. The same filter is used for STT transcript chunking and
// sidecar-cue chunking so configured phrases are stripped consistently before
// embedding. An empty config yields an inactive filter (no-op).
func (s *Service) captionWordFilter() *subtitle.WordFilter {
	return subtitle.NewWordFilter(s.cfg.MediaFilterWords)
}

func sttExpectedLanguage(cfg config.Config) string {
	prof, ok := resolveSTTProfile(cfg)
	if !ok {
		return ""
	}
	return strings.TrimSpace(prof.STTLanguage)
}

// resolveSTTProfile resolves the active STT provider profile (SPEC 8.1.3),
// mirroring the provider selection in TranscriberFromConfigWithLanguage. It
// returns ok=false when STT is off, when no STT-capable profile resolves, or
// when the selector is unrecognised — in all of which cases callers treat STT as
// having no derivation identity. It is the single resolution point shared by the
// expected-language hint and the STT derivation identity recorded on transcript
// representations (§8.6.7), so both observe the exact same profile.
func resolveSTTProfile(cfg config.Config) (provider.Profile, bool) {
	sel := strings.ToLower(strings.TrimSpace(cfg.STTProvider))
	if sel == "" {
		sel = transcriberProviderAuto
	}
	if sel == transcriberProviderOff || sel == "none" || sel == "disabled" {
		return provider.Profile{}, false
	}
	r := cfg.Providers()
	var (
		prof provider.Profile
		err  error
	)
	switch sel {
	case transcriberProviderMistral:
		prof, err = r.ResolveExplicit(provider.CapSTT, "mistral-ocr", true)
	case transcriberProviderElevenLabs:
		prof, err = r.ResolveExplicit(provider.CapSTT, "elevenlabs", true)
	case transcriberProviderWhisper:
		prof, err = r.ResolveExplicit(provider.CapSTT, "whisper", true)
	case transcriberProviderAuto:
		prof, err = r.Resolve(provider.CapSTT)
	default:
		return provider.Profile{}, false
	}
	if err != nil {
		return provider.Profile{}, false
	}
	return prof, true
}

// translatorFromConfig resolves the chat-capability binding used to translate
// transcripts (SPEC §8.6.2: "uses the chat capability unless a dedicated
// binding is configured") and builds a model.Generator from it. A dedicated
// translate provider/model can be configured by binding the chat capability to
// a specific provider; with no explicit binding the resolver picks the first
// eligible chat-capable profile in precedence order. Returns ErrNoProvider
// (wrapped) when nothing eligible resolves so the caller can leave translation
// off rather than failing ingest.
func translatorFromConfig(cfg config.Config) (model.Generator, provider.Profile, error) {
	prof, err := cfg.Providers().Resolve(provider.CapChat)
	if err != nil {
		return nil, provider.Profile{}, fmt.Errorf("resolve chat provider for translation: %w", err)
	}
	gen, err := providerfactory.Generator(prof)
	if err != nil {
		return nil, provider.Profile{}, fmt.Errorf("build translation generator (%s): %w", prof.Name, err)
	}
	return gen, prof, nil
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

func (s *Service) SetDocumentExtractor(extractor model.DocumentExtractor) {
	s.extractor = extractor
}

func (s *Service) SetOCR(ocr model.OCR) {
	s.extractor = ocr
}

func (s *Service) SetTranscriber(transcriber model.Transcriber) {
	s.transcriber = transcriber
}

// SetTranslator overrides the transcript-translation binding and its target
// languages, primarily for tests. Passing a nil generator (or empty langs)
// disables translation. The recorded provider/model are written into translated
// transcripts' meta_json. Target languages are normalized (trimmed/lower-cased).
func (s *Service) SetTranslator(translator model.Generator, providerName, modelName string, targetLangs []string) {
	s.translator = translator
	s.translateProvider = strings.TrimSpace(providerName)
	s.translateModel = strings.TrimSpace(modelName)
	norm := make([]string, 0, len(targetLangs))
	for _, l := range targetLangs {
		if t := strings.ToLower(strings.TrimSpace(l)); t != "" {
			norm = append(norm, t)
		}
	}
	s.translateTargetLangs = norm
}

// SetTranscriptLanguage overrides the recorded source-transcript language,
// primarily for tests that need a deterministic source_language without
// resolving an STT provider profile.
func (s *Service) SetTranscriptLanguage(language string) {
	s.transcriptLanguage = strings.TrimSpace(language)
}

// SetSTTIdentity overrides the recorded STT derivation identity (provider name
// and model) written into machine transcript meta_json and compared by the
// re-ingest gate (spec §8.6.7). It mirrors SetTranscriptLanguage / SetTranslator
// for tests that need a deterministic STT identity without resolving a real STT
// provider profile. Values are trimmed.
func (s *Service) SetSTTIdentity(providerName, model string) {
	s.sttProvider = strings.TrimSpace(providerName)
	s.sttModel = strings.TrimSpace(model)
}

// SetDiarizer wires the optional model-driven diarization seam (SPEC §8.6.8) and
// records the diarize derivation identity (provider name + model, §8.6.7). It is
// the injection point for an out-of-band diarization-capable backend (dir2mcp
// ships no default diarizer). Passing a non-nil diarizer activates the
// model-driven attribution path; the provider/model are written into a diarized
// transcript's meta_json and folded into its derivation identity. Values are
// trimmed. Primarily for wiring and tests; production resolution happens in
// NewService from the STT profile + diarize config.
func (s *Service) SetDiarizer(d Diarizer, providerName, modelName string) {
	s.diarizer = d
	s.diarizeActive = d != nil
	s.diarizeProvider = strings.TrimSpace(providerName)
	s.diarizeModel = strings.TrimSpace(modelName)
}

// SetCorpusFS overrides the corpus filesystem backend used for discovery and
// byte reads. Passing nil restores the default local-filesystem backend rooted
// at cfg.RootDir.
func (s *Service) SetCorpusFS(fsys corpusfs.CorpusFS) {
	s.fsys = fsys
}

// corpusFS resolves the active corpus filesystem, defaulting to a local
// filesystem rooted at cfg.RootDir when none was injected. This keeps local
// corpora behaving exactly as before while allowing an S3 (or other) backend to
// be supplied.
func (s *Service) corpusFS() corpusfs.CorpusFS {
	if s.fsys != nil {
		return s.fsys
	}
	return corpusfs.NewLocalFS(s.cfg.RootDir)
}

// openScanCache returns an opened directory-discovery scan cache (issue #267
// item 5) when the feature is enabled in config AND the active corpus backend is
// the local filesystem; otherwise it returns nil so discovery performs an
// unoptimized full walk. The cache only benefits (and is only honored by) the
// local-filesystem walker — object-store backends key change detection off the
// ETag instead — so it is never opened for a remote source. The caller owns the
// returned cache's lifetime and must Close it.
func (s *Service) openScanCache() *scancache.SQLiteCache {
	if !s.cfg.IngestScanCache {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(s.cfg.Source.Kind)) {
	case "", "local", "nfs":
		// local-filesystem backends only.
	default:
		return nil
	}
	stateDir := strings.TrimSpace(s.cfg.StateDir)
	if stateDir == "" {
		return nil
	}
	return scancache.Open(scancache.DefaultPath(stateDir))
}

// SetQualityGate overrides the output quality gate (spec 0.16.0). A nil gate
// disables screening so generated transcript/OCR text is chunked and embedded
// without quarantine. Mirrors SetTranscriber for tests.
func (s *Service) SetQualityGate(gate *quality.Gate) {
	s.qualityGate = gate
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

	discoverOpts := DiscoverOptionsFromConfig(s.cfg)
	// Attach the optional directory-discovery scan cache (issue #267 item 5) for
	// the local-filesystem backend only. It is opened for the duration of this
	// scan and closed afterward; a failure to open it is non-fatal (we just walk
	// without the optimization). The walker only ever trusts it after confirming
	// each directory's mtime and re-stat'ing its children, so it cannot cause a
	// changed file to be missed.
	if cache := s.openScanCache(); cache != nil {
		defer func() { _ = cache.Close() }()
		discoverOpts.ScanCache = cache
	}
	discovered, err := s.corpusFS().Walk(ctx, s.cfg.RootDir, discoverOpts.corpusfsOptions())
	if err != nil {
		return err
	}
	// Record discovered file mtimes (full, pre-dedup set) so the transcript path
	// can detect subtitle sidecars next to any media rendition and mtime-gate
	// their ingestion (§8.6.4) even for renditions dropped by variant grouping.
	s.setSidecarIndex(discovered)

	// Collapse multi-rendition media to a single canonical file (spec §8.6.5)
	// before ingestion so chunks/embeddings are not duplicated across renditions.
	// No-op when media.variants.group is disabled (the default).
	discovered = SelectMediaVariants(discovered, discoverOpts.MediaVariants)

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

// etagUnchanged reports whether a remote (object-store) source object can be
// treated as unchanged without re-reading its bytes (SPEC §7.8.3, #245). The
// ETag is the cheap change token for S3: when the discovered object carries a
// non-empty ETag, a prior document recorded the same ETag and size, and that
// document is healthy (active, not in error status), the object body has not
// changed and a full GET + content_hash recompute is unnecessary.
//
// It is deliberately conservative and a strict no-op for local/NFS corpora,
// whose DiscoveredFile.ETag is always empty: there the (size, mtime) pre-check
// plus content_hash confirm path is left entirely intact. A forced reindex
// always bypasses the skip so an operator can recover regardless of token state.
func etagUnchanged(f DiscoveredFile, existing model.Document, forceReindex bool) bool {
	if forceReindex {
		return false
	}
	if strings.TrimSpace(f.ETag) == "" {
		return false
	}
	if existing.Deleted || existing.Status == "error" {
		return false
	}
	if strings.TrimSpace(existing.ETag) != strings.TrimSpace(f.ETag) {
		return false
	}
	// Size is part of the cheap S3 signal alongside the ETag; a mismatch means
	// the object differs even if a (truncated/legacy) ETag happens to collide.
	return existing.SizeBytes == f.SizeBytes
}

// trySkipUnchangedRemoteDocument implements the remote (S3) incremental fast
// path (SPEC §7.8.3, #245). It looks up the recorded document and, when the
// object's ETag+size signal an unchanged body, records run-progress counters and
// returns handled=true so the caller skips the GET + content_hash recompute. It
// returns handled=false (no error) when the object is new, changed, or local
// (empty ETag), letting the normal read/hash path run; a genuine store failure
// is returned as an error. content_hash stays the canonical identity and the
// stored row is left untouched on the skip path.
func (s *Service) trySkipUnchangedRemoteDocument(ctx context.Context, f DiscoveredFile, forceReindex bool, seen map[string]struct{}) (bool, error) {
	existing, err := s.store.GetDocumentByPath(ctx, f.RelPath)
	if err != nil {
		if isUnexpectedStoreErr(err) {
			return false, fmt.Errorf("get existing document: %w", err)
		}
		return false, nil
	}
	if !etagUnchanged(f, existing, forceReindex) {
		return false, nil
	}
	// A media object's own ETag does not reflect changes to an adjacent subtitle
	// sidecar (.srt/.vtt/.ttml): buildDocumentWithContent folds the sidecar
	// fingerprint into ContentHash, but the ETag fast-path runs before that
	// recompute. So a sidecar added/changed/removed while the media bytes are
	// unchanged would be missed by an ETag-only comparison. Recompute the current
	// sidecar fingerprint cheaply (sibling paths + mtimes from the in-memory
	// sidecar index — no media read) and skip only when it ALSO matches the
	// persisted value; re-read when either the ETag or the fingerprint differs
	// (SPEC §7.8.3, #298). For non-media objects and media with no sidecar the
	// fingerprint is empty on both sides, so the fast path behaves as before.
	currentFP := s.sidecarFingerprint(ctx, f.RelPath, ClassifyDocType(f.RelPath))
	if currentFP != existing.SidecarFingerprint {
		return false, nil
	}
	// Derivation-identity gate (spec §8.6.7): the ETag/fingerprint fast path
	// proves the BYTES are unchanged, but a transcript/OCR representation may
	// still be stale because the active STT/OCR model changed since it was
	// derived. When the recorded derivation identity no longer matches, fall back
	// to the full read/hash path so the stale representation is re-derived rather
	// than silently skipped. Only meaningful for an existing ok document.
	if existing.Status == "ok" && s.derivationIdentityStale(ctx, f.RelPath) {
		return false, nil
	}
	s.skipUnchangedRemoteDocument(ctx, f, existing, seen)
	return true, nil
}

// skipUnchangedRemoteDocument records the run-progress counters for an object
// whose ETag matched (so it was not re-read) and preserves the existing
// behavior for archive containers, whose already-ingested members must be
// retained in `seen` so markMissingAsDeleted does not tombstone them. The stored
// document row is intentionally left untouched: its content_hash, ETag, and
// representations are still valid. Counters mirror the unchanged-content path so
// status totals stay consistent across runs.
func (s *Service) skipUnchangedRemoteDocument(ctx context.Context, f DiscoveredFile, existing model.Document, seen map[string]struct{}) {
	switch existing.Status {
	case "ok":
		s.addIndexed(1)
	case "skipped", "secret_excluded":
		s.addSkipped(1)
	}
	// An unchanged archive still owns its previously-extracted members; retain
	// them so they are not treated as deletions this run.
	if existing.DocType == "archive" && seen != nil {
		s.retainArchiveMembers(ctx, f.RelPath, seen)
	}
}

// persistBuildError records a document whose body could not be read/built as a
// status="error" row (with the source ETag preserved) and returns the original
// build error. The scan error counter is intentionally not incremented here:
// runScan already counts any non-nil return value as an error.
func (s *Service) persistBuildError(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, buildErr error) error {
	doc := model.Document{
		RelPath:      f.RelPath,
		DocType:      ClassifyDocType(f.RelPath),
		SizeBytes:    f.SizeBytes,
		MTimeUnix:    f.MTimeUnix,
		ETag:         f.ETag,
		Status:       "error",
		Deleted:      false,
		ErrorMessage: RedactSecretsInMessage(buildErr.Error(), secretPatterns),
	}
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("upsert error document: %w", err)
	}
	return buildErr
}

// resolveNeedsProcessing decides whether a document's representations must be
// (re)generated. content_hash is the primary trigger (or --force, or a brand-new
// document, via needsReprocessing). Two further triggers apply on otherwise
// unchanged content:
//   - a document that previously failed representation generation is retried so
//     a transient error recovers without a full reindex;
//   - a derived representation whose recorded derivation identity no longer
//     matches the active STT/OCR model is stale and must be re-derived (spec
//     §8.6.7). The identity probe runs only when the content gate did not already
//     trigger and the new document is ok, so it adds no work on the common path.
func (s *Service) resolveNeedsProcessing(ctx context.Context, existingDoc, doc model.Document, forceReindex bool) bool {
	if needsReprocessing(existingDoc.ContentHash, doc.ContentHash, forceReindex) {
		return true
	}
	if existingDoc.Status == "error" {
		return true
	}
	if doc.Status == "ok" && s.derivationIdentityStale(ctx, doc.RelPath) {
		return true
	}
	return false
}

// refreshDocID re-reads the persisted document after an upsert to obtain the
// store-assigned DocID needed for downstream representation creation
// (UpsertDocument returns only an error). A not-found result is ignored — it
// would be surprising immediately after a successful upsert and is already
// handled by the store; any other error is propagated.
func (s *Service) refreshDocID(ctx context.Context, doc *model.Document) error {
	updated, err := s.store.GetDocumentByPath(ctx, doc.RelPath)
	if err == nil {
		doc.DocID = updated.DocID
		return nil
	}
	if isNotFoundError(err) {
		return nil
	}
	return fmt.Errorf("fetch document after upsert: %w", err)
}

func (s *Service) processDocument(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}) error {
	// Remote (S3) incremental fast path (SPEC §7.8.3, #245): when the object's
	// ETag+size match the recorded document, the body is unchanged, so skip the
	// full GET + content_hash recompute entirely. No-op for local/NFS corpora
	// (empty ETag) and under --force/reindex.
	if handled, err := s.trySkipUnchangedRemoteDocument(ctx, f, forceReindex, seen); err != nil || handled {
		return err
	}

	doc, content, buildErr := s.buildDocumentWithContent(ctx, f, secretPatterns)
	if buildErr != nil {
		return s.persistBuildError(ctx, f, secretPatterns, buildErr)
	}

	existingDoc, err := s.store.GetDocumentByPath(ctx, doc.RelPath)
	if isUnexpectedStoreErr(err) {
		return fmt.Errorf("get existing document: %w", err)
	}

	needsProcessing := s.resolveNeedsProcessing(ctx, existingDoc, doc, forceReindex)
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}

	if err := s.refreshDocID(ctx, &doc); err != nil {
		return err
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
		return s.handleArchiveDocument(ctx, f, secretPatterns, forceReindex, seen, needsProcessing)
	}

	if !needsProcessing || doc.Status != "ok" {
		return nil
	}

	if err := s.generateRepresentations(ctx, doc, content, forceReindex); err != nil {
		// Persist error status so the next incremental run retries this document
		// and operators can identify it via status queries.  Silence the upsert
		// error since the original rep error is the more actionable signal.
		doc.Status = "error"
		doc.ErrorMessage = RedactSecretsInMessage(err.Error(), secretPatterns)
		_ = s.store.UpsertDocument(ctx, doc)
		return fmt.Errorf("generate representations: %w", err)
	}
	return nil
}

// handleArchiveDocument handles an archive-type document: if the archive
// content changed (or a full reindex was requested) it extracts and processes
// the members; otherwise it retains the existing member paths in seen so that
// markMissingAsDeleted does not tombstone them.
func (s *Service) handleArchiveDocument(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}, needsProcessing bool) error {
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

// processArchiveMembers extracts members from an archive and ingests each one
// as an independent document. One bad member is logged and skipped without
// aborting the rest.
func (s *Service) processArchiveMembers(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}) error {
	// Archive readers need a real filesystem path. Localize returns the in-root
	// path for a local corpus (no-op cleanup) or a temp download for an object
	// store, so this works uniformly across backends.
	localPath, cleanup, err := s.corpusFS().Localize(ctx, f.RelPath)
	if err != nil {
		s.getLogger().Printf("archive localize %s: %v", f.RelPath, err)
		return nil // localize failure is non-fatal; archive stays "skipped"
	}
	defer cleanup()
	members, err := extractArchiveMembers(localPath, f.RelPath)
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
	needsProcessing := s.resolveNeedsProcessing(ctx, existingDoc, doc, forceReindex)

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
	if err := s.generateRepresentations(ctx, doc, content, forceReindex); err != nil {
		// Persist error status so the next incremental run retries this document
		// and operators can identify it via status queries.  Silence the upsert
		// error since the original rep error is the more actionable signal.
		doc.Status = "error"
		doc.ErrorMessage = RedactSecretsInMessage(err.Error(), secretPatterns)
		_ = s.store.UpsertDocument(ctx, doc)
		return fmt.Errorf("generate representations: %w", err)
	}
	return nil
}

func (s *Service) buildDocumentWithContent(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp) (model.Document, []byte, error) {
	docType := ClassifyDocType(f.RelPath)
	doc := model.Document{
		RelPath:   f.RelPath,
		DocType:   docType,
		SizeBytes: f.SizeBytes,
		MTimeUnix: f.MTimeUnix,
		// Record the source's cheap change token (S3 ETag; empty for local/NFS)
		// so a later incremental scan can skip the re-read when it is unchanged
		// (SPEC §7.8.3, #245). content_hash below stays the canonical identity.
		ETag:    f.ETag,
		Status:  "ok",
		Deleted: false,
	}

	rc, err := s.corpusFS().Open(ctx, f.RelPath)
	if err != nil {
		return doc, nil, fmt.Errorf("read %s: %w", f.RelPath, err)
	}
	content, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return doc, nil, fmt.Errorf("read %s: %w", f.RelPath, err)
	}
	// Fold any subtitle sidecar fingerprint (sibling paths + mtimes) into the
	// media document's content hash so the incremental gate (§7.6) re-processes
	// the media when a sidecar is added, removed, or modified — even though the
	// media bytes are unchanged. Empty for non-media docs or media with no
	// sidecar, preserving the existing hash exactly. The same fingerprint is
	// persisted separately on the row (SidecarFingerprint) so the remote ETag
	// fast path can detect a sidecar change without re-reading the media bytes
	// (SPEC §7.8.3, #298).
	sidecarFP := s.sidecarFingerprint(ctx, f.RelPath, docType)
	doc.SidecarFingerprint = sidecarFP
	doc.ContentHash = mediaContentHash(content, sidecarFP)

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

	deletedPaths := make([]string, 0, len(paths))
	for _, relPath := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := deleter.MarkDocumentDeleted(ctx, relPath); err != nil {
			s.addErrors(1)
			continue
		}
		s.addDeleted(1)
		deletedPaths = append(deletedPaths, relPath)
	}
	if len(deletedPaths) == 0 {
		return nil
	}
	s.onDocumentsDeletedMu.RLock()
	onDeleted := s.onDocumentsDeleted
	s.onDocumentsDeletedMu.RUnlock()
	if onDeleted != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.addErrors(1)
					s.getLogger().Printf("onDocumentsDeleted panic for %d paths (%s)", len(deletedPaths), safePanicValue(r))
				}
			}()
			onDeleted(append([]string(nil), deletedPaths...))
		}()
	}
	return nil
}

func safePanicValue(r interface{}) string {
	typeName := "<nil>"
	if t := reflect.TypeOf(r); t != nil {
		typeName = t.String()
	}
	return "type=" + typeName
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

// generateExtractedAndMediaRepresentations handles non-text "visual" and media
// documents: the OCR/extractor text representation and, under multimodal
// augment/replace (SPEC 8.1.7), direct media chunks (one per page for PDFs,
// one per time window for audio/video, one for an image). In `replace` the
// media is embedded from its bytes instead of via OCR→text (OCR is skipped);
// `augment` keeps OCR and adds the media chunks. It returns whether any media
// chunks were produced so the caller can apply the same `replace`-skips-text
// rule to the audio transcript path.
func (s *Service) generateExtractedAndMediaRepresentations(ctx context.Context, doc model.Document, content []byte) (bool, error) {
	// A non-empty span set means media chunks will be produced. In `replace` we
	// then skip OCR; in `augment` OCR is kept alongside.
	spans := s.mediaSpansFor(ctx, doc, content)
	mediaProduced := len(spans) > 0
	skipOCR := s.embedMultimodal == "replace" && mediaProduced

	if ShouldGenerateExtractedMarkdown(doc.DocType) && s.extractor != nil && !skipOCR {
		if err := s.generateOCRMarkdownRepresentation(ctx, doc, content); err != nil {
			return false, err
		}
		s.addRepresentations(1)
	}
	if mediaProduced {
		if err := s.repGen.GenerateMediaChunks(ctx, doc, computeRepHash(content), spans); err != nil {
			return false, err
		}
		s.addRepresentations(1)
	}
	return mediaProduced, nil
}

// Default media-window lengths for direct audio/video embedding (SPEC 8.1.7).
// Conservative values at or below the per-modality per-request caps (audio
// ≤ 180 s, video ≤ 120 s) that also leave headroom under the unified
// 8192-token budget.
const (
	audioWindowMS = 120 * 1000
	videoWindowMS = 60 * 1000
)

// Per-modality window caps (SPEC 8.1.7): the maximum window length the
// keyframe-drift logic in avutil.ExtractSegment can serve without a clip
// exceeding the per-request duration cap (audio ≤ 180 s, video ≤ 120 s). A
// configured window (media.audio_window_sec / media.video_window_sec) is
// clamped to these so a misconfiguration can never push a clip over the cap.
const (
	audioWindowCapMS = 180 * 1000
	videoWindowCapMS = 120 * 1000
)

// resolveMediaWindowMS returns the window length (ms) to use for windowing
// media of the given doc type. A positive configured value (cfgSec seconds)
// overrides the default; values exceeding the per-modality cap are clamped
// (with a warning); zero/negative values fall back to the default. This keeps
// behavior identical to the hardcoded constants when unconfigured (SPEC 8.1.7).
func (s *Service) resolveMediaWindowMS(cfgSec, defaultMS, capMS int, modality string) int {
	if cfgSec <= 0 {
		return defaultMS
	}
	// Compare in seconds before converting to ms: a huge cfgSec would overflow
	// cfgSec*1000 (wrapping negative and slipping past the cap), so the cap
	// check must run first on the un-multiplied value.
	if cfgSec > capMS/1000 {
		s.getLogger().Printf("multimodal: configured %s window %ds exceeds the per-modality cap (%ds); clamping to the cap", modality, cfgSec, capMS/1000)
		return capMS
	}
	return cfgSec * 1000
}

// mediaSpansFor returns the per-chunk spans to embed directly for doc under the
// multimodal mode (SPEC 8.1.7): nil when media embedding is off or doc is not
// an embeddable media type; one `page` span for an image; one `page` span per
// page for a PDF; one `time` span per window for audio/video. Media whose unit
// count can't be determined (unreadable PDF, undecodable duration, missing
// ffprobe) yields nil — media is skipped and the text path is kept — rather
// than failing the ingest.
func (s *Service) mediaSpansFor(ctx context.Context, doc model.Document, content []byte) []model.Span {
	if s.embedMultimodal != "augment" && s.embedMultimodal != "replace" {
		return nil
	}
	switch doc.DocType {
	case "image":
		return []model.Span{{Kind: "page", Page: 1}}
	case "pdf":
		return s.pdfPageSpans(doc, content)
	case "audio":
		if !isEmbeddableAudio(doc.RelPath) {
			return nil
		}
		windowMS := s.resolveMediaWindowMS(s.cfg.MediaAudioWindowSec, audioWindowMS, audioWindowCapMS, "audio")
		return s.mediaTimeSpans(ctx, doc, windowMS)
	case "video":
		windowMS := s.resolveMediaWindowMS(s.cfg.MediaVideoWindowSec, videoWindowMS, videoWindowCapMS, "video")
		return s.mediaTimeSpans(ctx, doc, windowMS)
	default:
		return nil
	}
}

// pdfPageSpans returns one `page` span per PDF page, or nil when the page count
// can't be read (media skipped, OCR kept).
func (s *Service) pdfPageSpans(doc model.Document, content []byte) []model.Span {
	n, err := pdfutil.PageCount(content)
	if err != nil {
		s.getLogger().Printf("multimodal: PDF page count failed for %s (%v); skipping direct media embedding", doc.RelPath, err)
		return nil
	}
	if n < 1 {
		s.getLogger().Printf("multimodal: PDF page count invalid for %s (count=%d); skipping direct media embedding", doc.RelPath, n)
		return nil
	}
	spans := make([]model.Span, n)
	for i := 0; i < n; i++ {
		spans[i] = model.Span{Kind: "page", Page: i + 1}
	}
	return spans
}

// probeDuration resolves doc's media duration using the configured probe
// (ProbeDurationFunc, defaulting to avutil.Duration). It is the shared entry
// point for both time-window chunking and the quality gate's density check.
// It resolves the media through the CorpusFS (Localize) so non-local backends
// (e.g. S3) still get duration-aware behavior; for LocalFS this is the real
// path with a no-op cleanup, preserving local behavior.
func (s *Service) probeDuration(ctx context.Context, doc model.Document) (time.Duration, error) {
	localPath, cleanup, err := s.corpusFS().Localize(ctx, doc.RelPath)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	probe := s.ProbeDurationFunc
	if probe == nil {
		probe = avutil.Duration
	}
	return probe(ctx, localPath)
}

// detectLeadingSilence resolves the leading-silence duration for doc's media
// using the configured detector (DetectLeadingSilenceFunc, defaulting to
// avutil.DetectLeadingSilence with the configured threshold). It is graceful by
// contract: any error, or ffmpeg being absent, yields a 0 offset (no trim).
// Media is resolved through the CorpusFS (Localize) so non-local backends still
// get the behavior; for LocalFS this is the real path with a no-op cleanup.
func (s *Service) detectLeadingSilence(ctx context.Context, doc model.Document) time.Duration {
	localPath, cleanup, err := s.corpusFS().Localize(ctx, doc.RelPath)
	if err != nil {
		s.getLogger().Printf("leading-silence trim: localize %s: %v (skipping trim)", doc.RelPath, err)
		return 0
	}
	defer cleanup()

	detect := s.DetectLeadingSilenceFunc
	if detect == nil {
		thresholdDB := s.cfg.MediaSilenceThresholdDB
		detect = func(ctx context.Context, path string) (time.Duration, error) {
			return avutil.DetectLeadingSilence(ctx, path, thresholdDB, 0)
		}
	}
	offset, derr := detect(ctx, localPath)
	if derr != nil {
		s.getLogger().Printf("leading-silence trim: detect %s: %v (skipping trim)", doc.RelPath, derr)
		return 0
	}
	if offset < 0 {
		return 0
	}
	return offset
}

// mediaTimeSpans probes doc's duration and windows it into contiguous,
// non-overlapping `time` spans of at most windowMS (SPEC 8.1.7). Returns nil
// when the duration can't be determined (undecodable, or ffprobe absent),
// keeping the text path; the condition is a non-fatal per-document warning.
func (s *Service) mediaTimeSpans(ctx context.Context, doc model.Document, windowMS int) []model.Span {
	d, err := s.probeDuration(ctx, doc)
	if err != nil {
		s.getLogger().Printf("multimodal: media duration unavailable for %s (%v); skipping direct media embedding", doc.RelPath, err)
		return nil
	}
	totalMS := int(d.Milliseconds())
	if totalMS <= 0 {
		s.getLogger().Printf("multimodal: media duration invalid for %s (%dms); skipping direct media embedding", doc.RelPath, totalMS)
		return nil
	}
	return windowSpans(totalMS, windowMS)
}

// windowSpans splits [0, totalMS) into contiguous, non-overlapping half-open
// `time` windows of at most windowMS. Boundaries are deterministic so
// citations are stable across re-index (SPEC 8.1.7). The final window holds
// the remainder.
func windowSpans(totalMS, windowMS int) []model.Span {
	if totalMS <= 0 || windowMS <= 0 {
		return nil
	}
	spans := make([]model.Span, 0, (totalMS+windowMS-1)/windowMS)
	for start := 0; start < totalMS; start += windowMS {
		end := start + windowMS
		if end > totalMS {
			end = totalMS
		}
		spans = append(spans, model.Span{Kind: "time", StartMS: start, EndMS: end})
	}
	return spans
}

// isEmbeddableAudio reports whether an audio file's format is accepted for
// direct multimodal embedding (SPEC 8.1.7 lists MP3 and WAV). Other audio
// formats keep only their transcript path.
func isEmbeddableAudio(relPath string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(relPath), ".")) {
	case "mp3", "wav":
		return true
	default:
		return false
	}
}

func (s *Service) generateRepresentations(ctx context.Context, doc model.Document, content []byte, forceReindex bool) error {
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

		const titleScanLimit = 4096
		titleContent := content
		if len(titleContent) > titleScanLimit {
			titleContent = titleContent[:titleScanLimit]
		}
		s.persistTitleIfFound(ctx, doc, string(titleContent))
		return nil
	}

	mediaProduced, err := s.generateExtractedAndMediaRepresentations(ctx, doc, content)
	if err != nil {
		return err
	}
	// In `replace`, direct media embedding stands in for STT→text, so skip the
	// transcript when audio media chunks were produced (SPEC 8.1.7). `augment`
	// and `off` keep the transcript path unchanged.
	skipTranscript := s.embedMultimodal == "replace" && mediaProduced
	if skipTranscript {
		return nil
	}

	return s.generateTranscriptOrSidecar(ctx, doc, content, forceReindex)
}

// generateTranscriptOrSidecar resolves a media document's transcript. Subtitle
// sidecar precedence (§8.6.4): a subtitle file (.vtt/.srt/.ttml) next to the
// media is ingested AS the transcript instead of running STT — an authored
// transcript is authoritative. `--force`/reindex overrides the gate, retiring
// any stale sidecar transcripts and re-running STT. Sidecar ingestion bypasses
// the quality gate (authored, not model-derived; §8.6.6/§8.6.7).
func (s *Service) generateTranscriptOrSidecar(ctx context.Context, doc model.Document, content []byte, forceReindex bool) error {
	if !forceReindex {
		ingested, err := s.ingestSidecarTranscripts(ctx, doc)
		if err != nil {
			return err
		}
		if ingested {
			return nil
		}
	} else if err := s.retireStaleSidecarTranscripts(ctx, doc); err != nil {
		return err
	}

	if doc.DocType != "audio" || s.transcriber == nil {
		return nil
	}
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
	return nil
}

// retireStaleSidecarTranscripts tombstones a media document's existing
// sidecar-sourced transcript representations before a forced STT reindex
// regenerates the transcript. Without this, the stale "transcript-<lang>" sidecar
// rows stay live and surface alongside the fresh STT transcript in
// retrieval/export (spec §8.6.4). It is a no-op for non-media docs, when the
// store lacks the optional retirer capability, or when the document has no
// sidecar transcripts (reported as os.ErrNotExist).
func (s *Service) retireStaleSidecarTranscripts(ctx context.Context, doc model.Document) error {
	if !isSidecarMediaType(doc.DocType) {
		return nil
	}
	retirer, ok := s.store.(sidecarTranscriptRetirer)
	if !ok {
		return nil
	}
	n, err := retirer.SoftDeleteSidecarTranscripts(ctx, doc.RelPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("retire stale sidecar transcripts for %s: %w", doc.RelPath, err)
	}
	if n > 0 {
		s.getLogger().Printf("retired %d stale sidecar transcript(s) for %s before STT reindex", n, doc.RelPath)
	}
	return nil
}

// persistTitleIfFound runs the title heuristic on the supplied text body and,
// if a title is extracted, re-upserts the document so the title column is
// populated. Failures here are non-fatal: an empty or unwritten title only
// degrades citation display, so we log and continue rather than aborting the
// ingest of an otherwise-successful document.
func (s *Service) persistTitleIfFound(ctx context.Context, doc model.Document, body string) {
	title := ExtractTitle(body)
	if title == "" || title == doc.Title {
		return
	}
	doc.Title = title
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		s.getLogger().Printf("persist title for %s: %v", doc.RelPath, err)
	}
}

// isUnexpectedStoreErr returns true when err is non-nil and is not a
// "not found" sentinel.  It is the idiomatic guard used throughout the ingest
// package to distinguish missing-record results from genuine store failures.
func isUnexpectedStoreErr(err error) bool {
	return err != nil && !isNotFoundError(err)
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
	if s.repGen == nil || s.extractor == nil {
		return nil
	}

	// Structured path: when the extractor exposes a DoclingDocument, preserve
	// its structure (reading order, section breadcrumb, page/bbox provenance)
	// as region spans. Falls through to the flat path when the extractor is not
	// structured, or yields no parseable structure (custom --to md command).
	if se, ok := s.extractor.(structuredExtractor); ok {
		res, err := s.readOrComputeStructured(ctx, doc, content, se)
		if err == nil && len(res.Blocks) > 0 {
			return s.persistStructuredRepresentation(ctx, doc, res)
		}
	}

	ocrText, err := s.readOrComputeOCR(ctx, doc, content)
	if err != nil {
		return err
	}

	ocrText = strings.TrimSpace(ocrText)
	if ocrText == "" {
		return nil
	}

	s.persistTitleIfFound(ctx, doc, ocrText)

	decision := s.screenOutputQuality(doc.RelPath, "ocr", ocrText, quality.Context{
		Modality: quality.ModalityOCR,
	})

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeExtractedMarkdown,
		RepHash:     computeRepHash([]byte(ocrText)),
		MetaJSON:    s.extractionMetaJSON(),
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
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, decision); err != nil {
		return fmt.Errorf("persist ocr chunks: %w", err)
	}
	return nil
}

// persistStructuredRepresentation stores the extracted_markdown representation
// from a structured extraction: the rendered Markdown is the representation
// text (rep_hash is computed over it, as in the flat path), while the document
// structure is carried as region spans on section-aware chunks. The title from
// the document's title element is preferred over the text heuristic.
func (s *Service) persistStructuredRepresentation(ctx context.Context, doc model.Document, res StructuredExtraction) error {
	md := strings.TrimSpace(res.Markdown)
	if md == "" {
		return nil
	}

	if title := strings.TrimSpace(res.Title); title != "" {
		s.persistTitle(ctx, doc, title)
	} else {
		s.persistTitleIfFound(ctx, doc, md)
	}

	decision := s.screenOutputQuality(doc.RelPath, "ocr", md, quality.Context{
		Modality: quality.ModalityOCR,
	})

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeExtractedMarkdown,
		RepHash:     computeRepHash([]byte(md)),
		MetaJSON:    s.extractionMetaJSON(),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert structured representation: %w", err)
	}

	segments := chunkStructuredBlocks(res.Blocks)
	if len(segments) == 0 {
		return nil
	}
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, decision); err != nil {
		return fmt.Errorf("persist structured chunks: %w", err)
	}
	return nil
}

// readOrComputeStructured returns the structured extraction for content,
// caching the result as JSON under <state>/cache/docling keyed by content hash
// so re-indexing does not re-run docling. The cache is an implementation
// detail (spec §7.4.B: the raw DoclingDocument is not a representation).
func (s *Service) readOrComputeStructured(ctx context.Context, doc model.Document, content []byte, se structuredExtractor) (StructuredExtraction, error) {
	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "docling")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return StructuredExtraction{}, fmt.Errorf("create docling cache dir: %w", err)
	}
	cachePath := filepath.Join(cacheDir, s.ocrCacheKey(content)+".json")
	if cached, err := os.ReadFile(cachePath); err == nil {
		var res StructuredExtraction
		if json.Unmarshal(cached, &res) == nil && len(res.Blocks) > 0 {
			return res, nil
		}
	}

	res, err := se.ExtractStructured(ctx, doc.RelPath, content)
	if err != nil {
		return StructuredExtraction{}, err
	}
	if encoded, mErr := json.Marshal(res); mErr == nil {
		if wErr := os.WriteFile(cachePath, encoded, 0o644); wErr != nil {
			s.getLogger().Printf("cache structured extraction for %s: %v", doc.RelPath, wErr)
		}
	}
	return res, nil
}

// persistTitle sets documents.title to an explicit title (e.g. from a
// structured document's title element), preferring it over the heuristic.
// Non-fatal: a failed write only degrades citation display.
func (s *Service) persistTitle(ctx context.Context, doc model.Document, title string) {
	title = strings.TrimSpace(title)
	if title == "" || title == doc.Title {
		return
	}
	doc.Title = title
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		s.getLogger().Printf("persist title for %s: %v", doc.RelPath, err)
	}
}

func (s *Service) extractionMetaJSON() string {
	if s == nil || s.extractor == nil {
		return ""
	}
	prov, modelName := s.extractorProviderModel()
	meta := map[string]string{"provider": prov}
	if modelName != "" {
		meta["model"] = modelName
	}
	switch ex := s.extractor.(type) {
	case *doclingExtractor:
		meta["command"] = strings.TrimSpace(ex.commandTemplate)
	case *doclingServeExtractor:
		// Sanitized (scheme/host/path only): this is persisted per-document, so
		// any userinfo/query in the URL must not become durable metadata.
		meta["serve_url"] = SanitizeServeURL(ex.baseURL)
	case *mistral.Client:
		// provider/model already set from extractorProviderModel.
	default:
		meta["type"] = fmt.Sprintf("%T", s.extractor)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// extractorProviderModel returns the provider name and model recorded for the
// active OCR/extraction backend, the structured fields that form its OCR
// derivation identity (§8.6.7). model is empty for backends that have no model
// concept (docling / docling-serve / custom commands), so their identity is
// provider-only and stable across runs. Returns ("","") when no extractor is
// configured.
func (s *Service) extractorProviderModel() (providerName string, modelName string) {
	if s == nil || s.extractor == nil {
		return "", ""
	}
	switch ex := s.extractor.(type) {
	case *doclingExtractor:
		return "docling", ""
	case *doclingServeExtractor:
		return "docling-serve", ""
	case *mistral.Client:
		return "mistral", strings.TrimSpace(ex.DefaultOCRModel)
	default:
		return "custom", ""
	}
}

// GenerateOCRMarkdownRepresentation exposes OCR representation generation for tests.
func (s *Service) GenerateOCRMarkdownRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	return s.generateOCRMarkdownRepresentation(ctx, doc, content)
}

// quarantineDecision carries the result of screening generated output through
// the quality gate (spec 0.16.0). When quarantine is true the chunks for the
// representation must be inserted already-failed (embedding_status=error with
// category quality_gate) so the embedding worker never picks them up; the
// embErr/category fields are the content-free values to persist.
type quarantineDecision struct {
	quarantine bool
	embErr     string
	category   string
}

// screenOutputQuality runs the quality gate over generated text. It returns a
// zero-value (non-quarantine) decision when the gate is disabled (nil) or the
// verdict is clean, so callers can always insert via the returned decision. On
// a failed gate it logs a content-free warning and returns the quarantine
// values. relPath/kind are used only for the diagnostic log line.
func (s *Service) screenOutputQuality(relPath, kind string, text string, qctx quality.Context) quarantineDecision {
	if s.qualityGate == nil {
		return quarantineDecision{}
	}
	verdict := s.qualityGate.Evaluate(text, qctx)
	if verdict.OK() {
		return quarantineDecision{}
	}
	primary := verdict.Primary()
	var reason quality.Reason
	var detail string
	if primary != nil {
		reason = primary.Reason
		detail = primary.Detail
	}
	// Detail is already redacted/content-free; SanitizeReason bounds/normalizes
	// it for persistence into chunks.embedding_error.
	embErr := store.SanitizeReason(detail)
	s.getLogger().Printf("quality gate quarantined %s output for %s: reason=%s", kind, relPath, reason)
	return quarantineDecision{
		quarantine: true,
		embErr:     embErr,
		category:   string(store.ErrorCategoryQualityGate),
	}
}

// transcriptExpectedLanguage returns the active STT provider profile's language
// tag used to drive the quality gate's language detector, or "" when none is
// configured (in which case the language gate self-skips). The value is
// resolved from the same provider profile the transcriber uses (SPEC 8.1.3),
// not the legacy ElevenLabs-only field, so provider-profile setups (e.g.
// Mistral) supply the correct expected language.
func (s *Service) transcriptExpectedLanguage() string {
	return s.transcriptLanguage
}

func (s *Service) generateTranscriptRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	if s.repGen == nil || s.transcriber == nil {
		return nil
	}

	transcriptText, words, err := s.readOrComputeTranscriptWithWords(ctx, doc, content, "")
	if err != nil {
		return err
	}

	transcriptText = strings.TrimSpace(transcriptText)
	if transcriptText == "" {
		return nil
	}

	var duration time.Duration
	if d, derr := s.probeDuration(ctx, doc); derr == nil {
		duration = d
	}
	decision := s.screenOutputQuality(doc.RelPath, "transcript", transcriptText, quality.Context{
		Modality:         quality.ModalityTranscript,
		ExpectedLanguage: s.transcriptExpectedLanguage(),
		Duration:         duration,
	})

	segments := chunkTranscriptByTimeWithWordsFiltered(transcriptText, words, s.captionWordFilter())
	if len(segments) == 0 {
		return nil
	}
	// Optional model-driven speaker diarization (SPEC §8.6.8): when active and a
	// diarizer is injected, attribute each segment to a speaker. This is metadata
	// only — it stamps span.Speaker/SpeakerLabel and never changes chunk text or
	// span bounds. With no diarizer (the default), this is a no-op and the
	// transcript is un-attributed exactly as today (sidecar <v> attribution is
	// unaffected). Run BEFORE the meta is built so diarized/speakers reflect the
	// attribution that is actually present on the segments.
	s.applyDiarization(ctx, doc, content, segments)

	metaJSON, err := s.sttTranscriptMetaJSON(distinctSpeakers(segments))
	if err != nil {
		return fmt.Errorf("marshal transcript meta: %w", err)
	}
	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeTranscript,
		RepHash:     computeRepHash([]byte(transcriptText)),
		MetaJSON:    metaJSON,
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert transcript representation: %w", err)
	}

	// Optional leading-silence trim (dir2mcp#258): when enabled, subtract the
	// detected dead-air offset from every time span and word timestamp so the
	// transcript aligns to first speech. Disabled (default) leaves spans
	// untouched; ffmpeg absent / detection failure -> 0 offset -> no change. The
	// offset is captured so the SAME shift is applied to any translated
	// transcripts below, keeping source/translated time windows aligned.
	trimOffsetMS := 0
	if s.cfg.MediaTrimLeadingSilence {
		if offset := s.detectLeadingSilence(ctx, doc); offset > 0 {
			trimOffsetMS = int(offset.Milliseconds())
			shiftTranscriptSpans(segments, trimOffsetMS)
		}
	}
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, decision); err != nil {
		return fmt.Errorf("persist transcript chunks: %w", err)
	}

	// Optional translation step (SPEC §8.6.2): after the source transcript
	// representation is persisted, produce one additional per-language transcript
	// representation for each configured target language. This sits AFTER the
	// source transcript so a translation failure never blocks the authoritative
	// transcript, and each translated transcript routes through the same quality
	// gate (it IS model output, unlike sidecar transcripts which bypass it).
	//
	// Translation is a best-effort enrichment: any failure (chat provider error,
	// translated-rep persistence) is logged and counted but NOT propagated, so a
	// failed translation never marks the source transcript's document as errored.
	if err := s.translateTranscriptRepresentations(ctx, doc, content, transcriptText, duration, trimOffsetMS); err != nil {
		s.getLogger().Printf("transcript translation skipped for %s: %v", doc.RelPath, err)
		s.addErrors(1)
	}
	return nil
}

// applyDiarization stamps speaker attribution onto the transcript segments via
// the injected diarizer (SPEC §8.6.8). It self-skips when diarization is
// inactive or no diarizer is wired, leaving segments un-attributed (the default,
// byte-identical to a non-diarized transcript). It is best-effort and fail-open:
// a diarizer error degrades to a flat transcript rather than failing ingest
// (§8.6.8 degenerate-output rule). Attribution is metadata only — it sets
// span.Speaker/SpeakerLabel and never alters chunk text or span bounds.
func (s *Service) applyDiarization(ctx context.Context, doc model.Document, content []byte, segments []chunkSegment) {
	if !s.diarizeActive || s.diarizer == nil || len(segments) == 0 {
		return
	}
	in := make([]SpeakerSegment, 0, len(segments))
	for _, seg := range segments {
		in = append(in, SpeakerSegment{StartMS: seg.Span.StartMS, EndMS: seg.Span.EndMS, Text: seg.Text})
	}
	attrs, err := s.diarizer.Diarize(ctx, content, in)
	if err != nil {
		s.getLogger().Printf("diarization skipped for %s (degrading to flat transcript): %v", doc.RelPath, err)
		return
	}
	if len(attrs) != len(segments) {
		s.getLogger().Printf("diarization skipped for %s: diarizer returned %d attributions for %d segments",
			doc.RelPath, len(attrs), len(segments))
		return
	}
	for i := range segments {
		id := strings.TrimSpace(attrs[i].ID)
		if id == "" {
			continue // un-attributed segment degrades to flat
		}
		segments[i].Span.Speaker = id
		segments[i].Span.SpeakerLabel = strings.TrimSpace(attrs[i].Label)
	}
}

// translateTranscriptRepresentations produces, for each configured target
// language, a translated transcript representation that is time-aligned to the
// source transcript (SPEC §8.6.2). It self-skips (returns nil) when translation
// is disabled, no translator resolved, or no target languages are configured —
// so behaviour with translation off is identical to before. Each translated
// transcript: is cached per-language via TranscriptLangSuffix so it is not
// recomputed across re-ingests; carries a distinct rep_type via
// TranscriptRepType(lang) so it coexists with the source transcript and any
// sidecar per-language reps; routes through the output quality gate; and records
// source_language + translate provider/model in meta_json.
func (s *Service) translateTranscriptRepresentations(ctx context.Context, doc model.Document, content []byte, sourceText string, duration time.Duration, trimOffsetMS int) error {
	if s.translator == nil || len(s.translateTargetLangs) == 0 {
		return nil
	}
	sourceLang := strings.TrimSpace(s.transcriptLanguage)
	var firstErr error
	for _, lang := range s.translateTargetLangs {
		lang = strings.ToLower(strings.TrimSpace(lang))
		if lang == "" || sameLanguage(lang, sourceLang) {
			// Skip a no-op translation into the source language itself.
			continue
		}
		if err := s.translateOneTranscript(ctx, doc, content, sourceText, sourceLang, lang, duration, trimOffsetMS); err != nil {
			// Best-effort per language: a failure on one target must not suppress
			// the remaining targets. Log here and keep going; the first error is
			// returned so the caller can log/count it (non-fatally).
			s.getLogger().Printf("transcript translation failed for %s into %q: %v", doc.RelPath, lang, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}
	return firstErr
}

// sameLanguage reports whether two language tags name the same language for the
// purpose of skipping a no-op self-translation. The comparison is loose: a tag
// matches if it equals the other or shares its primary subtag (so "en" matches
// "en-US"). An empty source language never matches (auto-detected/unknown), so
// translation always proceeds when the source language was not pinned.
func sameLanguage(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	primary := func(s string) string {
		if i := strings.IndexAny(s, "-_"); i >= 0 {
			return s[:i]
		}
		return s
	}
	return primary(a) == primary(b)
}

// translateOneTranscript translates the source transcript into a single target
// language and persists it as a transcript representation. The translated text
// is cached per-language (TranscriptLangSuffix) keyed by the SOURCE content hash
// so re-ingesting an unchanged document reuses the cached translation instead of
// re-calling the chat provider.
func (s *Service) translateOneTranscript(ctx context.Context, doc model.Document, content []byte, sourceText, sourceLang, targetLang string, duration time.Duration, trimOffsetMS int) error {
	translated, err := s.readOrComputeTranslation(ctx, content, sourceText, targetLang)
	if err != nil {
		return fmt.Errorf("translate transcript for %s into %q: %w", doc.RelPath, targetLang, err)
	}
	translated = strings.TrimSpace(translated)
	if translated == "" {
		return nil
	}

	// A translated transcript is model output, so it routes through the output
	// quality gate exactly like an STT transcript (anti-hallucination). The
	// expected language is the TARGET language: the gate's language detector
	// should accept the translated text, not the source.
	decision := s.screenOutputQuality(doc.RelPath, "transcript-translation", translated, quality.Context{
		Modality:         quality.ModalityTranscript,
		ExpectedLanguage: targetLang,
		Duration:         duration,
	})

	meta := transcriptMeta{
		Source:            translationSource,
		Language:          targetLang,
		SourceLanguage:    sourceLang,
		TranslateProvider: s.translateProvider,
		TranslateModel:    s.translateModel,
		Timestamps:        true,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal translated transcript meta: %w", err)
	}

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     TranscriptRepType(targetLang),
		RepHash:     computeRepHash([]byte(translated)),
		MetaJSON:    string(metaJSON),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert translated transcript representation: %w", err)
	}
	s.addRepresentations(1)

	// Chunk the translated text with the same transcript chunker so its time
	// spans line up with the source segments (the translation preserves each
	// segment's verbatim [mm:ss] marker).
	segments := chunkTranscriptByTime(translated)
	if len(segments) == 0 {
		return nil
	}
	// Apply the same leading-silence trim offset as the source transcript so the
	// translated time windows stay aligned with the source (dir2mcp#258). A zero
	// offset (trim disabled / no silence detected) is a no-op.
	if trimOffsetMS > 0 {
		shiftTranscriptSpans(segments, trimOffsetMS)
	}
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, decision); err != nil {
		return fmt.Errorf("persist translated transcript chunks: %w", err)
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
		if upsertErr := s.repGen.upsertChunksForRepresentationWithStore(ctx, tx, jsonRepID, "text", chunkTextByChars(jsonText, annotationChunkSize, annotationChunkOverlap, annotationChunkMinSize), quarantineDecision{}); upsertErr != nil {
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
		if upsertErr := s.repGen.upsertChunksForRepresentationWithStore(ctx, tx, textRepID, "text", chunkTextByChars(flattened, annotationChunkSize, annotationChunkOverlap, annotationChunkMinSize), quarantineDecision{}); upsertErr != nil {
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
// cacheFileInfo records path and stat info for a single cache entry used by
// enforceCachePolicy and its eviction helpers.
type cacheFileInfo struct {
	path string
	info os.FileInfo
}

// evictOldCacheEntries removes entries whose modification time is before
// cutoff, returning the surviving entries and the updated total size.
func evictOldCacheEntries(files []cacheFileInfo, total int64, cutoff time.Time, cacheDir string) ([]cacheFileInfo, int64, error) {
	kept := make([]cacheFileInfo, 0, len(files))
	for _, f := range files {
		if f.info.ModTime().Before(cutoff) {
			if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
				return nil, 0, fmt.Errorf("prune cache ttl: remove %s in %s: %w", f.path, cacheDir, err)
			}
			total -= f.info.Size()
			continue
		}
		kept = append(kept, f)
	}
	return kept, total, nil
}

// evictBySizeLimit removes the oldest cache entries until total ≤ maxBytes.
func evictBySizeLimit(files []cacheFileInfo, total, maxBytes int64, cacheDir string) error {
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
	return nil
}

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

	var files []cacheFileInfo
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
		files = append(files, cacheFileInfo{path: p, info: info})
		total += info.Size()
	}

	// age-based eviction first
	if ttl > 0 {
		files, total, err = evictOldCacheEntries(files, total, now.Add(-ttl), cacheDir)
		if err != nil {
			return err
		}
	}

	// size-based eviction
	if maxBytes > 0 && total > maxBytes {
		return evictBySizeLimit(files, total, maxBytes, cacheDir)
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

	cachePath := filepath.Join(cacheDir, s.ocrCacheKey(content)+".md")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), nil
	}

	// enforce any configured cache policy around writes. A full directory scan
	// can be expensive, so we only run it on a configurable write interval.
	// The counter increments only when we are about to perform a real write
	// (cache miss), not for cache hits.
	ocrText, err := s.extractor.Extract(ctx, doc.RelPath, content)
	if err != nil {
		return "", fmt.Errorf("document extract %s: %w", doc.RelPath, err)
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

// TranscriptLangSuffix returns a safe filename suffix for the given language,
// used to key the transcript cache by content+language. Empty language returns
// an empty suffix so language-unaware callers share the same cache file.
func TranscriptLangSuffix(language string) string {
	l := strings.TrimSpace(language)
	if l == "" {
		return ""
	}
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, strings.ToLower(l))
	if safe == "" {
		safe = "unknown"
	}
	return "-" + safe
}

func (s *Service) readOrComputeTranscript(ctx context.Context, doc model.Document, content []byte, language string) (string, error) {
	text, _, err := s.readOrComputeTranscriptWithWords(ctx, doc, content, language)
	return text, err
}

// readOrComputeTranscriptWithWords is readOrComputeTranscript plus the optional
// per-word timing the provider returned (spec §8.6.1, #252). The segment text is
// the authoritative cache key/value (the .txt cache file); word timing is a
// best-effort sidecar (.words.json) carried only when the transcriber implements
// model.StructuredTranscriber. A missing or unreadable sidecar yields nil words
// — behaviour identical to a provider without word timing.
func (s *Service) readOrComputeTranscriptWithWords(ctx context.Context, doc model.Document, content []byte, language string) (string, []model.TimedWord, error) {
	if s.transcriber == nil {
		return "", nil, errors.New("transcriber not configured")
	}

	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "transcribe")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("create transcript cache dir: %w", err)
	}

	// Key the cache on the media bytes AND the active STT derivation identity
	// (SPEC §8.6.7), not the bytes alone: when the STT provider/model changes the
	// re-ingest gate forces re-transcription, and a bytes-only key would return
	// the previous model's cached text — silently defeating the re-derivation. The
	// TranscriptLangSuffix is retained on the filename so cache files stay
	// human-identifiable by language. With no resolved STT identity the historical
	// bytes-only key is preserved.
	base := s.transcriptCacheKey(content) + TranscriptLangSuffix(language)
	cachePath := filepath.Join(cacheDir, base+".txt")
	wordsPath := filepath.Join(cacheDir, base+".words.json")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), readCachedWords(wordsPath), nil
	}

	transcript, words, err := s.transcribe(ctx, doc, content)
	if err != nil {
		return "", nil, fmt.Errorf("%w: transcribe %s: %w", ErrTranscriptProviderFailure, doc.RelPath, err)
	}

	transcriptBytes := []byte(strings.ReplaceAll(strings.ReplaceAll(transcript, "\r\n", "\n"), "\r", "\n"))
	if err := os.WriteFile(cachePath, transcriptBytes, 0o644); err != nil {
		return "", nil, fmt.Errorf("write transcript cache: %w", err)
	}
	s.writeCachedWords(wordsPath, words)
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
	return string(transcriptBytes), words, nil
}

// transcribe calls the configured transcriber, preferring the structured
// (word-timing) capability when available. Providers that do not implement
// model.StructuredTranscriber return no words, leaving downstream behaviour
// unchanged.
func (s *Service) transcribe(ctx context.Context, doc model.Document, content []byte) (string, []model.TimedWord, error) {
	if st, ok := s.transcriber.(model.StructuredTranscriber); ok {
		res, err := st.TranscribeStructured(ctx, doc.RelPath, content)
		if err != nil {
			return "", nil, err
		}
		return res.Text, res.Words, nil
	}
	text, err := s.transcriber.Transcribe(ctx, doc.RelPath, content)
	if err != nil {
		return "", nil, err
	}
	return text, nil, nil
}

// readCachedWords loads per-word timing from the sidecar cache file, returning
// nil on any error (missing/corrupt sidecar degrades to no word timing).
func readCachedWords(path string) []model.TimedWord {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var words []model.TimedWord
	if err := json.Unmarshal(raw, &words); err != nil {
		return nil
	}
	return words
}

// writeCachedWords persists per-word timing alongside the transcript text. It is
// best-effort: a write failure is logged and ignored so word timing never blocks
// transcript ingest. No sidecar is written when there are no words.
func (s *Service) writeCachedWords(path string, words []model.TimedWord) {
	if len(words) == 0 {
		return
	}
	raw, err := json.Marshal(words)
	if err != nil {
		s.getLogger().Printf("marshal transcript words cache: %v", err)
		return
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		s.getLogger().Printf("write transcript words cache (%s): %v", path, err)
	}
}

// ReadOrComputeTranscript exposes transcript cache lookup/computation for tests.
func (s *Service) ReadOrComputeTranscript(ctx context.Context, doc model.Document, content []byte, language string) (string, error) {
	return s.readOrComputeTranscript(ctx, doc, content, language)
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
