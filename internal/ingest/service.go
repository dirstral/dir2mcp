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
	"github.com/dirstral/dir2mcp/internal/statefs"
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

	// transcriberProviderAuto/Off are the two selectors handled directly by the
	// STT resolvers; every other explicit selector (mistral/elevenlabs/whisper/
	// openai/gemini) maps to a built-in profile via config.STTSelectorProfile,
	// the single shared selector->profile table (issue #440 F6).
	transcriberProviderAuto = "auto"
	transcriberProviderOff  = "off"
)

type Service struct {
	cfg           config.Config
	store         model.Store
	indexingState *appstate.IndexingState
	repGen        *RepresentationGenerator
	extractor     model.DocumentExtractor
	// pandocExtractor is the capability-activated pandoc engine (T2, #393). Under
	// `ingest.extractor: auto` it runs as a SECOND active engine alongside the
	// primary s.extractor (e.g. docling handles .docx, pandoc handles .odt); under
	// the `pandoc` pin it is also the primary. It is nil under any non-pandoc pin
	// (docling/docling-serve/mistral/off) or when no functional pandoc resolves.
	pandocExtractor *pandocExtractor
	transcriber     model.Transcriber
	// recognizer is the optional recognition binding (design 0004): a backend
	// that turns video into time-ranged annotation statements. Nil when
	// recognize.provider is off (the default).
	recognizer model.Recognizer
	// recognizeBackendPID is the managed backend child's pid (0 when dir2mcp
	// did not launch one); recognizeHealthWait overrides the startup health
	// deadline (tests).
	recognizeBackendPID int
	recognizeHealthWait time.Duration

	// onUnsupported is the resolved §7.4.B.2 degradation mode for a format no
	// active extraction engine supports (ingest.on_unsupported): "lenient"
	// (default) skips with a warning and surfaces the gap in the honest coverage
	// report, while "strict" records a non-fatal per-document UNSUPPORTED_FORMAT
	// error. Resolved once in NewService; empty is treated as lenient.
	onUnsupported string

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

	// sttLanguages is the resolved STT profile's DECLARED language coverage
	// (SPEC §8.2.1, #566): a non-empty set asserts which BCP-47 languages the
	// model transcribes well. Empty = open/unknown (no assertion). It drives the
	// honest-coverage warning at construction and the transcript meta's
	// language_covered flag; it is never part of the derivation identity.
	sttLanguages []string

	// onUncoveredLanguage is the resolved media.stt.on_uncovered_language action
	// (SPEC §8.2.1, #566): "warn" (default, fail-open) transcribes an
	// uncovered-language item anyway and records the fact; "skip" records the item
	// as documents.status="skipped" with skip_reason="language_uncovered" instead
	// of transcribing to degraded output. Resolved once from config in NewService.
	onUncoveredLanguage string

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

	// translateProvider/translateModel record the resolved provider/model used
	// for translation so each translated transcript representation can carry its
	// translation derivation identity in meta_json (SPEC §5.2/§8.6.7). For the
	// chat engine these are the chat provider/model; for the whisper engine they
	// are the STT provider/model.
	translateProvider string
	translateModel    string

	// translateEngine selects how transcripts are translated (config
	// media.translate.engine): "chat" (default, line-by-line via s.translator) or
	// "whisper" (native audio->English translate task via s.translateSTT).
	translateEngine string
	// translateSTT runs Whisper's translate task; set only when
	// translateEngine == "whisper", nil for the chat engine.
	translateSTT model.Transcriber

	// summarizer is the chat/generation client used to derive document-level
	// `summary` representations for hierarchical retrieval (SPEC §5.2/§9.7). It is
	// the chat capability binding resolved at construction — honoring an explicit
	// `retrieval.hierarchical.provider` pin — or nil when hierarchical retrieval is
	// disabled (the default) or no chat-capable provider resolves, in which case
	// the optional summary step self-skips (capability-driven, fail-open).
	summarizer model.Generator

	// summaryProvider/summaryModel record the resolved generator identity so each
	// summary representation can carry it in meta_json (§5.2) and in the summary
	// derivation identity (§8.6.7); a generator swap then re-derives the summary.
	// Empty when summary generation is inactive.
	summaryProvider string
	summaryModel    string

	// contextualActive reports whether contextual retrieval (SPEC §8.1.8, issue
	// #330) is EFFECTIVELY on for this service: enabled in config AND a chat
	// generator was successfully bound. False for both "disabled" and the
	// capability fail-open, matching the effective mode the embed identity
	// records. It gates the fallback-chunk retry gate; the contextualizer itself
	// lives on repGen.
	contextualActive bool

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

	// ExtractAudioTrackFunc overrides extraction of a video's audio track before it
	// is transcribed (issue #495). Defaults to avutil.ExtractAudioTrack (ffmpeg)
	// when nil; tests set it to supply deterministic audio bytes (or
	// avutil.ErrNoAudioStream) without requiring the ffmpeg binary.
	ExtractAudioTrackFunc func(ctx context.Context, path string) ([]byte, error)

	// ExtractAudioTrackIndexFunc overrides extraction of a SPECIFIC audio stream
	// (0-based audio-relative index) for per-track transcription (SPEC §8.6.12,
	// issue #567). Defaults to avutil.ExtractAudioTrackIndex (ffmpeg `-map 0:a:<N>`)
	// when nil; tests set it to supply deterministic per-track audio bytes (or
	// avutil.ErrNoAudioStream for a track past the count) without the ffmpeg binary.
	ExtractAudioTrackIndexFunc func(ctx context.Context, path string, audioIndex int) ([]byte, error)

	// ProbeMediaInfoFunc overrides the container/stream probe used to detect
	// multi-track audio (issue #567). Defaults to avutil.ProbeMediaInfo (ffprobe)
	// when nil; tests set it to supply a deterministic stream census without the
	// ffprobe binary. It is best-effort: any error leaves the transcript untouched
	// and simply suppresses the multi-track diagnostic.
	ProbeMediaInfoFunc func(ctx context.Context, path string) (avutil.MediaInfo, error)

	// ArchiveMaxMembers and ArchiveMaxTotalBytes bound archive fan-out to contain
	// decompression bombs (#408): the number of members ingested per archive and
	// the aggregate uncompressed bytes buffered. A value <= 0 uses the package
	// defaults (archiveMaxMembers / archiveMaxTotalBytes). Exposed so tests can
	// exercise the caps without building multi-hundred-MiB fixtures.
	ArchiveMaxMembers    int
	ArchiveMaxTotalBytes int64

	// MaxMediaChunksPerDoc bounds the number of direct-embedding media chunks
	// (PDF pages or A/V time windows) generated for one document (#408). A value
	// <= 0 uses the package default (maxMediaChunksPerDoc). Exposed so tests can
	// exercise the cap without huge media inputs.
	MaxMediaChunksPerDoc int

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

	// optional callback invoked for every non-fatal per-document failure, after
	// the message has been secret-redacted. The CLI uses it to emit the
	// spec-required per-document `file_error` NDJSON event (SPEC §3.2, #414);
	// without it a failed document is only observable by polling the store.
	onDocumentError   func(relPath, docType, message string)
	onDocumentErrorMu sync.RWMutex

	// optional callback invoked for every document this run leaves never-indexed
	// (status "skipped"/"secret_excluded"). The CLI uses it to emit the
	// spec-required per-document `file_skip` NDJSON event (SPEC §3.2, #414). It
	// is the streaming counterpart of the durable skip_reasons aggregate.
	onDocumentSkip   func(relPath, docType, reason string)
	onDocumentSkipMu sync.RWMutex

	// optional callback invoked with a document's rel_path and the content_hash
	// its row now holds, every time that row is durably written. The CLI uses it
	// to keep retrieval-time cross-file dedup (SPEC §9.2) on live state instead
	// of a startup-only snapshot (#691). An empty hash means "not known good
	// right now" (withheld #402 done marker, or a failed document), which makes
	// the consumer forget the path rather than group on a stale hash.
	onDocumentContentHash   func(relPath, contentHash string)
	onDocumentContentHashMu sync.RWMutex
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

	// batch holds the optional per-scan batch-ergonomics state (SPEC §8.6.11):
	// the JSONL run manifest writer and the side-channel progress reporter. It is
	// created in runScan only when a media.batch feature is enabled and torn down
	// when the scan ends; nil otherwise, so the default ingest path is unchanged.
	batch *batchRun

	// activeOutcome is the manifest/progress accumulator for the asset currently
	// being processed. runScan sets it around each per-asset processing call (the
	// scan loop is sequential, so at most one asset is in flight); the rep
	// generators stamp produced outputs onto it via recordOutput. Nil when no
	// batch run is active, making recordOutput a no-op on the hot path.
	activeOutcome *assetOutcome

	// pendingOversize accumulates files dropped at discovery for exceeding the
	// ingest size cap (#497). They never become assets in the scan loop, so their
	// canonical §14.4 FILE_TOO_LARGE manifest entries are emitted once, after the
	// batch run is created. Populated by the OnOversize discovery callback.
	pendingOversize []string

	// quarantinedThisDoc records that the output quality gate (§8.6.6) marked the
	// document currently being processed as a non-fatal per-document error, so the
	// scan loop must count it as exactly one error and NOT credit it as indexed
	// (issue #426). Like activeOutcome it is per-document state: the scan loop is
	// sequential (at most one asset in flight). The authoritative reset is at
	// processDocument entry so every document starts clean by construction on every
	// path; generateRepresentations additionally resets it to re-scope the flag for
	// sequential archive members (separate processDocumentFromContent calls under a
	// single processDocument entry). A rejected representation is thus counted once
	// even when a document produces several (transcript + per-language translations).
	quarantinedThisDoc bool

	// docSecretPatterns holds the compiled security.secret_patterns set for the
	// document currently being processed. It is the SAME slice the caller passes
	// down the raw-byte path, captured at the document entry points
	// (processDocument / processDocumentFromContent) by beginDocumentSecretScope,
	// so the source scan and the derived-text scan can never disagree about which
	// patterns are active. It exists because the derived-text producers sit many
	// frames below those entry points (OCR, pandoc, docling, STT, translation,
	// annotation, recognition, sidecar) and threading one more argument through all
	// of them would say nothing the existing per-document Service state does not
	// already say: the scan loop is sequential, at most one asset is in flight.
	docSecretPatterns []*regexp.Regexp

	// secretExcludedThisDoc records that a DERIVED text of the document currently
	// being processed matched a configured secret pattern (#681), so the document
	// is withheld as status="secret_excluded" and must count as exactly one skip,
	// never as indexed. Per-document state with the same lifetime and the same
	// sequential-scan justification as quarantinedThisDoc; reset by
	// beginDocumentSecretScope at each document entry point.
	secretExcludedThisDoc bool

	// reconcileOutputs arms the per-document output-set reconciliation (#692) for
	// the scan currently running. It is set once per scan by
	// beginOutputReconciliation, when the recorded pipeline output identity no
	// longer matches the active one (or the operator forced a reindex), and read
	// by reconcileDocumentOutputs on every asset the scan visits. Keeping it as
	// per-run state is what makes a steady-state scan free: with no pipeline
	// change there is one settings read for the whole run and no per-document
	// query at all.
	reconcileOutputs bool

	// pendingOutputIdentity holds the pipeline output identity (#692) this scan is
	// entitled to record when it completes. It is set only when the recorded value
	// was read successfully, so a scan that could not compare identities records
	// nothing and leaves the comparison to the next run.
	pendingOutputIdentity string

	// deferGateDocError suppresses the EAGER per-document status=error marking that
	// the output quality gate normally applies (recordQualityGateDocError), scoping a
	// gate rejection to the failing TRACK instead of the whole document (SPEC
	// §8.6.12). It is set only while transcribing a multi-track selection (all/list):
	// there a rejected track's transcript is dropped and recorded as honest coverage,
	// and the document is marked error only if EVERY selected track failed — that
	// final decision is made by the per-track orchestrator, not per gate call. The
	// chunk-level quarantine encoded in the returned decision is unaffected. It is
	// per-document/per-pass state (the scan loop is sequential) and is always reset to
	// false after the multi-track loop.
	deferGateDocError bool

	// activePass selects which work the representation generators perform for the
	// asset currently being processed under the optional two-phase pass split
	// (SPEC §8.6.11). runScan sets it around each per-asset call (the scan loop is
	// sequential). passSingle (the zero value) is the default single-pass pipeline
	// — byte-identical to before the split existed. passTranscription produces
	// every representation EXCEPT translated transcripts; passDerivation produces
	// ONLY translated transcripts from the already-computed (cached) source
	// transcript. The split changes ordering and reporting only, never the final
	// representations/chunks/embeddings/citations.
	activePass processPass

	// runSkipReasons counts path-excluded drops by skip_reason for the CURRENT
	// scan only. Path-excludes can be numerous (a broad glob can drop thousands
	// of files), so — unlike archive/secret/size-cap skips — they are NOT
	// persisted as documents rows; they live here for the run and are surfaced
	// via SkipReasonCounts() merged into the reindex summary. This makes them a
	// best-effort, in-process signal: the count is NOT durable across a restart
	// and is not visible to a separate `status` process reading the store.
	// Reset at the start of each runScan.
	runSkipReasons   map[string]int64
	runSkipReasonsMu sync.Mutex
}

// processPass is the per-asset work selector for the optional two-phase pass
// split (SPEC §8.6.11). The two ordered passes together produce exactly the same
// representations as a single pass; each pass is independently resumable via the
// existing identity/cache state (§7.6/§8.6.7).
type processPass int

const (
	// passSingle runs the full per-document pipeline (transcription THEN
	// derivation) in one go. It is the default and is observably identical to the
	// pipeline before the two-phase split was introduced.
	passSingle processPass = iota
	// passTranscription runs every representation step EXCEPT translated
	// transcripts: raw text, OCR/extracted markdown, direct media chunks, and the
	// source (STT/sidecar) transcript with its chunks.
	passTranscription
	// passDerivation runs ONLY the derivation step (translation §8.6.2) for media
	// documents, reusing the cached source transcript so it never re-transcribes.
	passDerivation
)

// passLabel maps a pass to its manifest/progress label (SPEC §8.6.11). The
// single pass carries no label so its manifest records stay identical to the
// pre-split single-pass output.
func (p processPass) label() string {
	switch p {
	case passTranscription:
		return "transcription"
	case passDerivation:
		return "derivation"
	default:
		return ""
	}
}

// recordContentHash stamps the resolved content_hash (§7.6) onto the asset
// currently being processed under an active batch run, for the run manifest (SPEC
// §8.6.11). No-op when no batch run is active.
func (s *Service) recordContentHash(hash string) {
	if s == nil {
		return
	}
	s.activeOutcome.setContentHash(hash)
}

// markActiveSkipped marks the asset currently being processed under an active
// batch run as "skipped" — no work performed (cache hit, unchanged content, or a
// non-ingestable type), SPEC §8.6.11. No-op when no batch run is active.
func (s *Service) markActiveSkipped() {
	if s == nil {
		return
	}
	s.activeOutcome.markSkipped()
}

// markActiveErrored records a terminal error outcome — the canonical §14.4 code
// and a content-free message — on the asset currently being processed under an
// active batch run, for the run manifest (SPEC §8.6.11). The first code recorded
// wins (markErrorIfUnset), so a specific inner failure is never clobbered by a
// later one. No-op when no batch run is active.
func (s *Service) markActiveErrored(code, message string) {
	if s == nil {
		return
	}
	s.activeOutcome.markErrorIfUnset(code, message)
}

// ErrTranscriptProviderFailure marks failures originating from the transcript
// provider call itself (as opposed to persistence/cache write failures).
var ErrTranscriptProviderFailure = errors.New("transcript provider failure")

// ErrOCRProviderFailure marks failures originating from the OCR/document
// extraction provider call itself (as opposed to persistence/cache write
// failures). It is classified as the canonical §14.4 OCR_FAILED code on the run
// manifest (manifestErrorCode), distinct from the generic EXTRACT_FAILED.
var ErrOCRProviderFailure = errors.New("ocr provider failure")

// ErrTranslateProviderFailure marks failures originating from the translation
// provider call itself — either the chat engine or Whisper's native translate
// task (as opposed to persistence/cache write failures). It is classified as the
// canonical §14.4 TRANSLATE_FAILED code on the run manifest (manifestErrorCode),
// distinct from the transcript's TRANSCRIBE_FAILED.
var ErrTranslateProviderFailure = errors.New("translation provider failure")

// ErrRecognitionProviderFailure marks failures originating from the recognition
// backend call itself (design 0004) — as opposed to persistence/cache write
// failures. Wrapped at GenerateRecognitionRepresentation so manifestErrorCode
// classifies a transient recognize-backend failure as RECOGNIZE_FAILED rather
// than the generic EXTRACT_FAILED, mirroring the STT/OCR provider sentinels.
var ErrRecognitionProviderFailure = errors.New("recognition provider failure")

// ErrFileTooLarge marks a document rejected because its size exceeds the ingest
// size cap (§14.4). Wrapped at the size-check sites so manifestErrorCode
// classifies it as the canonical FILE_TOO_LARGE code rather than the generic
// EXTRACT_FAILED.
var ErrFileTooLarge = errors.New("file too large")

// errBinaryOnRawTextPath marks a document that classified into a text-oriented
// doc type (SPEC §7.4) but whose bytes are binary — most notably a .parquet file,
// which classifies as "data". Indexing it as raw text would normalize the binary
// bytes to U+FFFD replacement-character soup and chunk/embed the garbage, so it is
// skipped and recorded as a durable diagnostic instead (#398).
var errBinaryOnRawTextPath = errors.New(
	"binary content on the raw-text path; not indexed as text (e.g. Parquet or another binary file with a text-classified extension)")

// errNoVideoRepresentation marks a video document that produced zero
// representations: no subtitle sidecar (.vtt/.srt/.ttml) was found next to it, no
// transcript was derived (speech-to-text is off/unconfigured, or the video has no
// audio track to transcribe — issue #495), multimodal keyframe embedding is off,
// and recognition produced nothing either (recognize.provider=off, or the backend
// returned no annotations — #622). Such a video is known but unsearchable, so it
// is surfaced as a durable non-fatal diagnostic instead of a silent no-op (#398).
var errNoVideoRepresentation = errors.New(
	"video produced no representation: no subtitle sidecar found, no transcript was derived " +
		"(speech-to-text is off/unconfigured or the video has no audio track), multimodal keyframe embedding is off, " +
		"and no recognition annotations were produced — configure a speech-to-text provider, enable embed_multimodal, " +
		"configure a recognition backend (recognize.provider), or provide a .vtt/.srt sidecar to make it searchable")

// activeExtractionAvailability derives which extraction engines are active for
// this run (§7.4.B "Extractor availability") from the resolved extractors. The
// primary s.extractor is the best-available engine chosen by the docling →
// docling-serve → mistral cascade (DescribeDocumentExtractor) — structured
// (docling family), flat OCR, or pandoc when it is the only engine. pandoc (T2,
// #393) may additionally be active as a SECOND engine (s.pandocExtractor) under
// `auto`. Deriving availability from the wired extractors keeps the per-format
// selection in lockstep with the engines the service will actually run.
func (s *Service) activeExtractionAvailability() extractionAvailability {
	avail := extractionAvailability{}
	switch s.extractor.(type) {
	case nil:
		// No primary extractor; only pandoc (if wired) may be active.
	case *pandocExtractor:
		// Primary is pandoc (the `pandoc` pin or the auto-only-pandoc case);
		// avail.pandoc is set below from s.pandocExtractor.
	default:
		if _, structured := s.extractor.(structuredExtractor); structured {
			avail.structured = true
		} else {
			avail.flatOCR = true
		}
	}
	if s.pandocExtractor != nil {
		avail.pandoc = true
	}
	return avail
}

// ActivatePandocEngine wires the capability-activated pandoc engine (T2, #393) as
// an active extraction engine when cfg permits it (ingest.extractor auto or the
// pandoc pin) AND a functional pandoc resolves. Under `auto` it runs as a second
// engine alongside the primary s.extractor (e.g. docling handles .docx, pandoc
// handles .odt); under the `pandoc` pin the primary IS pandoc, so this reuses that
// same instance rather than building a second one. It is called by the CLI
// ingestor wiring right after the primary extractor is set; NewService itself
// stays free of any ambient-PATH probe so unit tests that inject a single
// extractor are unaffected by whether a pandoc binary happens to be installed.
func (s *Service) ActivatePandocEngine(cfg config.Config) {
	if !pandocEngineActive(cfg) {
		s.pandocExtractor = nil
		return
	}
	if pe, ok := s.extractor.(*pandocExtractor); ok {
		s.pandocExtractor = pe
		return
	}
	s.pandocExtractor = NewPandocExtractor(cfg.IngestPandocCommand)
}

// routeExtractionExt resolves the capability-aware, per-format extraction route
// (§7.4.B.1 / §7.4.A) for a lowercased extension under the active
// `ingest.extractor` policy and the currently-active engines. It is the single
// routing decision both the extracted-markdown path and the html markup-boundary
// (#556) consult.
func (s *Service) routeExtractionExt(ext string) extractionRoute {
	return selectExtractionRoute(s.cfg.IngestExtractor, s.activeExtractionAvailability(), ext)
}

// extractorCanReadExt reports whether the currently-selected extractor can read
// the asset's format for the extracted-markdown path (pdf/image/document),
// consulting the per-format selection (§7.4.B.1) rather than a coarse
// structured/flat switch. It is true only when the active engine is routed to
// extract the format (structured or flat OCR); a format that must degrade — or
// that routes to the raw_text baseline (html) — is not "readable" on this path.
func (s *Service) extractorCanReadExt(relPath string) bool {
	// Routing-aware, mirroring CanExtractSourceText: pandoc (T2, #393) can be the
	// only active extraction engine (primary s.extractor nil, s.pandocExtractor
	// set), so gate on whether ANY engine is active rather than on the primary
	// alone — otherwise a pandoc-routed born-digital format would be wrongly
	// treated as unreadable at index time.
	if s.extractor == nil && s.pandocExtractor == nil {
		return false
	}
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	switch s.routeExtractionExt(ext) {
	case routeStructured, routePandoc, routeFlatOCR:
		return true
	default:
		return false
	}
}

// extractorLabel is a short human name for the active extractor, used in the
// #394 unsupported-format diagnostic. It reflects the same structured/flat split
// extractorCanReadExt uses, not the concrete provider profile name.
func (s *Service) extractorLabel() string {
	if _, ok := s.extractor.(*pandocExtractor); ok {
		return "pandoc"
	}
	if _, structured := s.extractor.(structuredExtractor); structured {
		return "docling"
	}
	if s.extractor == nil && s.pandocExtractor != nil {
		return "pandoc"
	}
	return "the OCR provider"
}

// unsupportedExtractionErr builds the durable, non-fatal diagnostic for a
// pdf/image/document asset whose format the active extractor cannot read (#394).
// Before #394 such a format was handed to the extractor anyway, which either
// failed silently (docling → empty output → a silent empty rep) or hard-errored
// (Mistral OCR → "unsupported file extension"); it is now skipped with this
// clear message instead. Content support for the unreadable formats is #393.
func unsupportedExtractionErr(relPath, extractor string) error {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	return fmt.Errorf(
		"unsupported format for extraction: the active extractor (%s) cannot read %s files; "+
			"install a capable extractor or convert the file to a supported format (tracked in #393)",
		extractor, ext)
}

// onUnsupportedStrict/Lenient are the §7.4.B.2 degradation modes.
const (
	onUnsupportedLenient = "lenient"
	onUnsupportedStrict  = "strict"
)

// normalizeOnUnsupported maps the raw ingest.on_unsupported config value to the
// resolved §7.4.B.2 mode. Only "strict" selects the strict contract; everything
// else (including empty and any unrecognized value) is the lenient default, so a
// service constructed from a bare config.Config degrades leniently — the
// backward-compatible, not-indexed-but-honest outcome.
func normalizeOnUnsupported(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), onUnsupportedStrict) {
		return onUnsupportedStrict
	}
	return onUnsupportedLenient
}

// SetOnUnsupported overrides the resolved §7.4.B.2 degradation mode, primarily
// for tests that need to exercise strict/lenient without threading a full config.
// The value is normalized (only "strict" is strict; anything else is lenient).
func (s *Service) SetOnUnsupported(mode string) {
	s.onUnsupported = normalizeOnUnsupported(mode)
}

// onUncoveredLanguageWarn / onUncoveredLanguageSkip are the resolved
// media.stt.on_uncovered_language actions (SPEC §8.2.1, #566). "skip" suppresses
// transcription for an uncovered source language; "warn" (and any other resolved
// value) is the fail-open default: transcribe anyway.
const (
	onUncoveredLanguageWarn = "warn"
	onUncoveredLanguageSkip = "skip"
)

// normalizeOnUncoveredLanguage maps the raw media.stt.on_uncovered_language config
// value to the resolved §8.2.1 action. Only "skip" (case-insensitive) selects the
// strict skip contract; everything else — including empty and any unrecognized
// value — is the fail-open "warn" default, so a service constructed from a bare
// config.Config transcribes as today (config.Validate rejects unknown values, but
// a Service built directly from an unvalidated config must still degrade safely).
func normalizeOnUncoveredLanguage(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), onUncoveredLanguageSkip) {
		return onUncoveredLanguageSkip
	}
	return onUncoveredLanguageWarn
}

// SetOnUncoveredLanguage overrides the resolved §8.2.1 honest-coverage floor
// action, primarily for tests that need to exercise warn/skip without threading a
// full config. Only "skip" is strict; anything else is the fail-open warn default.
func (s *Service) SetOnUncoveredLanguage(action string) {
	s.onUncoveredLanguage = normalizeOnUncoveredLanguage(action)
}

// sttLanguageUncovered reports whether the pinned STT source language trips the
// honest-coverage floor (SPEC §8.2.1): the finally-selected model declares a
// non-empty stt_languages set and the pinned language is outside it. It keys off
// the pinned source language (s.transcriptLanguage) and the routed profile's
// declared coverage (s.sttLanguages) — the same inputs as the Slice A
// construction-time warning — both resolved once in resolveTranscriptIdentityFields.
// It deliberately uses only the configured pin, never per-item auto-detection:
// langdetect is text-based and cannot know an item's language before it is
// transcribed, so a skip-before-transcribe decision can only rest on the pin.
// False when there is no pin or coverage is unknown (STTLanguageCoverageSet
// returns declared=false for a blank language or an empty coverage set).
func (s *Service) sttLanguageUncovered() bool {
	lang := strings.TrimSpace(s.transcriptLanguage)
	if lang == "" {
		return false
	}
	declared, covered := provider.STTLanguageCoverageSet(s.sttLanguages, lang)
	return declared && !covered
}

// skipUncoveredLanguageTranscript applies the SPEC §8.2.1 honest-coverage floor
// SKIP action (#566) for a media item about to be transcribed. It returns
// skip=false when transcription should proceed (warn mode, or a covered/unknown
// language); the caller then transcribes as today. It returns skip=true when the
// pinned source language is outside the selected STT model's declared coverage AND
// media.stt.on_uncovered_language=skip, in which case transcription is NOT run:
//
//   - When the item produced no other searchable representation (mediaProduced=false)
//     it is recorded as a durable status="skipped" with skip_reason="language_uncovered"
//     and credited to the skipped counter, so the gap surfaces as honest not-indexed
//     coverage in the skip_reasons aggregate instead of garbage the §8.6.6 quality gate
//     silently drops. suppressCredit is true so the caller does not also credit it as
//     indexed (#426).
//   - When the item already produced media chunks (embed_multimodal=augment) it stays
//     searchable via those, so it is NOT recorded as skipped — only the transcript is
//     suppressed and suppressCredit is false (the document is still indexed).
//
// It is reached only after the sidecar attempt returned no sidecar transcript, so a
// real subtitle track (not produced by the uncovered STT model) still wins.
func (s *Service) skipUncoveredLanguageTranscript(ctx context.Context, doc model.Document, mediaProduced bool) (skip, suppressCredit bool) {
	if s.onUncoveredLanguage != onUncoveredLanguageSkip || !s.sttLanguageUncovered() {
		return false, false
	}
	s.getLogger().Printf("stt: skipping transcription of %s (%s) — source language %q is outside %s's declared coverage %v (media.stt.on_uncovered_language=skip, SPEC §8.2.1)",
		doc.RelPath, doc.DocType, strings.TrimSpace(s.transcriptLanguage), s.sttProvider, s.sttLanguages)
	if mediaProduced {
		return true, false
	}
	s.addSkipped(1)
	s.persistNonFatalDocSkip(ctx, doc, model.SkipReasonLanguageUncovered)
	return true, true
}

// degradeUnsupportedExtraction applies the §7.4.B.2 degradation contract to a
// pdf/image/document asset whose format no active extraction engine can read,
// and which produced no direct-embedding media chunks either (so it would
// otherwise be unsearchable). It never hands the asset to an engine that cannot
// read it and never lets the gap be silent:
//
//   - strict: a non-fatal per-document UNSUPPORTED_FORMAT error (§7.7) —
//     documents.status=error, the error counter is bumped, a file_error fires.
//   - lenient (default): a **durable skip** (§7.4.B.2, #584) — documents.status=
//     skipped with an unsupported-format skip_reason, the skipped counter is bumped,
//     and a file_skip fires. The document is NOT indexed (it has no searchable
//     representation), so the coverage report (§7.7) names it durably — after a
//     restart, not just via an in-run counter — instead of leaving it status="ok"
//     (an unsearchable document mislabeled as indexed, the gap #584 closed).
//
// In BOTH modes the returned suppressCredit is true: the caller must not also
// credit the document as indexed (#426), because it is now recorded as either an
// error or a skip. (The error-vs-skip counting is done here, not by the caller, so
// returning true never double-counts.)
func (s *Service) degradeUnsupportedExtraction(ctx context.Context, doc model.Document, secretPatterns []*regexp.Regexp) (suppressCredit bool) {
	cause := unsupportedExtractionErr(doc.RelPath, s.extractorLabel())
	if s.onUnsupported == onUnsupportedStrict {
		s.getLogger().Printf("unsupported format (strict) for %s (%s): %v", doc.RelPath, doc.DocType, cause)
		s.addErrors(1)
		s.persistNonFatalDocError(ctx, doc, cause, secretPatterns)
		return true
	}
	// lenient: a durable, honest skip — not status="ok". This path is reached only
	// when no active engine can read the format AND no media chunks were produced,
	// so the document is genuinely unsearchable; record it as status="skipped" so
	// the coverage aggregate (§7.7) and a file_skip name it durably (#584).
	s.getLogger().Printf("unsupported format (lenient, durable skip) for %s (%s): %v", doc.RelPath, doc.DocType, cause)
	s.addSkipped(1)
	s.persistNonFatalDocSkip(ctx, doc, model.SkipReasonUnsupportedFormat)
	return true
}

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
		onUnsupported:                   normalizeOnUnsupported(cfg.IngestOnUnsupported),
		onUncoveredLanguage:             normalizeOnUncoveredLanguage(cfg.MediaSTTOnUncoveredLanguage),
	}
	transcriber, err := TranscriberFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("configure transcriber: %w", err)
	}
	svc.transcriber = transcriber
	if strings.EqualFold(strings.TrimSpace(cfg.RecognizeProvider), "serve") {
		svc.recognizer = NewRecognizeServeClient(cfg.RecognizeServeURL)
	}
	if rs, ok := store.(model.RepresentationStore); ok {
		svc.repGen = NewRepresentationGenerator(rs)
		// Best-effort raw_text language auto-detection (SPEC §8.8), on by default.
		svc.repGen.SetLanguageDetection(cfg.LanguageDetectionEnabled)
	} else {
		// #398/#364: the shipped store always satisfies model.RepresentationStore
		// (enforced by a compile-time guard in the cli package). If a
		// future/alternate backend does not, repGen stays nil and
		// generateRepresentations returns nil for EVERY document — producing zero
		// raw-text/OCR/media/transcript representations, zero chunks and zero
		// embeddings corpus-wide, silently. That is the #364 failure mode one layer
		// up from the ChunkSource seam it fixed, so warn just as loudly here rather
		// than degrade without a trace.
		svc.logger.Printf("WARNING: store %T does not satisfy model.RepresentationStore; "+
			"representation generation is DISABLED — no chunks or embeddings will be produced for any document", store)
	}
	// Construct the output quality gate from config (spec 0.16.0). When the
	// master switch is on we use the package defaults (per-threshold config is
	// a follow-up); when off, the field stays nil and screening is skipped.
	if cfg.QualityGatesEnabled {
		svc.qualityGate = quality.New(quality.DefaultConfig())
	}
	svc.resolveTranscriptIdentityFields()
	// Resolve the optional transcript-translation binding (SPEC §8.6.2). When
	// translation is enabled we resolve the chat capability and build a
	// generator; when off (default), or no chat provider resolves, the field
	// stays nil and the translate step self-skips so behaviour is unchanged.
	svc.translateTargetLangs = append([]string(nil), cfg.MediaTranslateTargetLangs...)
	svc.translateEngine = strings.ToLower(strings.TrimSpace(cfg.MediaTranslateEngine))
	if svc.translateEngine == "" {
		svc.translateEngine = "chat"
	}
	svc.resolveTranslateBinding(cfg)
	// Resolve the optional summary-generation binding for hierarchical retrieval
	// (SPEC §9.7). Off by default: with the feature disabled, or with no chat
	// provider, the binding stays nil and every summary step self-skips.
	svc.resolveSummaryBinding(cfg)
	// Resolve the multimodal embedding mode once (SPEC 8.1.7); a missing or
	// unresolvable embed profile leaves it off (text-only).
	if ep, err := cfg.Providers().Resolve(provider.CapEmbed); err == nil {
		svc.embedMultimodal = provider.NormalizeEmbedMultimodal(ep.EmbedMultimodal)
	}
	svc.resolveContextualBinding(cfg)
	return svc, nil
}

// resolveContextualBinding resolves the optional contextual-retrieval binding
// (SPEC §8.1.8, issue #330) and, when it is effectively active, builds the
// per-chunk context generator the representation generator uses.
//
// Capability-driven and FAIL-OPEN, exactly like OCR/STT: with the feature off,
// with no chat provider resolvable, or with a generator that cannot be built,
// the contextualizer stays nil — the corpus embeds raw under the effective
// `…|off` embed identity plus a warning, never a hard error and never a raw
// corpus wearing a contextual identity.
func (svc *Service) resolveContextualBinding(cfg config.Config) {
	binding := cfg.ContextualBinding()
	svc.contextualActive = binding.Active
	if binding.FellOpen {
		svc.getLogger().Printf(
			"contextual retrieval requested (retrieval.contextual.enabled=true) but no chat provider is available; " +
				"embedding raw chunks and recording the effective `off` embed identity (SPEC §8.1.8) — " +
				"configure a chat-capable provider to enable it")
		return
	}
	if !binding.Active || svc.repGen == nil {
		return
	}
	gen, err := providerfactory.Generator(binding.Profile)
	if err != nil {
		svc.contextualActive = false
		svc.getLogger().Printf(
			"contextual retrieval disabled: build context generator (%s): %v", binding.Profile.Name, err)
		return
	}
	svc.repGen.SetContextualizer(newChunkContextGenerator(
		gen, binding, filepath.Join(cfg.StateDir, "cache", "context"), svc.getLogger().Printf))
}

// resolveTranslateBinding resolves the optional transcript-translation binding
// (SPEC §8.6.2). Split out of NewService to keep that constructor under the
// cyclomatic-complexity budget; behaviour is unchanged. When translation is
// enabled we resolve the chat capability and build a generator; when off
// (default), or no chat provider resolves, the field stays nil and the
// translate step self-skips so behaviour is unchanged.
func (svc *Service) resolveTranslateBinding(cfg config.Config) {
	if cfg.MediaTranslateEnabled && len(svc.translateTargetLangs) > 0 {
		if svc.translateEngine == "whisper" {
			// Whisper's native translate task (audio->English) instead of the chat
			// generator. Config validation guarantees the STT provider is
			// kind:whisper and the targets are English-only before we get here.
			if tr, prof, terr := translateTranscriberFromConfig(cfg); terr == nil && tr != nil {
				svc.translateSTT = tr
				svc.translateProvider = prof.Name
				svc.translateModel = strings.TrimSpace(prof.STTModel)
			} else if terr != nil {
				svc.getLogger().Printf("whisper transcript translation disabled: %v", terr)
			}
		} else if tr, prof, terr := translatorFromConfig(cfg); terr == nil && tr != nil {
			svc.translator = tr
			svc.translateProvider = prof.Name
			svc.translateModel = strings.TrimSpace(prof.ChatModel)
		} else if terr != nil {
			svc.getLogger().Printf("transcript translation disabled: %v", terr)
		}
	}
}

// resolveTranscriptIdentityFields populates the transcript derivation-identity
// fields (transcript language, STT provider/model, and the diarize binding) on s
// from s.cfg. It is the single source of truth for that resolution, shared by
// NewService and ActiveDerivationIdentities so the transcript identity the
// retriever reconstructs for open_file (issue #488) cannot drift from the one
// ingest folds into the transcript cache key it writes (SPEC §8.6.7).
func (s *Service) resolveTranscriptIdentityFields() {
	s.transcriptLanguage = sttExpectedLanguage(s.cfg)
	// Resolve the STT derivation identity (SPEC §8.6.7) from the same profile the
	// transcriber uses, so a recorded transcript identity can be compared against
	// the active one to detect a model swap. Empty when STT is off.
	prof, ok := resolveSTTProfile(s.cfg)
	if !ok {
		return
	}
	s.sttProvider = strings.TrimSpace(prof.Name)
	s.sttModel = strings.TrimSpace(prof.STTModel)
	// Honest STT language coverage (SPEC §8.2.1, #566). When the profile declares
	// a non-empty coverage set and the pinned source language falls outside it,
	// warn once at construction so an operator sees "this model does not declare
	// this language" instead of only a downstream quality-gate drop. Fail-open:
	// transcription still runs; the §8.6.6 quality gate remains the backstop for
	// degraded output. Unknown coverage (no declaration) makes no assertion.
	s.sttLanguages = append([]string(nil), prof.STTLanguages...)
	if lang := strings.TrimSpace(s.transcriptLanguage); lang != "" {
		if declared, covered := provider.STTLanguageCoverage(prof, lang); declared && !covered {
			s.getLogger().Printf("stt: language %q is outside %s's declared coverage %v; transcription will proceed but may be degraded (SPEC §8.2.1) — configure an STT provider that covers this language",
				lang, s.sttProvider, prof.STTLanguages)
		}
	}
	// Resolve the diarization binding (SPEC §8.6.8) from the SAME STT profile:
	// diarization is active when enabled (tri-state) AND the backend advertises
	// CapDiarize. The diarize derivation identity (§8.6.7) is the backend's
	// provider name + STT model, so a backend/model swap re-derives. When
	// inactive the fields stay empty and the STT path is unchanged.
	if config.DiarizationActive(s.cfg, prof) {
		s.diarizeActive = true
		s.diarizeProvider = strings.TrimSpace(prof.Name)
		s.diarizeModel = strings.TrimSpace(prof.STTModel)
	}
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

// SetOnDocumentError registers a callback invoked once per non-fatal
// per-document failure. The message passed to fn has already been
// secret-redacted by persistNonFatalDocError; callers MUST NOT re-derive it
// from the underlying error. Passing nil clears the callback.
func (s *Service) SetOnDocumentError(fn func(relPath, docType, message string)) {
	s.onDocumentErrorMu.Lock()
	defer s.onDocumentErrorMu.Unlock()
	s.onDocumentError = fn
}

// SetOnDocumentSkip registers a callback invoked once per document this run
// leaves never-indexed. `reason` is a model.SkipReason* value. Passing nil
// clears the callback.
//
// A document raises either this callback or SetOnDocumentError, never both:
// the two partition the never-indexed set (SPEC §3.2).
func (s *Service) SetOnDocumentSkip(fn func(relPath, docType, reason string)) {
	s.onDocumentSkipMu.Lock()
	defer s.onDocumentSkipMu.Unlock()
	s.onDocumentSkip = fn
}

// SetOnDocumentContentHash registers a callback invoked after every durable
// write of a document row, with the content_hash that row now holds (#691).
// An empty hash reports that the document has no usable content_hash right now:
// the #402 done marker is withheld until the representations commit, and a
// failed document never gets one. Passing nil clears the callback.
func (s *Service) SetOnDocumentContentHash(fn func(relPath, contentHash string)) {
	s.onDocumentContentHashMu.Lock()
	defer s.onDocumentContentHashMu.Unlock()
	s.onDocumentContentHash = fn
}

// DiscoverOptionsFromConfig resolves ingest discovery behavior from config.
// Defaults mirror config.Config defaults: .gitignore support is enabled by
// default (IngestGitignore=true), and symlink following is disabled by default
// (IngestFollowSymlinks=false).
func DiscoverOptionsFromConfig(cfg config.Config) DiscoverOptions {
	options := DefaultDiscoverOptions()
	options.UseGitIgnore = cfg.IngestGitignore
	options.FollowSymlinks = cfg.IngestFollowSymlinks
	// One resolved list for the scan, the watcher, and the S3 lister (#773).
	options.ExcludeDirs = append([]string(nil), cfg.IngestExcludeDirs...)
	// One resolver for the cap as well (#682), so discovery, the source reads, the
	// object-store backend, and the on-demand tool paths enforce one number.
	options.MaxSizeBytes = ResolvedMaxFileBytes(cfg)
	options.MediaVariants = MediaVariantOptionsFromConfig(cfg)
	return options
}

// mistralExtractor resolves the bespoke-OCR provider (the `kind: mistral`
// /v1/ocr surface) via the provider model and adapts it to
// model.DocumentExtractor. It honors the explicit `model.ocr.provider`
// binding (ocrProviderName) so an operator can point OCR at a self-hosted
// bespoke-OCR endpoint — a `kind: mistral` profile on a custom `base_url`
// (dir2mcp#240) — falling back to the built-in `mistral-ocr` profile when the
// binding is unset. Returns nil when no OCR-capable profile resolves (matching
// the prior "no key -> no extractor" behavior). Docling is handled separately
// below — it is a local tool / docling-serve endpoint, not a provider profile.
func mistralExtractor(cfg config.Config) model.DocumentExtractor {
	prof, err := cfg.Providers().ResolveExplicit(provider.CapOCR, ocrProviderName(cfg), true)
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
	case "pandoc":
		return NewPandocExtractor(strings.TrimSpace(cfg.IngestPandocCommand))
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
		// Language-based routing (SPEC §8.2.1, #566): if the effective source-language
		// pin (the profile's configured STTLanguage, or the per-request override folded
		// in above) maps to a provider profile via media.stt.language_providers, that
		// profile REPLACES the default so the ACTUAL transcriber matches the backend
		// resolveSTTProfile reports. Routing is pin-based and per-run — it keys off the
		// configured pin, never per-item auto-detection — so an empty pin leaves the
		// default untouched. RouteSTTProfile carries the pin onto the routed profile.
		routed, rerr := cfg.RouteSTTProfile(prof)
		if rerr != nil {
			return nil, fmt.Errorf("stt provider %q: %w", sel, rerr)
		}
		prof = routed
		// media.vad (dir2mcp#258) is a global toggle, applied onto whichever STT
		// profile resolves; providers without VAD support ignore it.
		prof.STTVAD = cfg.MediaVAD
		// media.stt.* request limits (dir2mcp#510/#511), likewise applied onto the
		// resolved STT profile; 0 leaves the whisper client's built-in defaults.
		prof.STTMaxPayloadMB = cfg.MediaSTTMaxPayloadMB
		prof.STTRequestTimeoutSec = cfg.MediaSTTRequestTimeoutSec
		tr, berr := buildTranscriber(prof)
		if berr != nil {
			return nil, fmt.Errorf("stt provider %q: %w", sel, berr)
		}
		return tr, nil
	}
	if sel == transcriberProviderAuto {
		prof, err := r.Resolve(provider.CapSTT)
		if errors.Is(err, provider.ErrNoProvider) {
			return nil, nil // nothing eligible -> STT off
		}
		return build(prof, err)
	}
	// Shared selector->profile table (issue #440 F6): mistral -> mistral-ocr
	// (Voxtral), elevenlabs, whisper (self-hosted, credential-less — a missing
	// base_url surfaces as a provider error at first use), openai, gemini. Every
	// mapped kind is buildable by providerfactory.Transcriber, so an explicit
	// selector reaches the same backend the resolver validated. An unmapped
	// selector is rejected here and, in a normal startup, already at
	// config.Validate (validateSTTProvider).
	profileName, known := config.STTSelectorProfile(sel)
	if !known {
		return nil, fmt.Errorf("unsupported transcriber provider %q", sel)
	}
	prof, err := r.ResolveExplicit(provider.CapSTT, profileName, true)
	return build(prof, err)
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

// captionCleanOptions builds the shared ingest-time cue cleaning from
// media.subtitles.{drop_urls,drop_phrases,scrub_phrases,collapse_repeats}
// (issues #545, #765). The same options clean STT transcript chunks, translated
// transcript chunks and sidecar-cue chunks before embedding, and they are the
// SAME subtitle.CleanOptions shape the export path builds (cli.newCuePipeline),
// so a hallucinated URL, a wholly-spam chunk or a repetition run is neither
// exported nor indexed, keeping the index and the sidecar in agreement rather
// than leaving cues that are invisible in the sidecar but citable from the index.
//
// Glossary is deliberately left unset: SPEC §8.6.2 pins media.subtitles.glossary
// as an export-time find/replace on already-rendered cue text, so rewriting
// indexed text with it needs a spec change first, not a code fix. With every key
// unset the options are inactive and the cleaning is a byte-for-byte no-op.
//
// The patterns were already validated at config load (config.Validate →
// subtitle.NewDropSet), so a compile error here is unexpected; it is logged and
// treated as no cleaning (nil set) rather than failing ingestion.
func (s *Service) captionCleanOptions() subtitle.CleanOptions {
	drop, err := subtitle.NewDropSet(s.cfg.MediaSubtitlesDropPhrases)
	if err != nil {
		s.getLogger().Printf("media.subtitles.drop_phrases invalid at ingest, ignoring: %v", err)
	}
	scrub, err := subtitle.NewDropSet(s.cfg.MediaSubtitlesScrubPhrases)
	if err != nil {
		s.getLogger().Printf("media.subtitles.scrub_phrases invalid at ingest, ignoring: %v", err)
	}
	return subtitle.CleanOptions{
		DropURLs:        s.cfg.MediaSubtitlesDropURLs,
		Drop:            drop,
		Scrub:           scrub,
		CollapseRepeats: s.cfg.MediaSubtitlesCollapseRepeats,
	}
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
	prof, ok := resolveDefaultSTTProfile(cfg)
	if !ok {
		return provider.Profile{}, false
	}
	// Language-based routing (SPEC §8.2.1, #566): if the resolved source-language
	// pin maps to a provider profile via media.stt.language_providers, that profile
	// REPLACES the default here too, so the expected-language hint, the STT
	// derivation identity, and the honest-coverage check all observe the SAME
	// backend that TranscriberFromConfigWithLanguage builds. A routed profile whose
	// credential is unset resolves as STT-off on this path (ok=false), mirroring the
	// default-profile eligibility handling above.
	routed, err := cfg.RouteSTTProfile(prof)
	if err != nil {
		return provider.Profile{}, false
	}
	return routed, true
}

// resolveDefaultSTTProfile resolves the DEFAULT (un-routed) STT provider profile
// from the legacy stt.provider selector (SPEC 8.1.3). It is the pre-routing half
// of resolveSTTProfile, split out so both the routed resolver and the router
// (which needs the resolved pin) share one selection path. ok=false when STT is
// off, when no STT-capable profile resolves, or when the selector is unrecognised.
func resolveDefaultSTTProfile(cfg config.Config) (provider.Profile, bool) {
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
	if sel == transcriberProviderAuto {
		prof, err = r.Resolve(provider.CapSTT)
	} else if profileName, known := config.STTSelectorProfile(sel); known {
		// Shared selector->profile table (issue #440 F6): mistral/elevenlabs/
		// whisper/openai/gemini all resolve here, matching the factory's support,
		// instead of silently falling through to STT-off on this path.
		prof, err = r.ResolveExplicit(provider.CapSTT, profileName, true)
	} else {
		return provider.Profile{}, false
	}
	if err != nil {
		return provider.Profile{}, false
	}
	return prof, true
}

// ResolveSTTProviderModel returns the resolved active STT provider profile name
// and its STT model for cfg (SPEC 8.1.3), mirroring the exact selection
// TranscriberFromConfigWithLanguage / resolveSTTProfile perform. It exists so the
// MCP tool layer can report the transcriber ACTUALLY used as provenance instead
// of a hardcoded "mistral"/"voxtral-mini-latest" pair (issue #440 F5): with
// stt_provider=elevenlabs/whisper/gemini/auto the reported provider now matches
// the backend that produced the transcript. ok=false when STT is off or no
// STT-capable profile resolves, in which case the caller keeps its own fallback.
func ResolveSTTProviderModel(cfg config.Config) (providerName, sttModel string, ok bool) {
	prof, resolved := resolveSTTProfile(cfg)
	if !resolved {
		return "", "", false
	}
	return strings.TrimSpace(prof.Name), strings.TrimSpace(prof.STTModel), true
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

// translationConfigured reports whether transcript translation should run: at
// least one target language plus a resolved engine binding (the chat generator
// for engine=chat, or the Whisper translate transcriber for engine=whisper).
// Both translate gates consult this so the two engines self-skip identically
// when translation is off or unresolved.
func (s *Service) translationConfigured() bool {
	return len(s.translateTargetLangs) > 0 && (s.translator != nil || s.translateSTT != nil)
}

// translateTranscriberFromConfig resolves the active STT profile and builds a
// Whisper translate-task transcriber for media.translate.engine=whisper. It
// reuses the same STT profile resolution as source transcription (resolveSTTProfile)
// so translation re-decodes the audio on the exact provider that produced the
// source transcript. Returns an error (leaving translation off) when no STT
// profile resolves or the profile is not translate-capable (non-whisper).
func translateTranscriberFromConfig(cfg config.Config) (model.Transcriber, provider.Profile, error) {
	prof, ok := resolveSTTProfile(cfg)
	if !ok {
		return nil, provider.Profile{}, errors.New("resolve STT provider for whisper translation: no STT provider")
	}
	tr, err := providerfactory.TranslateTranscriber(prof)
	if err != nil {
		return nil, provider.Profile{}, fmt.Errorf("build whisper translate transcriber (%s): %w", prof.Name, err)
	}
	return tr, prof, nil
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

// SetContextualizer overrides the contextual-retrieval binding (SPEC §8.1.8),
// primarily for tests and for callers that supply their own generator. Passing a
// nil contextualizer disables contextualization: every chunk then records
// embedding_mode=disabled and embeds raw, exactly as before the feature existed.
func (s *Service) SetContextualizer(c ChunkContextualizer) {
	s.contextualActive = c != nil
	if s.repGen != nil {
		s.repGen.SetContextualizer(c)
	}
}

// SetTranslateTranscriber overrides the whisper translate-task binding and
// selects the whisper translation engine, primarily for tests. Passing a nil
// transcriber (or empty langs) disables translation. The recorded provider/model
// are written into translated transcripts' meta_json. Target languages are
// normalized (trimmed/lower-cased).
func (s *Service) SetTranslateTranscriber(transcriber model.Transcriber, providerName, modelName string, targetLangs []string) {
	s.translateSTT = transcriber
	s.translateEngine = "whisper"
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

// SetSTTLanguages overrides the resolved STT profile's declared language coverage
// (SPEC §8.2.1, #566) — the non-empty BCP-47 set that drives the honest-coverage
// floor. It mirrors SetSTTIdentity for tests that need deterministic coverage
// without resolving a real STT provider profile. A nil/empty set means
// open/unknown (no coverage assertion), exactly as an undeclared profile.
func (s *Service) SetSTTLanguages(languages []string) {
	s.sttLanguages = append([]string(nil), languages...)
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

	// Reset the per-run progress counters so each scan reports the current corpus
	// rather than accumulating across every scan the daemon performs (issue #426).
	// Without this the 10-min safety rescan re-adds the whole corpus each time,
	// `errors` never clears after a transient failure is repaired, and stale
	// event-path increments carry forward. embeddedOK is intentionally preserved
	// (owned by the embed worker, not this scan).
	if s.indexingState != nil {
		s.indexingState.ResetProgress()
	}
	// Reset the in-run per-reason skip counter so each scan reports only its own
	// path-excluded drops (these are not persisted; see runSkipReasons).
	s.runSkipReasonsMu.Lock()
	s.runSkipReasons = nil
	s.runSkipReasonsMu.Unlock()

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
	// Surface size-cap exclusions (issue #497): a file over the ingest file cap is
	// dropped at discovery — never scanned, indexed, or (previously) reported — so
	// an operator had no signal that, say, a 300 MB media file was excluded. Count
	// each drop as scanned+skipped so live `status` shows a non-zero skipped, and
	// log a clear line naming ingest.max_file_mb so the cap is discoverable and
	// actionable rather than a silent no-op (dishonest coverage).
	capBytes := discoverOpts.MaxSizeBytes
	if capBytes <= 0 {
		capBytes = corpusfs.DefaultMaxFileSizeBytes()
	}
	capMB := float64(capBytes) / (1024 * 1024)
	// Persist a minimal skipped row for each size-capped file (skip_reason=
	// size_cap) so the honest-coverage aggregate (CorpusStats.SkipSummary)
	// reports *why* it was not indexed, not just that skipped>0 (#414/#497).
	// Collected here and upserted after `seen` is built so those rows are
	// registered as seen and markMissingAsDeleted does not tombstone them. The
	// same drops are recorded on s.pendingOversize so a FILE_TOO_LARGE manifest
	// entry (§14.4, #422) is also emitted once the batch run exists.
	oversize := make(map[string]int64)
	s.pendingOversize = nil
	discoverOpts.OnOversize = func(relPath string, size int64) {
		s.addScanned(1)
		s.addSkipped(1)
		oversize[relPath] = size
		s.pendingOversize = append(s.pendingOversize, relPath)
		s.getLogger().Printf(
			"discovery: skipping %s (%d bytes) — exceeds ingest.max_file_mb cap (%.0f MB); raise ingest.max_file_mb to include it",
			relPath, size, capMB,
		)
	}
	// A bucket key that is not a usable rel_path is refused at discovery
	// (#735). Logged rather than counted as a skip: it is not a corpus file
	// dir2mcp declined to index, it is a key that could not be named safely,
	// and an operator needs to see that their bucket contains such keys.
	discoverOpts.OnUnsafeKey = func(key string, unsafeErr error) {
		s.getLogger().Printf(
			"discovery: refusing object key %q — it does not resolve inside the corpus root: %v",
			key, unsafeErr,
		)
	}
	// A symlink is dropped at discovery under the default
	// ingest.follow_symlinks=false, which is right, but it used to be silent
	// too: a corpus of links into a media library reported scanned=0,
	// skipped=0, errors=0 and a ready daemon, exactly like an empty directory
	// (#781). #792 added the log. This now also COUNTS the drop, which #792
	// could not: SPEC §15.2 closes the skip_reasons enum for a spec minor, and
	// none of the eight reasons then described a not-followed link. Borrowing
	// path_excluded would have named a false cause in the honest-coverage
	// aggregate, so the count waited for spec 0.46.0 to add symlink_ignored.
	//
	// Counted like OnOversize (#497) rather than logged like OnUnsafeKey
	// (#735), because the two cases differ: an unsafe bucket key is not a
	// corpus file dir2mcp declined to index, while a link IS a corpus entry
	// with a deliberate policy applied to it.
	symlinks := make(map[string]struct{})
	discoverOpts.OnSkippedSymlink = func(relPath string) {
		s.addScanned(1)
		s.addSkipped(1)
		symlinks[relPath] = struct{}{}
		s.getLogger().Printf(
			"discovery: skipping %s (symlink); ingest.follow_symlinks is false, so links are not indexed. Set ingest.follow_symlinks: true to include it (a followed link must still resolve inside the corpus root)",
			relPath,
		)
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

	// Arm the output-set reconciliation for this scan (#692) when the pipeline's
	// desired output SET changed since the last completed scan. This is what makes
	// an UNCHANGED document eligible for cleanup: the content gate would skip it,
	// so a removed translation target or a disabled summary level would otherwise
	// stay searchable forever. It is a no-op on a steady-state scan.
	s.beginOutputReconciliation(ctx, forceReindex)

	// Optional batch-ergonomics run (SPEC §8.6.11): a JSONL run manifest and/or a
	// side-channel progress reporter. nil (and inert) unless a media.batch feature
	// is enabled, so the default ingest path is unchanged.
	s.batch = newBatchRun(s.cfg.MediaBatchProgress, s.cfg.MediaBatchManifest != "", s.cfg.MediaBatchManifest, s.getLogger())
	defer func() {
		s.batch.close()
		s.batch = nil
		s.activePass = passSingle
	}()

	// Emit a FILE_TOO_LARGE manifest entry (§14.4) for each file dropped at
	// discovery for exceeding the ingest size cap (#497). These never enter the
	// per-asset loop, so this is their only manifest producer. Manifest-only and
	// no-op when the manifest is disabled.
	for _, relPath := range s.pendingOversize {
		s.batch.recordSkippedWithCode(relPath, manifestErrFileTooLarge)
	}

	seen := make(map[string]struct{}, len(discovered))

	// Register size-capped drops as durable skipped rows before the pass(es)
	// run so they survive markMissingAsDeleted and feed the coverage aggregate.
	s.persistOversizeSkips(ctx, oversize, seen)
	s.persistSymlinkSkips(ctx, symlinks, seen)

	// Optional two-phase pass split (SPEC §8.6.11): run media ingest as two ordered
	// passes over the corpus — a transcription pass (STT/sidecar → source
	// transcript, plus all non-media representations) followed by a derivation pass
	// (translation §8.6.2). It is observably equivalent to single-pass for the
	// resulting representations/chunks/embeddings/citations (it changes ordering and
	// reporting only), and each pass is independently resumable via the existing
	// identity/cache state (§7.6/§8.6.7) so an interrupted transcription pass does
	// not force re-transcription of completed assets. Default (off) is the single
	// pass below, byte-identical to before the split existed.
	if s.cfg.MediaBatchTwoPhase {
		if err := s.scanPass(ctx, passTranscription, discovered, compiledSecrets, forceReindex, seen); err != nil {
			return err
		}
		if err := s.scanPass(ctx, passDerivation, discovered, compiledSecrets, forceReindex, seen); err != nil {
			return err
		}
		return s.finishScan(ctx, existing, seen)
	}

	if err := s.scanPass(ctx, passSingle, discovered, compiledSecrets, forceReindex, seen); err != nil {
		return err
	}
	return s.finishScan(ctx, existing, seen)
}

// finishScan closes out a completed scan: it tombstones the documents that are
// no longer present, then records the pipeline output identity this scan
// reconciled the corpus against (#692). The identity is recorded LAST, and only
// on success, so an interrupted scan reconciles again next run rather than
// claiming a cleanup it did not finish.
func (s *Service) finishScan(ctx context.Context, existing, seen map[string]struct{}) error {
	if err := s.markMissingAsDeleted(ctx, existing, seen); err != nil {
		return err
	}
	s.commitOutputReconciliation(ctx)
	return nil
}

// scanPass processes every discovered asset once under the given pass (SPEC
// §8.6.11). passSingle runs the full pipeline; passTranscription and
// passDerivation are the two ordered halves of the two-phase split. The same
// per-asset processing is reused across all three passes — only the work each
// representation generator performs differs, governed by s.activePass. The seen
// set is shared across passes so markMissingAsDeleted sees the union of assets
// observed in either pass.
func (s *Service) scanPass(ctx context.Context, pass processPass, discovered []DiscoveredFile, compiledSecrets []*regexp.Regexp, forceReindex bool, seen map[string]struct{}) error {
	s.activePass = pass
	s.batch.startPass(pass.label(), len(discovered))

	for _, f := range discovered {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Scan/skip counters reflect the corpus, not the pass: count them only on the
		// first (or only) pass so a two-phase run reports the same totals as
		// single-pass. The derivation pass reuses the same per-asset processing but
		// must not double-count.
		countCorpus := pass != passDerivation
		if countCorpus {
			s.addScanned(1)
		}

		var outcome *assetOutcome
		if s.batch != nil {
			outcome = newAssetOutcome(f.RelPath)
			s.activeOutcome = outcome
		}

		if matchesAnyPathExclude(f.RelPath, s.cfg.PathExcludes) {
			if countCorpus {
				s.addSkipped(1)
				s.addRunSkipReason(model.SkipReasonPathExcluded)
				// Path-excluded files are never persisted, so the stream event is
				// the only per-document record of them anywhere.
				s.notifyDocumentSkip(model.Document{
					RelPath:    f.RelPath,
					DocType:    ClassifyDocType(f.RelPath),
					Status:     "skipped",
					SkipReason: model.SkipReasonPathExcluded,
				})
			}
			if outcome != nil {
				outcome.markSkipped()
				s.batch.finalize(outcome)
				s.activeOutcome = nil
			}
			continue
		}

		err := s.processDocument(ctx, f, compiledSecrets, forceReindex, seen)
		if outcome != nil {
			if err != nil {
				outcome.markErrorIfUnset(manifestErrorCode(err), RedactSecretsInMessage(err.Error(), compiledSecrets))
			} else {
				s.recordAssetOutputs(ctx, outcome, f.RelPath)
			}
			s.batch.finalize(outcome)
			s.activeOutcome = nil
		}

		if err != nil {
			if countCorpus {
				s.addErrors(1)
			}
			// record that we saw the file even if processing failed so
			// markMissingAsDeleted does not treat it as removed
			seen[f.RelPath] = struct{}{}
			continue
		}
		seen[f.RelPath] = struct{}{}
	}
	return nil
}

// recordAssetOutputs records the rep_types persisted for a successfully-processed
// asset onto its batch manifest outcome (SPEC §8.6.11 "outputs produced"). It is
// best-effort: a store that cannot list rep_types, or a lookup error, leaves
// outputs empty — the manifest is advisory and must never fail an ingest.
func (s *Service) recordAssetOutputs(ctx context.Context, outcome *assetOutcome, relPath string) {
	if outcome == nil {
		return
	}
	lister, ok := s.store.(interface {
		RepresentationTypesByPath(context.Context, string) ([]string, error)
	})
	if !ok {
		return
	}
	types, err := lister.RepresentationTypesByPath(ctx, relPath)
	if err != nil {
		return
	}
	for _, t := range types {
		outcome.addOutput(t)
	}
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
	// An empty recorded content_hash means the document was never durably
	// finalized — a withheld #402/#502 done-marker left blank by a crash before
	// the representation-commit (or, for an archive container, before member
	// extraction completed). The ETag+size may match, but the object still needs
	// processing, so do NOT take the fast-skip path: fall through to the full
	// read/hash path which re-runs the withheld work. A fully-indexed document
	// always carries a non-empty content_hash, so the normal S3 fast-path is
	// unaffected. This is what makes the withhold gate effective for S3 archives
	// (the ETag skip runs before the content_hash recompute).
	if strings.TrimSpace(existing.ContentHash) == "" {
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
	// The remote fast path returns before processDocument's own reconciliation
	// call sites, so reconcile here too (#692). Without this, an object-store
	// corpus would never retire an obsolete output on the path it takes for every
	// unchanged object.
	s.reconcileDocumentOutputs(ctx, f.RelPath)
	switch existing.Status {
	case "ok":
		s.addIndexed(1)
	case "skipped", "secret_excluded":
		s.addSkipped(1)
		s.notifyDocumentSkip(existing)
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
	// The row carries no content_hash, so dedup must forget this path (#691).
	s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
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
	// Authoritative per-document reset of the §8.6.6/#426 quality-gate quarantine
	// flag. The scan loop is sequential (at most one asset in flight), so resetting
	// here — at the VERY START, before any early-return branch (build error, remote
	// skip, archive, cache-hit/skipped, non-ok status) — guarantees every document
	// begins with a clean flag by construction, matching the per-doc reset pattern
	// used for activeOutcome/indexedPending. Without this, a stale true from a prior
	// quarantined document could persist across an early-return path that never
	// reaches generateRepresentations/deriveDocument (which also reset the flag) and
	// wrongly suppress this document's indexed credit. The resets inside
	// generateRepresentations/deriveDocument are kept: they re-scope the flag for
	// sequential archive members, which run as separate processDocumentFromContent
	// calls under a single processDocument entry.
	s.quarantinedThisDoc = false
	// Open the per-document secret scope (#681) at the same authoritative point,
	// and before any branch below can produce a derived text. It captures the
	// pattern set this call was given, so the derived-text scan always uses the
	// same patterns as the raw-byte scan in buildDocumentWithContent.
	s.beginDocumentSecretScope(secretPatterns)

	// Two-phase derivation pass (SPEC §8.6.11): the transcription pass already did
	// all document building, upserting, counting, and source-transcript work for
	// this asset; the derivation pass runs ONLY translation (§8.6.2) and re-doing
	// the full document pipeline here would either double-count or be gated out by
	// the unchanged-content check (the document is identical to what the
	// transcription pass just wrote). Route to the lean derivation-only path.
	if s.activePass == passDerivation {
		return s.deriveDocument(ctx, f, secretPatterns)
	}

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
	if isSizeCapSkip(doc) {
		// #682: the read was refused, so this document is finished here. Everything
		// below reasons about content this run did not obtain.
		return s.settleSizeCapSkip(ctx, doc)
	}
	// Stamp the resolved content_hash onto the batch manifest outcome (§8.6.11);
	// no-op when no batch run is active.
	s.recordContentHash(doc.ContentHash)

	existingDoc, err := s.store.GetDocumentByPath(ctx, doc.RelPath)
	if isUnexpectedStoreErr(err) {
		return fmt.Errorf("get existing document: %w", err)
	}

	// #681: a secret found in DERIVED text is not reproducible from the source
	// bytes, so the verdict is carried on the row rather than recomputed. Applied
	// before the gate and the upsert so an unchanged withheld document stays
	// withheld instead of being written back as "ok".
	carrySecretExclusion(&doc, existingDoc)

	needsProcessing := s.resolveNeedsProcessing(ctx, existingDoc, doc, forceReindex)
	finalContentHash, willGenerateReps := withholdContentHash(&doc, needsProcessing)
	// #502: also withhold the marker for an archive container this run will
	// (re)extract; finalContentHash still carries the original hash for the deferred
	// finalize (withholdContentHash captured it before this blank).
	withholdArchiveContentHash(&doc, needsProcessing)

	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}
	// Report the hash the row now holds (#691). It is blank while the #402 done
	// marker is withheld, which makes dedup forget the path for the duration of
	// the reprocess instead of grouping on the previous content.
	s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)

	if err := s.refreshDocID(ctx, &doc); err != nil {
		return err
	}

	// Apply the #426 initial-status accounting. indexedPending tracks the ok case
	// so it is credited only at each point below where no further rep work can
	// fail this run; a terminal status is already fully counted and returns early.
	indexedPending, terminal := s.creditInitialStatus(doc)
	if terminal {
		return nil
	}

	// Archive containers: extract and ingest each member as its own document.
	// The archive document itself remains "skipped" (no direct text content).
	if doc.DocType == "archive" {
		s.creditIndexed(indexedPending)
		return s.handleArchiveDocumentAndNotify(ctx, doc, f, secretPatterns, forceReindex, seen, needsProcessing, finalContentHash)
	}

	if !needsProcessing || doc.Status != "ok" {
		// No representation work performed this run (cache hit / unchanged content
		// / non-ok status): record it as skipped for the batch manifest (§8.6.11).
		// An ok cache hit is still a durably-indexed document, so credit it.
		//
		// This is the incremental path #692 is about. The document is unchanged, so
		// nothing is re-derived, but the outputs a previous pipeline left on it are
		// still retired. Retiring costs nothing to undo: both covered outputs are
		// backed by an on-disk derivation cache.
		s.reconcileDocumentOutputs(ctx, doc.RelPath)
		s.creditIndexed(indexedPending)
		s.markActiveSkipped()
		return nil
	}

	nonFatalErrored, err := s.generateRepresentations(ctx, doc, content, secretPatterns, forceReindex)
	if err != nil {
		// Persist error status so the next incremental run retries this document
		// and operators can identify it via status queries.  Silence the upsert
		// error since the original rep error is the more actionable signal.
		doc.Status = "error"
		doc.ErrorMessage = RedactSecretsInMessage(err.Error(), secretPatterns)
		_ = s.store.UpsertDocument(ctx, doc)
		return fmt.Errorf("generate representations: %w", err)
	}
	// #681: a derived text of this document matched a configured secret pattern, so
	// the document was withheld as secret_excluded and its representations were
	// retired. Return before the summary step, which would summarize the very text
	// that is being withheld, and before the "ok" finalize, which the #413 guard
	// would refuse anyway. The skip is already counted, so the pending indexed
	// credit is simply never taken.
	if s.secretExcludedThisDoc {
		return s.finalizeSecretExcluded(ctx, &doc, finalContentHash)
	}
	return s.settleProcessedDocument(ctx, &doc, willGenerateReps, finalContentHash, nonFatalErrored, indexedPending)
}

// settleProcessedDocument runs the post-generation steps for a document whose
// representations committed: the optional document-level summary, the #692 output
// reconciliation, the #402 done-marker stamp, and the #426 indexed credit. Split
// out of processDocument so that function stays within the cyclomatic-complexity
// budget; the order of the steps is unchanged and is load-bearing (see the
// comments on each step).
func (s *Service) settleProcessedDocument(
	ctx context.Context,
	doc *model.Document,
	willGenerateReps bool,
	finalContentHash string,
	nonFatalErrored, indexedPending bool,
) error {
	// Derive the optional document-level `summary` representation over the
	// committed chunks (SPEC §5.2/§9.7). It runs AFTER the fine chunks are written
	// (it summarizes them) and is fail-open by construction: it never returns an
	// error, so an absent summary leaves the document fully indexed and
	// flat-retrievable, and the next scan retries.
	s.GenerateDocumentSummaries(ctx, *doc)
	// Every output this pipeline produces for the document has now committed, so
	// it is safe to retire the ones it no longer produces (#692). Running the
	// reconciliation AFTER the generation pass is what guarantees a representation
	// is never destroyed before the output that supersedes it exists, and it is
	// also what makes the batch manifest's `outputs` report the FINAL active set:
	// recordAssetOutputs reads the representation types after processDocument
	// returns.
	s.reconcileDocumentOutputs(ctx, doc.RelPath)
	// Stamp the withheld #402 done marker now that the
	// chunks are durably written. finalizeContentHash re-reads the row, so a
	// document a soft-error path persisted as status="error" is left unmarked and
	// retried next run rather than recorded as fully indexed.
	if err := s.finalizeIfGenerated(ctx, doc, willGenerateReps, finalContentHash); err != nil {
		return err
	}
	if s.suppressIndexedCredit(nonFatalErrored) {
		// A non-fatal soft-error path already persisted this document as
		// status="error" and bumped the error counter itself, so it must count
		// solely as an error, not also as indexed (otherwise indexed+skipped+errors
		// exceeds scanned — issue #426).
		indexedPending = false
	}
	s.creditIndexed(indexedPending)
	return nil
}

// withholdContentHash implements the #402 done-marker withholding. A document's
// content_hash is the incremental "done" marker — resolveNeedsProcessing treats a
// stored hash equal to the freshly computed one as "already indexed, skip".
// Committing that marker in the doc upsert *before* the representations/chunks
// are durably written means an ungraceful death (SIGKILL/OOM/power loss) in that
// gap leaves an ok document carrying the done marker but zero chunks —
// permanently invisible to search/ask and never reprocessed. When this run will
// (re)generate representations, blank the marker on the initial upsert so it is
// stamped only after the reps commit (see finalizeContentHash); it returns the
// original hash to finalize with and whether withholding applied.
func withholdContentHash(doc *model.Document, needsProcessing bool) (finalContentHash string, willGenerateReps bool) {
	willGenerateReps = needsProcessing && doc.Status == "ok" && doc.DocType != "archive"
	finalContentHash = doc.ContentHash
	if willGenerateReps {
		doc.ContentHash = ""
	}
	return finalContentHash, willGenerateReps
}

// withholdArchiveContentHash withholds the content_hash done marker for an archive
// container this run will (re)extract (#502). withholdContentHash does not cover
// archives — they persist as status="skipped", not "ok" — but they still use
// content_hash as the incremental gate for whether member extraction re-runs.
// Blanking it here (and stamping it only after processArchiveMembers completes)
// closes the same crash window #402/#485 closed for the representation-commit path,
// on the archive-extraction path. It is a no-op for non-archives and for archives
// this run will not re-extract. Kept separate from processDocument to hold that
// method within the cyclomatic-complexity budget.
func withholdArchiveContentHash(doc *model.Document, needsProcessing bool) {
	if needsProcessing && doc.DocType == "archive" {
		doc.ContentHash = ""
	}
}

// finalizeIfGenerated stamps the withheld #402 done marker via finalizeContentHash
// when this run generated representations for doc, and is a no-op otherwise. Split
// out so processDocument/processDocumentFromContent stay within the cyclomatic
// complexity limit.
func (s *Service) finalizeIfGenerated(ctx context.Context, doc *model.Document, willGenerateReps bool, contentHash string) error {
	if !willGenerateReps {
		return nil
	}
	return s.finalizeContentHash(ctx, doc, contentHash)
}

// finalizeContentHash stamps the withheld content_hash "done" marker only after a
// document's representations/chunks are durably committed (#402). It re-reads the
// current row first for two reasons:
//
//   - #413: it never overwrites an out-of-band status="error" that a non-fatal
//     per-representation failure may have persisted while generateRepresentations
//     still returned nil (e.g. a transcript provider outage). Such a document keeps
//     an empty content_hash and is retried on the next incremental run rather than
//     being falsely recorded as fully indexed.
//   - It upserts the re-read row rather than the by-value doc, so out-of-band
//     column writes made during representation generation — notably the title
//     (persistTitle/persistTitleIfFound writes documents.title on a separate
//     upsert against the DB row, invisible to this function's by-value doc) —
//     survive. Only the withheld content_hash comes from this finalize step; every
//     other column is taken from the freshly re-read row.
//
// Paths are explicit: found → upsert the re-read row (preserving out-of-band
// columns) after the #413 guard; not-found → recreate the row from doc (there is
// no current row whose columns must be preserved); store error → propagate.
func (s *Service) finalizeContentHash(ctx context.Context, doc *model.Document, contentHash string) error {
	return s.finalizeContentHashForStatus(ctx, doc, contentHash, "ok")
}

// finalizeContentHashForStatus is finalizeContentHash generalized over the
// document's healthy "done" status. The representation-commit path finalizes an
// "ok" document; the archive-extraction path (#502) finalizes a container that
// persists as status="skipped". healthyStatus is the status the freshly re-read
// row must still be in for the withheld marker to be stamped — the #413 guard
// that never resurrects an out-of-band status="error" into a done state.
func (s *Service) finalizeContentHashForStatus(ctx context.Context, doc *model.Document, contentHash, healthyStatus string) error {
	current, err := s.store.GetDocumentByPath(ctx, doc.RelPath)
	if err != nil {
		if !isNotFoundError(err) {
			return fmt.Errorf("fetch document before finalizing content hash: %w", err)
		}
		// Not found: the initial upsert's row is gone (deleted mid-run) or never
		// landed. There are no out-of-band columns to preserve, so deliberately
		// recreate the row from doc with the withheld hash stamped on.
		doc.ContentHash = contentHash
		if err := s.store.UpsertDocument(ctx, *doc); err != nil {
			return fmt.Errorf("finalize document content hash: %w", err)
		}
		s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
		return nil
	}
	// #413 guard: a non-fatal per-representation failure (or archive-subtype
	// failure) may have persisted a status other than the expected healthy one
	// out-of-band; never resurrect it into a done state.
	if current.Status != healthyStatus {
		return nil
	}
	// Stamp the withheld done marker onto the re-read row so out-of-band writes
	// (e.g. title) survive, then upsert that row rather than the by-value doc.
	current.ContentHash = contentHash
	if err := s.store.UpsertDocument(ctx, current); err != nil {
		return fmt.Errorf("finalize document content hash: %w", err)
	}
	// The done marker is durable now, so the group key is safe to publish (#691).
	s.notifyDocumentContentHash(current.RelPath, current.ContentHash)
	return nil
}

// creditInitialStatus applies the #426 initial-status accounting for a freshly
// built doc. A doc that is ok now but whose representation generation fails later
// must count solely as an error (runScan adds it), not as both indexed and error
// — otherwise the same doc is double-counted and indexed+skipped+errors exceeds
// scanned (issue #426). It returns indexedPending (the ok case is credited later,
// only past the exit points where no rep work can still fail) and terminal (a
// terminal status already fully counted, so processDocument should return early).
func (s *Service) creditInitialStatus(doc model.Document) (indexedPending, terminal bool) {
	switch doc.Status {
	case "ok":
		return true, false
	case "skipped", "secret_excluded":
		s.addSkipped(1)
		// A file skipped because it classified as a non-textual binary carries the
		// canonical §14.4 BINARY_SKIPPED code on its manifest entry; every other
		// skip (cache hit, unchanged content, ignore/archive type) stays a plain
		// skip with no code.
		if doc.DocType == "binary_ignored" {
			s.activeOutcome.markSkippedWithCode(manifestErrBinarySkipped)
		} else {
			s.markActiveSkipped()
		}
		// An archive container is credited as skipped here but is NOT terminal: it
		// reverts to an error (and addSkipped(-1)) if member extraction fails, so
		// its file_skip is deferred until handleArchiveDocument succeeds.
		//
		// A size_cap document never reaches this function (#682): settleSizeCapSkip
		// counts it and raises its event, precisely so an over-cap ARCHIVE is not
		// handed to the deferred emitter that only member extraction would reach.
		if doc.DocType != "archive" {
			s.notifyDocumentSkip(doc)
		}
		return false, false
	case "error":
		// although buildDocumentWithContent will never return a document with
		// Status="error" (the error case returns early in processDocument), we
		// leave this branch in place as a defensive measure. future changes to
		// document construction might introduce new terminal statuses and it's
		// nicer to handle them explicitly here rather than silently falling
		// through.
		s.addErrors(1)
		return false, true
	}
	return false, false
}

// creditIndexed bumps the run-progress "indexed" counter when the document was
// durably processed (status ok). Kept as a helper so processDocument credits the
// counter only at the exit points past which no representation work can still
// fail — a doc that later errors during rep-gen must count solely as an error,
// not as both indexed and error (issue #426).
func (s *Service) creditIndexed(indexed bool) {
	if indexed {
		s.addIndexed(1)
	}
}

// suppressIndexedCredit reports whether the document just processed must NOT be
// credited as indexed because a non-fatal soft error already counted it as an
// error (issue #426). Two sources: (1) nonFatalErrored — a rep-generation
// soft-failure (binary-content, video-no-representation, or a zero-representation
// provider failure) returned by generateRepresentations; (2) quarantinedThisDoc —
// an output quality gate (§8.6.6) that rejected a generated OCR/transcript/
// translation output. Either way the document counts solely as an error.
// (3) secretExcludedThisDoc: a derived text matched a configured secret pattern
// (#681), so the document was withheld and counted as a skip. processDocument
// returns before this on that path, so the guard here is a belt-and-braces
// statement of the same rule for any future caller.
func (s *Service) suppressIndexedCredit(nonFatalErrored bool) bool {
	return nonFatalErrored || s.quarantinedThisDoc || s.secretExcludedThisDoc
}

// deriveDocument runs the derivation pass for a single asset under the two-phase
// split (SPEC §8.6.11): it produces ONLY translated transcripts (§8.6.2) from the
// already-persisted source transcript, reusing the cached transcript text so it
// never re-transcribes — which is what makes the derivation pass independently
// resumable (§7.6/§8.6.7). All document building, upserting, counting, and
// source-transcript work was already done by the transcription pass.
//
// It mirrors the gating of the single-pass translation step exactly so the final
// set of representations is observably identical: translation applies only to a
// model-derived (STT) transcript of an audio document, and only when translation
// is configured. Non-media documents, sidecar-transcript documents, and assets
// with no source transcript are recorded as skipped and produce nothing — keeping
// the derivation pass's per-asset manifest/progress totals faithful (§8.6.11).
func (s *Service) deriveDocument(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp) error {
	// The per-document quality-gate quarantine flag (§8.6.6 / #426) was already
	// reset at processDocument entry (deriveDocument is reached only from there),
	// so a translation rejected in this derivation pass is counted once and cannot
	// leak its dedup state into the next asset without a redundant reset here.
	// No translation configured, or the multimodal "replace" mode that stands in
	// for STT→text (so no transcript exists to translate): nothing to derive. This
	// matches the single-pass gates so the corpus-wide output is identical.
	if !s.translationConfigured() {
		s.markActiveSkipped()
		return nil
	}

	docType := ClassifyDocType(f.RelPath)
	// Audio AND video carry model-derived transcripts (issue #495): a video's
	// source transcript was produced from its extracted audio track in the
	// transcription pass, so its translation is derived here exactly like audio.
	if !isSidecarMediaType(docType) || s.transcriber == nil {
		s.markActiveSkipped()
		return nil
	}

	doc, err := s.store.GetDocumentByPath(ctx, f.RelPath)
	if isUnexpectedStoreErr(err) {
		return fmt.Errorf("get existing document: %w", err)
	}
	// A document the transcription pass did not record as a healthy media asset has
	// no source transcript to translate; skip it.
	if isNotFoundError(err) || doc.Status != "ok" {
		s.markActiveSkipped()
		return nil
	}
	s.recordContentHash(doc.ContentHash)

	content, err := s.readDocumentContent(ctx, f.RelPath)
	if err != nil {
		// A read failure in the derivation pass must not fail the run: the source
		// transcript already exists. Record skipped and move on.
		s.getLogger().Printf("two-phase derivation: read %s: %v (skipping translation)", f.RelPath, err)
		s.markActiveSkipped()
		return nil
	}

	if err := s.deriveTranscriptTranslations(ctx, doc, content); err != nil {
		s.getLogger().Printf("two-phase derivation translation skipped for %s: %v", f.RelPath, err)
		s.addErrors(1)
		// §8.6.11/§14.4: record the canonical translation-failure code
		// (TRANSLATE_FAILED for a provider failure) on the derivation-pass manifest
		// record; the source transcript (persisted in the transcription pass) stays
		// searchable, so documents.status is untouched. No-op with no batch run.
		code := manifestErrorCode(err)
		s.markActiveErrored(code, code+": transcript translation failed")
	}
	return nil
}

// deriveTranscriptTranslations recomputes each selected track's source transcript
// (a cache hit, so no re-transcription) and translates it, reusing the SAME
// derivation logic and trim/alignment as the single-pass path so the resulting
// translated transcript representations and chunks are byte-identical to single-pass
// (SPEC §8.6.11). Under a multi-track selection (§8.6.12) it derives the translations
// for every selected track under its transcript@t<N>-<lang> keys; the default single
// track is the degenerate one-track case.
func (s *Service) deriveTranscriptTranslations(ctx context.Context, doc model.Document, content []byte) error {
	sel, err := config.ParseSTTTracks(s.cfg.MediaSTTTracks)
	if err != nil {
		return err
	}

	var duration time.Duration
	if d, derr := s.probeDuration(ctx, doc); derr == nil {
		duration = d
	}
	// Recompute the same leading-silence trim offset the transcription pass applied
	// to the source transcript so translated time windows stay aligned (dir2mcp#258
	// / SPEC §8.6.2). Detection is deterministic for an unchanged asset.
	trimOffsetMS := 0
	if s.cfg.MediaTrimLeadingSilence {
		if offset := s.detectLeadingSilence(ctx, doc); offset > 0 {
			trimOffsetMS = int(offset.Milliseconds())
		}
	}

	if sel.Mode == config.STTTracksFirst {
		return s.deriveOneTrackTranslations(ctx, doc, content, trackContext{audioIndex: 0}, duration, trimOffsetMS)
	}
	indices := resolveTrackIndices(sel, s.probeTrackInfo(ctx, doc))
	if len(indices) == 0 {
		return s.deriveOneTrackTranslations(ctx, doc, content, trackContext{audioIndex: 0}, duration, trimOffsetMS)
	}
	var firstErr error
	for _, n := range indices {
		if derr := s.deriveOneTrackTranslations(ctx, doc, content, trackContext{audioIndex: n}, duration, trimOffsetMS); derr != nil && firstErr == nil {
			firstErr = derr
		}
	}
	return firstErr
}

// deriveOneTrackTranslations reads a single track's cached source transcript and
// produces its per-language translations (SPEC §8.6.2/§8.6.12). A cache miss here
// means the track produced no source transcript (empty/failed in the transcription
// pass), which is a legitimate no-op — never a re-transcription.
func (s *Service) deriveOneTrackTranslations(ctx context.Context, doc model.Document, content []byte, tc trackContext, duration time.Duration, trimOffsetMS int) error {
	transcriptText, _, err := s.readTrackTranscript(ctx, doc, content, tc)
	if err != nil {
		return err
	}
	transcriptText = strings.TrimSpace(transcriptText)
	if transcriptText == "" {
		return nil
	}
	return s.translateTranscriptRepresentations(ctx, doc, content, transcriptText, duration, trimOffsetMS, tc.audioIndex)
}

// readDocumentContent reads an asset's bytes through the corpus filesystem,
// localizing remote (object-store) objects as needed. Used by the two-phase
// derivation pass, which needs the source bytes only to key the per-language
// translation cache.
//
// The read is bounded by the configured cap (#682). An over-cap asset returns
// ErrFileTooLarge rather than a truncated buffer: a short read would produce a
// different cache key and quietly re-derive every translation for a file the
// transcription pass keyed on the whole bytes. deriveDocument treats any read
// failure as a skip and leaves the existing source transcript searchable, which
// is the right outcome here too.
func (s *Service) readDocumentContent(ctx context.Context, relPath string) ([]byte, error) {
	content, overCap, err := s.readSourceBytes(ctx, relPath)
	if err != nil {
		return nil, err
	}
	if overCap {
		return nil, s.sourceOverCapError(relPath)
	}
	return content, nil
}

// handleArchiveDocument handles an archive-type document: if the archive
// content changed (or a full reindex was requested) it extracts and processes
// the members; otherwise it retains the existing member paths in seen so that
// markMissingAsDeleted does not tombstone them.
func (s *Service) handleArchiveDocument(ctx context.Context, doc model.Document, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}, needsProcessing bool, finalContentHash string) error {
	if needsProcessing {
		complete, err := s.processArchiveMembers(ctx, f, secretPatterns, forceReindex, seen)
		if err != nil {
			if !isDurableArchiveFailure(err) {
				// Context cancellation (shutdown mid-archive) and any other
				// non-sentinel error must propagate WITHOUT persisting a durable
				// "error" status or mutating counters: processArchiveMembers returns
				// ctx.Err() on cancellation, and recording that as a hard per-document
				// failure would corrupt CorpusStats and wrongly flag the container.
				//
				// This "persist only for a known durable sentinel" guard is what keeps
				// cancellation from writing a status="error"; it is
				// verified by static reasoning rather than a test. Triggering the
				// member-loop cancellation path deterministically needs a call-count
				// context tuned to the exact ctx.Err() sequence (LocalFS.Localize
				// checks ctx before the loop, so a pre-cancelled context is swallowed
				// as a non-fatal skip and never reaches the loop). That white-box hook
				// only works from inside this package against handleArchiveDocument
				// directly; the black-box tests/ingest suite (which drives the public
				// Service.Run) cannot reach it without flakiness, so it is intentionally
				// not covered by an external test.
				return err
			}
			// #398: an unsupported/unextractable archive (.xz/.7z/.rar) must not be
			// silently ingested as an empty skipped document. #658: an archive that
			// could not be read in full must not either. Persist the container
			// itself as status="error" so it is durably visible via status queries and
			// retried on the next incremental run, mirroring the representation-failure
			// path in processDocument.
			doc.Status = "error"
			doc.ErrorMessage = RedactSecretsInMessage(err.Error(), secretPatterns)
			if upErr := s.store.UpsertDocument(ctx, doc); upErr != nil {
				// Log and continue, mirroring persistNonFatalDocError: the original
				// archive error is the more actionable signal, but a persistence
				// failure here would otherwise be invisible and could leave the
				// archive looking like a silent skip again.
				s.getLogger().Printf("persist error status for %s: %v", f.RelPath, upErr)
			} else {
				// Keep the live dedup map equal to the row this write produced (#691).
				s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
			}
			// processDocument already counted this archive as skipped (archive
			// containers build as status="skipped"); scanPass will count the error
			// returned below separately. Drop the skipped tally so the same file is
			// not double-counted as both skipped and error in CorpusStats (#398).
			s.addSkipped(-1)
			return fmt.Errorf("process archive members %s: %w", f.RelPath, err)
		}
		if !complete {
			// #502 (member granularity): a member failed, or a localize/extraction
			// failure left the archive "skipped". Extraction did not FULLY complete,
			// so leave the container's content_hash withheld (blank, from the initial
			// upsert) — the next incremental scan re-extracts and retries the failed
			// members. The members that DID succeed this run are already ingested
			// (#398 best-effort); the only consequence is a re-extraction next scan
			// until every member is durably processed. Stamping the done marker here
			// would permanently skip the failed members on subsequent scans.
			return nil
		}
		// #502: member extraction FULLY completed — stamp the withheld content_hash
		// done marker now. The initial upsert blanked it, so a crash anywhere between
		// that upsert and this point leaves the container with an empty hash and the
		// next scan re-extracts instead of skipping on a premature marker. The
		// container persists as status="skipped", so finalize preserves that healthy
		// status.
		if err := s.finalizeContentHashForStatus(ctx, &doc, finalContentHash, "skipped"); err != nil {
			return err
		}
	} else if seen != nil {
		// Archive content unchanged: retain existing members in seen.
		s.retainArchiveMembers(ctx, f.RelPath, seen)
	}
	return nil
}

// isDurableArchiveFailure reports whether an archive failure must be persisted
// on the container as status="error" rather than merely propagated.
//
// Both members of the set describe the archive's own content: the format cannot
// be extracted at all (#398), or the bytes could not be read in full (#658).
// Both are worth an operator's attention and both are retried on the next scan.
// Everything else (notably context cancellation at shutdown) is about the RUN,
// not the file, and must never brand the container.
func isDurableArchiveFailure(err error) bool {
	return errors.Is(err, errUnsupportedArchiveFormat) || errors.Is(err, errArchiveUnreadable)
}

// archiveMaxMembersEff resolves the effective per-archive member-count cap:
// the Service override when positive, else the package default (#408).
func (s *Service) archiveMaxMembersEff() int {
	if s.ArchiveMaxMembers > 0 {
		return s.ArchiveMaxMembers
	}
	return archiveMaxMembers
}

// archiveMaxTotalBytesEff resolves the effective per-archive aggregate-bytes cap:
// the Service override when positive, else the package default (#408).
func (s *Service) archiveMaxTotalBytesEff() int64 {
	if s.ArchiveMaxTotalBytes > 0 {
		return s.ArchiveMaxTotalBytes
	}
	return archiveMaxTotalBytes
}

// processArchiveMembers extracts members from an archive and ingests each one
// as an independent document. One bad member is logged and skipped without
// aborting the rest (#398 best-effort).
//
// It returns complete=true only when extraction FULLY succeeded: the archive
// localized, extracted without error, and every member was durably processed.
// complete=false signals the caller to leave the container's content_hash
// withheld (blank) so the next incremental scan re-extracts and retries — the
// members that DID succeed this run are still ingested, but a container whose
// members did not all land must not be stamped "done", or the failed members
// would be permanently skipped on subsequent scans (the #502 crash window, at
// member granularity). Tradeoff: a partially-failing archive is re-extracted on
// every scan until all its members succeed (successful members are cheap cache
// hits; only the failed ones do real work). A non-nil error is a hard per-archive
// failure (an unsupported format, an unreadable archive, or context cancellation)
// the caller persists/propagates separately; complete is meaningless when
// err != nil.
func (s *Service) processArchiveMembers(ctx context.Context, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}) (complete bool, err error) {
	// Archive readers need a real filesystem path. Localize returns the in-root
	// path for a local corpus (no-op cleanup) or a temp download for an object
	// store, so this works uniformly across backends.
	localPath, cleanup, err := s.corpusFS().Localize(ctx, f.RelPath)
	if err != nil {
		s.getLogger().Printf("archive localize %s: %v", f.RelPath, err)
		// A localize failure is about the transport (an S3 download, a disk read),
		// not about the archive's content, so it is non-fatal and unbranded: the
		// archive stays "skipped" and its content_hash must not be finalized so
		// the next scan retries.
		return false, nil
	}
	defer cleanup()
	extraction, err := extractArchiveMembers(localPath, f.RelPath, s.archiveMaxMembersEff(), s.archiveMaxTotalBytesEff())
	if err != nil {
		return false, archiveExtractionError(f.RelPath, err)
	}
	s.reportArchiveExtraction(f.RelPath, extraction)
	allMembersOK, err := s.ingestArchiveMembers(ctx, f, extraction.Members, secretPatterns, forceReindex, seen)
	if err != nil {
		return false, err
	}
	// Members and exclusions never share a rel_path (see memberAccumulator.result),
	// so this pass cannot overwrite a row the pass above just wrote.
	skipsPersisted := s.persistArchiveMemberSizeCapSkips(ctx, f, extraction.Excluded, seen)
	if extraction.Unreadable > 0 {
		// #658: at least one declared member could not be opened or read. The
		// members that did read are ingested above (best-effort), but the container
		// must not look healthy. Report it as a durable per-archive failure so the
		// gap is visible and the next scan retries it.
		return false, fmt.Errorf("archive %s: %d declared member(s) could not be read: %w", f.RelPath, extraction.Unreadable, errArchiveUnreadable)
	}
	// Truncation (#408) is a deliberate, deterministic cap. The dropped members
	// can never be recovered by re-extraction, so it does NOT withhold the marker
	// (that would re-extract on every scan forever with no benefit). The same is
	// true of the per-member size cap (#683), which is why its members are
	// persisted as durable size_cap skips instead. A size_cap row that failed to
	// persist is the exception: the omission then has no durable record, so the
	// container must stay incomplete and re-extract until the row lands. Only
	// that, and genuine per-member failures, leave the archive incomplete.
	return allMembersOK && skipsPersisted, nil
}

// archiveExtractionError classifies an extractor failure into the durable
// sentinel the caller brands the container with.
//
// Both outcomes are about the file itself, so both are persisted on the
// container and retried. Before #658 the second branch returned no error at all:
// an archive whose bytes could not be opened or decompressed was logged once and
// then looked like an ordinary empty archive.
func archiveExtractionError(archiveRelPath string, err error) error {
	if errors.Is(err, errUnsupportedArchiveFormat) {
		// #398: .xz/.7z/.rar (and any other classified-but-unextractable
		// container) were being silently ingested as empty skipped documents:
		// known but unsearchable, with zero diagnostics.
		return fmt.Errorf("unsupported archive format for %s: %w", archiveRelPath, err)
	}
	return fmt.Errorf("extract archive %s: %w: %w", archiveRelPath, err, errArchiveUnreadable)
}

// reportArchiveExtraction logs the two extraction-wide diagnostics: the #408
// fan-out truncation warning and the #718 per-member skip record. Split out of
// processArchiveMembers to keep that function within the complexity budget.
func (s *Service) reportArchiveExtraction(archiveRelPath string, extraction archiveExtraction) {
	if extraction.Truncated {
		// #408 decompression-bomb guard: extraction stopped once the archive hit
		// the member-count or aggregate-uncompressed-size cap. The members read
		// before the cap are still ingested; surface a clear warning so the
		// truncation is visible rather than a silent partial ingest.
		s.getLogger().Printf(
			"archive %s: member fan-out exceeded caps (max_members=%d, max_total_bytes=%d); ingesting the first %d member(s), remaining entries skipped (#408)",
			archiveRelPath, s.archiveMaxMembersEff(), s.archiveMaxTotalBytesEff(), len(extraction.Members),
		)
	}
	s.logArchiveMemberSkips(archiveRelPath, extraction.Skips)
}

// ingestArchiveMembers ingests each extracted member as an independent document.
// One bad member is logged and does not abort the rest (#398 best-effort); it
// only clears the allOK result so the container's done marker stays withheld.
// A non-nil error is context cancellation, which must abort the whole archive.
func (s *Service) ingestArchiveMembers(ctx context.Context, f DiscoveredFile, members []archiveMember, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}) (allOK bool, err error) {
	allOK = true
	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := s.processDocumentFromContent(ctx, m.RelPath, m.Content, f.MTimeUnix, secretPatterns, forceReindex); err != nil {
			s.getLogger().Printf("archive member %s: %v", m.RelPath, err)
			allOK = false // extraction did not fully complete; retry next scan
			// continue with next member (#398 best-effort)
		}
		if seen != nil {
			seen[m.RelPath] = struct{}{}
		}
	}
	return allOK, nil
}

// persistArchiveMemberSizeCapSkips upserts a durable status=skipped row, with
// skip_reason=size_cap, for each archive member the per-member size cap left out
// (#683).
//
// Before this, an oversized member was a log line and nothing else. The archive
// finished, its content_hash was stamped, and the member simply was not in the
// corpus: an operator could not tell an archive that never held the file from
// one whose only member was 11 MiB, and no later incremental scan retried it
// (nor could one, since the cap is deterministic). The row makes the omission
// part of the honest-coverage aggregate (SPEC §7.7 skip_reasons), which survives
// the run that produced it, so the container may still be finalized as done.
//
// size_cap is the spec's own enum value for "exceeded the configured max file
// size". The enum is CLOSED for a given spec minor, so this path reuses it
// rather than inventing an archive-specific reason.
//
// The row is deliberately not reported through notifyDocumentSkip. Archive
// members are not credited to the run's scanned/skipped counters at all (the
// container accounts for the archive), and SPEC §3.2 ties the number of
// file_skip events to the run's terminal indexing.skipped. Emitting one here
// would break that equality. This matches how a nested archive member is
// already persisted as skipped without an event.
//
// A failed upsert does not abort the other exclusions, mirroring
// persistOversizeSkips, but it IS reported: the return value is false, and the
// caller then leaves the container incomplete so the next scan re-extracts and
// writes the row again. Logging alone would let the container be stamped done
// with the omission recorded nowhere, which is the very bug this path closes.
// Each path is registered in seen so markMissingAsDeleted does not tombstone
// the row that was just written.
func (s *Service) persistArchiveMemberSizeCapSkips(ctx context.Context, f DiscoveredFile, excluded []archiveMemberExclusion, seen map[string]struct{}) (allPersisted bool) {
	allPersisted = true
	for _, ex := range excluded {
		if seen != nil {
			seen[ex.RelPath] = struct{}{}
		}
		doc := model.Document{
			RelPath:    ex.RelPath,
			DocType:    ClassifyDocType(ex.RelPath),
			SourceType: "archive_member",
			// The archive's own mtime, matching every other member row: the member
			// content came from this archive at this revision.
			MTimeUnix: f.MTimeUnix,
			// The size the archive declared, or 0 when the format declares none.
			// Zero means "unknown", never "empty".
			SizeBytes:  ex.SizeBytes,
			Status:     "skipped",
			SkipReason: model.SkipReasonSizeCap,
		}
		if err := s.store.UpsertDocument(ctx, doc); err != nil {
			s.getLogger().Printf("archive %s: persist size-cap skip row for %s: %v", f.RelPath, ex.RelPath, err)
			allPersisted = false
		} else {
			// The row now carries no content_hash, so dedup must forget the path
			// rather than keep grouping it on the content it held before (#691).
			s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
		}
	}
	return allPersisted
}

// logArchiveMemberSkips surfaces members an archive declared but that did not
// become documents (#718). A refused member is otherwise invisible: the archive
// indexes fine, the member is simply absent, and nothing tells the operator the
// difference between "the archive did not contain it" and "dir2mcp refused the
// name". Log-only, matching the OnUnsafeKey precedent (#735): a refused name has
// no usable rel_path to key a skipped document row on (the unusable name IS the
// finding), so it is not counted as a corpus skip either.
//
// This is the log of every skip, including those that DO get a durable record
// elsewhere: a size-cap exclusion also becomes a status=skipped row (#683), and
// an unreadable member also brands the container status=error (#658). The log
// keeps naming them so one place still answers "what did this archive declare
// that did not become a document".
//
// Each member is named with its reason, up to the bounded sample the extractor
// retained; the count is always exact.
func (s *Service) logArchiveMemberSkips(archiveRelPath string, skips archiveSkips) {
	if skips.Total == 0 {
		return
	}
	details := make([]string, 0, len(skips.Entries))
	for _, e := range skips.Entries {
		details = append(details, fmt.Sprintf("%q (%s)", e.Name, e.Reason))
	}
	suffix := ""
	if skips.Total > len(skips.Entries) {
		suffix = fmt.Sprintf(" and %d more", skips.Total-len(skips.Entries))
	}
	s.getLogger().Printf(
		"archive %s: %d member(s) not indexed: %s%s (#718)",
		archiveRelPath, skips.Total, strings.Join(details, "; "), suffix,
	)
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

// buildArchiveMemberDocument assembles the document row for one extracted
// archive member and applies the two terminal statuses decided from the member's
// own bytes: a nested archive is a `skipped` container, and a member whose
// scanned bytes match a configured secret pattern is `secret_excluded`.
//
// The scan target follows the same "scan what becomes searchable" rule as the
// top-level path (secretScanBytes): a member whose raw bytes are its indexable
// text is scanned in full, and a member that reaches the index through a derived
// text is head-sampled here and scanned in full when that text is produced.
//
// Split out of processDocumentFromContent to keep that function within the
// cyclomatic-complexity budget.
func buildArchiveMemberDocument(
	relPath, docType string,
	content []byte,
	mtimeUnix int64,
	skipExtraction bool,
	secretPatterns []*regexp.Regexp,
) model.Document {
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
		doc.SkipReason = skipReasonForDocType(docType)
		return doc
	}
	if hasSecretMatch(secretScanBytes(docType, content), secretPatterns) {
		doc.Status = "secret_excluded"
		doc.SkipReason = model.SkipReasonSecretExcluded
	}
	return doc
}

// processDocumentFromContent ingests a document whose content is already in
// memory (e.g. an archive member). relPath is the virtual path stored in the
// documents table; mtimeUnix is inherited from the parent archive.
func (s *Service) processDocumentFromContent(ctx context.Context, relPath string, content []byte, mtimeUnix int64, secretPatterns []*regexp.Regexp, forceReindex bool) error {
	// Each archive member is an independent document, so it opens its own
	// per-document secret scope (#681) even though the members of one archive run
	// under a single processDocument entry.
	s.beginDocumentSecretScope(secretPatterns)
	docType := ClassifyDocType(relPath)
	// Never ingest binary or ignored artifacts from inside archives.
	if docType == "binary_ignored" || docType == "ignore" {
		return nil
	}
	// Nested archive files are persisted as skipped document rows, but are not
	// recursively extracted.
	skipExtraction := docType == "archive"

	doc := buildArchiveMemberDocument(relPath, docType, content, mtimeUnix, skipExtraction, secretPatterns)

	existingDoc, err := s.store.GetDocumentByPath(ctx, relPath)
	if err != nil && !isNotFoundError(err) {
		return fmt.Errorf("get existing document: %w", err)
	}
	// #681: same carry as the top-level path, for the same reason. A member
	// withheld for a secret in its DERIVED text must not be written back as "ok"
	// when the container is re-extracted with the member's bytes unchanged.
	carrySecretExclusion(&doc, existingDoc)
	needsProcessing := s.resolveNeedsProcessing(ctx, existingDoc, doc, forceReindex)

	// Withhold the content_hash done marker until representations commit so an
	// ungraceful crash mid-ingest cannot leave an ok archive member with zero
	// chunks that is never reprocessed (#402; see withholdContentHash).
	finalContentHash, willGenerateReps := withholdContentHash(&doc, needsProcessing)

	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}
	// Same live-dedup report as processDocument, for an archive member (#691).
	s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
	if updated, err := s.store.GetDocumentByPath(ctx, relPath); err == nil {
		doc.DocID = updated.DocID
	} else if !isNotFoundError(err) {
		return fmt.Errorf("fetch document after upsert: %w", err)
	}

	if !needsProcessing || doc.Status != "ok" {
		return nil
	}
	// Archive members are not credited to the run's indexed counter here (the
	// container document accounts for the archive), so the non-fatal-error signal
	// is irrelevant on this path: a persisted soft-error already counted itself.
	if _, err := s.generateRepresentations(ctx, doc, content, secretPatterns, forceReindex); err != nil {
		// Persist error status so the next incremental run retries this document
		// and operators can identify it via status queries.  Silence the upsert
		// error since the original rep error is the more actionable signal.
		doc.Status = "error"
		doc.ErrorMessage = RedactSecretsInMessage(err.Error(), secretPatterns)
		_ = s.store.UpsertDocument(ctx, doc)
		return fmt.Errorf("generate representations: %w", err)
	}
	// #681: a derived text of this member matched a configured secret pattern, so
	// it is withheld as secret_excluded. Settle it under that status instead of the
	// "ok" one, which the #413 guard would refuse.
	if s.secretExcludedThisDoc {
		return s.finalizeSecretExcluded(ctx, &doc, finalContentHash)
	}
	// Stamp the withheld #402 done marker now the archive member's reps commit.
	if err := s.finalizeIfGenerated(ctx, &doc, willGenerateReps, finalContentHash); err != nil {
		return err
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

	content, overCap, err := s.readSourceBytes(ctx, f.RelPath)
	if err != nil {
		return doc, nil, fmt.Errorf("read %s: %w", f.RelPath, err)
	}
	if overCap {
		// #682: the bytes passed the cap, so this file is not the file discovery
		// admitted. Return the honest size_cap skip row and no content: nothing
		// downstream may see a prefix of a file it cannot have in full.
		return s.sizeCapSkippedDocument(doc, f), nil, nil
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
		doc.SkipReason = skipReasonForDocType(docType)
		return doc, content, nil
	}

	if hasSecretMatch(secretScanBytes(docType, content), secretPatterns) {
		doc.Status = "secret_excluded"
		doc.SkipReason = model.SkipReasonSecretExcluded
	}

	return doc, content, nil
}

// sizeCapSkippedDocument turns a document whose SOURCE READ passed the configured
// cap into the honest skip row for it (#682).
//
// The reason is `size_cap`: SPEC §15.2 defines it as "exceeded the configured max
// file size", which is exactly what happened. That enum is CLOSED for a spec
// minor, and reusing the value the spec already has for this condition is what
// keeps the honest-coverage aggregate reporting a real cause. A file caught here
// and a file caught by the discovery stat (#497) were refused by the same policy,
// so they must read the same in `skip_reasons`.
//
// SizeBytes is deliberately zeroed. All this path knows is "more than the cap":
// it stopped reading at cap+1 by design, and to keep reading until the end just
// to measure the file would spend the very resources the bound exists to save.
// The size discovery recorded is worse than nothing, because it describes a
// snapshot the read has just proved wrong: a row saying "5 KB, skipped for
// size_cap" is a contradiction an operator cannot act on. Zero means unknown
// here, never empty, matching persistSymlinkSkips and the archive-member
// exclusions.
//
// content_hash and ETag are cleared for the same reason: both are identity claims
// about bytes this run did not obtain. A blank content_hash also keeps the
// incremental gate honest: the next scan re-reads and decides again, so a file
// that shrinks back under the cap is indexed instead of staying skipped forever,
// and the remote ETag fast path cannot skip the re-read.
//
// The log line names the path, the cap, and the size discovery measured, so the
// growth or the misreport is visible. It carries no file content.
func (s *Service) sizeCapSkippedDocument(doc model.Document, f DiscoveredFile) model.Document {
	s.getLogger().Printf(
		"ingest: skipping %s: its bytes passed the ingest.max_file_mb cap (%d bytes) during the read, although discovery measured %d bytes; recorded as a size_cap skip. Raise ingest.max_file_mb to include it",
		f.RelPath, s.sourceReadCapBytes(), f.SizeBytes,
	)
	doc.Status = "skipped"
	doc.SkipReason = model.SkipReasonSizeCap
	doc.SizeBytes = 0
	doc.ContentHash = ""
	doc.ETag = ""
	doc.SidecarFingerprint = ""
	doc.Title = ""
	return doc
}

// isSizeCapSkip reports whether doc is the size_cap skip row
// sizeCapSkippedDocument produced. It is the marker processDocument routes on, and
// it is deliberately narrow: no other builder pairs status="skipped" with
// skip_reason="size_cap".
func isSizeCapSkip(doc model.Document) bool {
	return doc.Status == "skipped" && doc.SkipReason == model.SkipReasonSizeCap
}

// settleSizeCapSkip is the whole terminal handling of a document whose SOURCE READ
// passed the cap (#682): retire, persist, count, report. It is the read-time
// counterpart of persistOversizeSkips, which does the same four things for a file
// the discovery stat dropped (#497), and the two must agree, because they write the
// same skip reason.
//
// Terminal by design. Everything processDocument does after this point reasons
// about content this run did not obtain: the incremental gate, the derivation
// identity, representation generation, output reconciliation. The most important of
// them is the archive branch, which runs for a `skipped` container: without a
// terminal return, an over-cap ARCHIVE would be localized and expanded, and members
// would be ingested out of a container whose own bytes were just refused. A file
// dropped by the discovery stat never reaches that branch, and this one must not
// either.
//
// The order is load-bearing, and so is the refusal to continue past a failed
// retirement. Representations are retired FIRST, so an ungraceful death between the
// two writes leaves a document with nothing searchable rather than one whose row
// reads "skipped for size_cap" while its chunks are still being served. A store
// that REFUSES the retirement would produce that same contradiction deliberately,
// so the row is not written at all: the document keeps the consistent state it
// already had, the run reports one error, and the next scan retries. The verdict is
// reproducible (the file is still over the cap, or it is not), so a retry costs a
// re-read and nothing more.
//
// The counters are the ones creditInitialStatus would have applied to a skipped
// document, with one difference that is the point of doing it here: the event is
// raised unconditionally. creditInitialStatus defers an archive container's
// file_skip until member extraction finishes, and for this document extraction
// never runs, so the deferred event would never be raised and the run would report
// one more skip than it raised events for (SPEC §3.2).
//
// The batch manifest entry carries the canonical §14.4 FILE_TOO_LARGE code, exactly
// as the discovery-time drop already does (runScan records it for every
// pendingOversize path). A plain, codeless skip would leave the manifest unable to
// tell this asset from a cache hit.
func (s *Service) settleSizeCapSkip(ctx context.Context, doc model.Document) error {
	if err := s.retireRepresentationsForPath(ctx, doc.RelPath, "size cap"); err != nil {
		return err
	}
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}
	// The row carries no content_hash, so cross-file dedup must forget the path
	// instead of grouping it on the content it held before (#691).
	s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
	s.addSkipped(1)
	s.activeOutcome.markSkippedWithCode(manifestErrFileTooLarge)
	s.notifyDocumentSkip(doc)
	return nil
}

// persistOversizeSkips upserts a minimal skipped document row for each file
// dropped at discovery for exceeding the ingest size cap (#497), stamping
// skip_reason=size_cap so CorpusStats.SkipSummary can report it. Each path is
// also registered in seen so markMissingAsDeleted (which runs after the scan)
// does not tombstone the freshly-persisted row. Best-effort per path: a failed
// upsert is logged and skipped rather than aborting the whole scan.
//
// It also retires anything an earlier scan indexed for the path (#682). A file
// that grew past the cap was indexed while it was smaller, and the row this
// writes carries no content_hash, so without the retirement the corpus would
// report the file as skipped for size_cap while still serving chunks of its old
// content. That is the same contradiction the read-time path avoids, so both
// paths resolve it the same way. It is a no-op (one empty list query) for a file
// that was never indexed, which is the common case.
//
// A REFUSED retirement skips the row for that path rather than writing the
// contradiction on purpose. The path keeps the consistent state it already had,
// and it stays in `seen`, so markMissingAsDeleted does not tombstone it either.
// The cap is deterministic, so the next scan drops the same file again and writes
// the row once the store accepts the retirement. Retrying until the record lands
// is the same choice persistArchiveMemberSizeCapSkips makes for its own rows.
func (s *Service) persistOversizeSkips(ctx context.Context, oversize map[string]int64, seen map[string]struct{}) {
	for relPath, size := range oversize {
		seen[relPath] = struct{}{}
		if err := s.retireRepresentationsForPath(ctx, relPath, "size cap"); err != nil {
			s.getLogger().Printf(
				"discovery: leaving %s as it was; its representations could not be retired, and a size_cap row beside live chunks would misreport coverage: %v",
				relPath, err,
			)
			continue
		}
		doc := model.Document{
			RelPath:    relPath,
			DocType:    ClassifyDocType(relPath),
			SizeBytes:  size,
			Status:     "skipped",
			SkipReason: model.SkipReasonSizeCap,
		}
		if err := s.store.UpsertDocument(ctx, doc); err != nil {
			s.getLogger().Printf("discovery: persist size-cap skip row for %s: %v", relPath, err)
		} else {
			// A file that grew past the cap keeps no content_hash, so dedup must
			// forget the path instead of grouping on stale content (#691).
			s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
		}
		// Emitted here rather than at the OnOversize counter bump so the event and
		// the persisted row stay one-to-one; the oversize map is keyed by relPath,
		// so no document raises it twice.
		s.notifyDocumentSkip(doc)
	}
}

// persistSymlinkSkips upserts a minimal skipped document row for each entry
// dropped at discovery for being a symbolic link while ingest.follow_symlinks
// is false (#781), stamping skip_reason=symlink_ignored so
// CorpusStats.SkipSummary reports it and §3.2's "one file_skip event per
// terminal skipped" invariant holds.
//
// It mirrors persistOversizeSkips step for step, including the seen
// registration that stops markMissingAsDeleted from tombstoning the row it
// just wrote, and the best-effort per-path error handling.
//
// SizeBytes is deliberately left zero. With following off the walker never
// resolves the target, so the only size available is the link's own inode
// size, which describes the path string rather than any content. Reporting
// that as the document size would be worse than reporting nothing.
func (s *Service) persistSymlinkSkips(ctx context.Context, symlinks map[string]struct{}, seen map[string]struct{}) {
	for relPath := range symlinks {
		seen[relPath] = struct{}{}
		doc := model.Document{
			RelPath:    relPath,
			DocType:    ClassifyDocType(relPath),
			Status:     "skipped",
			SkipReason: model.SkipReasonSymlinkIgnored,
		}
		if err := s.store.UpsertDocument(ctx, doc); err != nil {
			s.getLogger().Printf("discovery: persist symlink skip row for %s: %v", relPath, err)
		} else {
			// Same rule as the size-cap row: no content_hash, no group key (#691).
			s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
		}
		// Emitted even when the upsert failed, matching the size-cap path. The
		// skipped counter was already bumped at discovery, and SPEC §3.2 ties
		// the file_skip event count to the terminal indexing.skipped value. To
		// return early here would keep the count and drop the event, so a
		// failed row would break that invariant instead of only costing the
		// coverage aggregate one entry. The map is keyed by relPath, so one
		// link never raises two events.
		s.notifyDocumentSkip(doc)
	}
}

// skipReasonForDocType maps a never-ingested doc_type classification to its
// stable model.SkipReason. It is the single source of truth for the
// doc_type→skip_reason mapping so the top-level scan and archive-member paths
// agree. An unrecognized type falls back to unsupported_format.
func skipReasonForDocType(docType string) string {
	switch docType {
	case "archive":
		return model.SkipReasonArchive
	case "binary_ignored":
		return model.SkipReasonBinaryIgnored
	case "ignore":
		return model.SkipReasonIgnoreRule
	default:
		return model.SkipReasonUnsupportedFormat
	}
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
	s.notifyDocumentsDeleted(deletedPaths)
	return nil
}

// notifyDocumentsDeleted raises the onDocumentsDeleted callback once for a
// batch of tombstoned paths, so the retrieval layer can evict them in one pass.
// An empty batch and an unset callback are both no-ops. A panic in the callback
// is contained and counted, because a bad consumer must not abort a scan or a
// watch loop.
func (s *Service) notifyDocumentsDeleted(relPaths []string) {
	if len(relPaths) == 0 {
		return
	}
	s.onDocumentsDeletedMu.RLock()
	onDeleted := s.onDocumentsDeleted
	s.onDocumentsDeletedMu.RUnlock()
	if onDeleted == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.addErrors(1)
			s.getLogger().Printf("onDocumentsDeleted panic for %d paths (%s)", len(relPaths), safePanicValue(r))
		}
	}()
	onDeleted(append([]string(nil), relPaths...))
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

// addRunSkipReason increments the in-run, non-persisted per-reason skip counter
// (currently only path-excludes). Safe for concurrent callers, though the scan
// loop is sequential today.
func (s *Service) addRunSkipReason(reason string) {
	s.runSkipReasonsMu.Lock()
	defer s.runSkipReasonsMu.Unlock()
	if s.runSkipReasons == nil {
		s.runSkipReasons = map[string]int64{}
	}
	s.runSkipReasons[reason]++
}

// SkipReasonCounts returns a copy of the in-run per-reason skip counts recorded
// during the most recent scan (path-excludes). These are NOT persisted, so they
// reflect only work this process performed; a fresh `status` process reading the
// store will not see them. Returns nil when nothing was recorded.
func (s *Service) SkipReasonCounts() map[string]int64 {
	s.runSkipReasonsMu.Lock()
	defer s.runSkipReasonsMu.Unlock()
	if len(s.runSkipReasons) == 0 {
		return nil
	}
	out := make(map[string]int64, len(s.runSkipReasons))
	for k, v := range s.runSkipReasons {
		out[k] = v
	}
	return out
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
func (s *Service) generateExtractedAndMediaRepresentations(ctx context.Context, doc model.Document, content []byte, secretPatterns []*regexp.Regexp) (mediaProduced, nonFatalErrored bool, err error) {
	// A non-empty span set means media chunks will be produced. In `replace` we
	// then skip OCR; in `augment` OCR is kept alongside.
	spans := s.mediaSpansFor(ctx, doc, content)
	mediaProduced = len(spans) > 0
	skipOCR := s.embedMultimodal == "replace" && mediaProduced

	if ShouldGenerateExtractedMarkdown(doc.DocType) && (s.extractor != nil || s.pandocExtractor != nil) && !skipOCR {
		if s.extractorCanReadExt(doc.RelPath) {
			if err := s.generateOCRMarkdownRepresentation(ctx, doc, content); err != nil {
				return false, false, err
			}
			s.addRepresentations(1)
		} else if !mediaProduced {
			// #394/#395: the active extractor cannot read this format. Rather than
			// hand it to an engine that fails silently (docling → empty) or hard-errors
			// (Mistral OCR → "unsupported file extension"), degrade honestly per the
			// §7.4.B.2 strict/lenient contract — never a silent empty representation.
			// Skipped only when direct multimodal embedding is not also making the doc
			// searchable; content support for these formats is #393.
			nonFatalErrored = s.degradeUnsupportedExtraction(ctx, doc, secretPatterns)
		}
	}
	if mediaProduced {
		if err := s.repGen.GenerateMediaChunks(ctx, doc, computeRepHash(content), spans); err != nil {
			return false, false, err
		}
		s.addRepresentations(1)
	}
	return mediaProduced, nonFatalErrored, nil
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

// maxMediaChunksPerDoc bounds the number of direct-embedding media chunks (PDF
// pages or A/V time windows) generated for a single document. Without a ceiling
// a 10k-page PDF or a multi-hour recording fans out into thousands of multimodal
// embed inputs and ffmpeg segment extractions — an accidental or hostile
// amplification/DoS vector (#408). Chunks past the cap are dropped with a durable
// "truncated at N of M" warning rather than silently processed or dropped.
const maxMediaChunksPerDoc = 512

// maxMediaChunksEff resolves the effective per-document media-chunk cap: the
// Service override when positive, else the package default (#408).
func (s *Service) maxMediaChunksEff() int {
	if s.MaxMediaChunksPerDoc > 0 {
		return s.MaxMediaChunksPerDoc
	}
	return maxMediaChunksPerDoc
}

// capMediaSpans truncates spans to the per-document media-chunk cap, emitting a
// diagnostic when it does so (#408). Media chunks past the cap are not embedded
// directly; the document's text path (OCR/transcript) is unaffected.
func (s *Service) capMediaSpans(doc model.Document, spans []model.Span) []model.Span {
	limit := s.maxMediaChunksEff()
	if len(spans) <= limit {
		return spans
	}
	s.getLogger().Printf(
		"multimodal: %s produced %d media chunk(s), exceeding the per-document cap (%d); truncated at %d — %d chunk(s) will not be embedded directly (#408)",
		doc.RelPath, len(spans), limit, limit, len(spans)-limit,
	)
	return spans[:limit]
}

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
		return s.capMediaSpans(doc, s.pdfPageSpans(doc, content))
	case "audio":
		if !isEmbeddableAudio(doc.RelPath) {
			return nil
		}
		windowMS := s.resolveMediaWindowMS(s.cfg.MediaAudioWindowSec, audioWindowMS, audioWindowCapMS, "audio")
		return s.capMediaSpans(doc, s.mediaTimeSpans(ctx, doc, windowMS))
	case "video":
		windowMS := s.resolveMediaWindowMS(s.cfg.MediaVideoWindowSec, videoWindowMS, videoWindowCapMS, "video")
		return s.capMediaSpans(doc, s.mediaTimeSpans(ctx, doc, windowMS))
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

// warnMultiTrackAudio surfaces multi-track media so additional audio streams are
// not silently dropped (issue #567). The transcription path feeds only the first
// audio stream (ffmpeg's default selection) to STT; when the container carries
// more than one audio stream — common in broadcast/proxy media that bundles an
// original mix, per-language dubs, and a music-&-effects track — the other tracks
// are neither transcribed nor indexed. This emits a single structured, greppable
// warning naming every track (audio-relative index, codec, channels, declared
// language/title — all non-sensitive metadata, never the media bytes) so the
// selection is honest rather than silent.
//
// It is best-effort: media is resolved through the CorpusFS (Localize) and probed
// via ProbeMediaInfoFunc (default avutil.ProbeMediaInfo). Any failure — ffprobe
// absent, an undecodable input, a localize error — silently yields no diagnostic
// and never affects the transcript that was already produced. The full per-track
// transcription this diagnostic points at (transcribing each stream, or selecting
// by language) is deferred to a data-model/spec change (issue #567).
func (s *Service) warnMultiTrackAudio(ctx context.Context, doc model.Document) {
	probe := s.ProbeMediaInfoFunc
	if probe == nil {
		probe = avutil.ProbeMediaInfo
	}
	localPath, cleanup, err := s.corpusFS().Localize(ctx, doc.RelPath)
	if err != nil {
		return
	}
	defer cleanup()
	info, err := probe(ctx, localPath)
	if err != nil {
		return
	}
	if !info.HasMultipleAudioStreams() {
		return
	}
	streams := info.AudioStreams
	descs := make([]string, len(streams))
	for i, a := range streams {
		descs[i] = describeAudioStream(i, a)
	}
	s.getLogger().Printf("multi-track audio: %s carries %d audio streams; only the first (%s) was transcribed — %d additional track(s) are not indexed: %s (issue #567)",
		doc.RelPath, len(streams), descs[0], len(streams)-1, strings.Join(descs[1:], ", "))
}

// describeAudioStream renders one audio stream as a compact, non-sensitive
// descriptor for the multi-track diagnostic (issue #567). pos is the
// audio-relative index (0 == first audio stream, what `-map 0:a:0` selects).
// Fields absent in the probe are omitted so the descriptor stays terse.
func describeAudioStream(pos int, a avutil.AudioStream) string {
	parts := []string{fmt.Sprintf("a:%d", pos)}
	if a.CodecName != "" {
		parts = append(parts, a.CodecName)
	}
	if a.Channels > 0 {
		parts = append(parts, fmt.Sprintf("%dch", a.Channels))
	}
	if a.Language != "" {
		parts = append(parts, "lang="+a.Language)
	}
	if a.Title != "" {
		parts = append(parts, "title="+a.Title)
	}
	return "[" + strings.Join(parts, " ") + "]"
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

// generateRepresentations produces a document's representations. It returns
// (nonFatalErrored, err). A returned err is a hard failure the caller propagates
// (aborting this document, counted as an error by runScan). nonFatalErrored is
// true when a soft-failure path (binary-content, video-no-representation, or a
// zero-representation provider failure) already persisted the document as
// status="error" and incremented the error counter itself while returning no
// hard error: the caller MUST NOT then also credit the document as indexed, or it
// would be double-counted as both indexed and error (issue #426).
func (s *Service) generateRepresentations(ctx context.Context, doc model.Document, content []byte, secretPatterns []*regexp.Regexp, forceReindex bool) (bool, error) {
	// Reset the per-document quality-gate quarantine flag (§8.6.6 / #426). The scan
	// loop is sequential, so this scopes the flag to the document about to be
	// processed even though it is Service-level state.
	s.quarantinedThisDoc = false
	if s.repGen == nil {
		return false, nil
	}

	// #556 / §7.4.A markup boundary: html is a dual-path format. When a structured
	// extraction engine (the docling family) is active and the per-format selection
	// routes html to it, produce a structured extracted_markdown representation
	// (headings/tables/links as region spans) instead of flat raw_text. When no
	// structured HTML engine is active — extractor off/unavailable, or pinned to a
	// non-structured engine — html falls back to the raw_text baseline below, so it
	// is never dropped and does not regress when docling is absent.
	if doc.DocType == "html" && s.htmlRoutesToStructured(doc.RelPath) {
		return s.generateHTMLStructured(ctx, doc, content, secretPatterns)
	}

	if ShouldGenerateRawText(doc.DocType) {
		// #398: a binary payload that classified into a text-oriented doc type
		// (e.g. .parquet → "data") must not be run through the raw-text path, where
		// normalizeUTF8 would turn it into U+FFFD soup and chunk/embed the garbage.
		// Skip it and record a durable, non-fatal diagnostic; the run continues.
		if looksLikeBinaryContent(content) {
			s.getLogger().Printf("skipping binary content on raw-text path for %s (%s): %v", doc.RelPath, doc.DocType, errBinaryOnRawTextPath)
			s.addErrors(1)
			s.persistNonFatalDocError(ctx, doc, errBinaryOnRawTextPath, secretPatterns)
			return true, nil
		}
		// we already loaded the file contents earlier in processDocument,
		// avoid re-reading it by using the new helper method.
		if err := s.repGen.GenerateRawTextFromContent(ctx, doc, content); err != nil {
			return false, err
		}
		s.addRepresentations(1)

		const titleScanLimit = 4096
		titleContent := content
		if len(titleContent) > titleScanLimit {
			titleContent = titleContent[:titleScanLimit]
		}
		s.persistTitleIfFound(ctx, doc, string(titleContent))
		return false, nil
	}

	mediaProduced, nonFatalErrored, err := s.generateExtractedAndMediaRepresentations(ctx, doc, content, secretPatterns)
	if err != nil {
		return false, err
	}
	// #681: the extracted text was withheld, so the document is already terminal.
	// Stop before the transcript and recognition steps, and report no soft error:
	// the caller settles it as secret_excluded, and a document counted as a skip
	// must not also be counted as an error.
	if s.secretExcludedThisDoc {
		return false, nil
	}
	if nonFatalErrored {
		// #394/#584: the extractor degraded an unsupported format and already
		// recorded it — as status="error" (strict) or a durable status="skipped"
		// (lenient) — counting it as an error or a skip itself. A pdf/image/document
		// asset has no transcript path, so propagate the suppress-credit signal now:
		// the caller must not also credit it as indexed (issue #426).
		return true, nil
	}
	// In `replace`, direct media embedding stands in for STT→text, so skip the
	// transcript when audio media chunks were produced (SPEC 8.1.7). `augment`
	// and `off` keep the transcript path unchanged.
	skipTranscript := s.embedMultimodal == "replace" && mediaProduced

	var noVideoRep bool
	if !skipTranscript {
		nonFatalErrored, noVideoRep, err = s.generateTranscriptOrSidecar(ctx, doc, content, secretPatterns, forceReindex, mediaProduced)
		if err != nil {
			return nonFatalErrored, err
		}
	}
	// #681: the transcript or the sidecar was withheld. Without this the media would
	// fall into the "#398/#495 no representation produced" verdict below and be
	// stamped status="error", which would overwrite the secret_excluded row and
	// count the same document as both a skip and an error. A withheld document is
	// unsearchable by DESIGN, not by failure, so it is neither.
	if s.secretExcludedThisDoc {
		return false, nil
	}

	return s.recognizeAndFinalizeMedia(ctx, doc, secretPatterns, noVideoRep, nonFatalErrored)
}

// recognizeAndFinalizeMedia runs recognition over a media document and finalizes
// the video no-representation verdict.
//
// Recognition (design 0004 §4) runs after transcript handling, but it is an
// INDEPENDENT representation source (§5.2 `recognize`), not a step subordinate to
// the transcript: it must run even when the transcript path produced nothing (STT
// off, no sidecar, no audio track) and even when multimodal `replace` skipped the
// transcript entirely — otherwise a video whose only available source is
// recognition is never recognized at all (#622). A recognition-backend failure is
// a hard per-document error exactly like an STT failure.
//
// `noVideoRep` is the provisional verdict from generateTranscriptOrSidecar; it
// only becomes a durable status="error" diagnostic when recognition came up empty
// too. Split out of generateRepresentations to keep that function within the
// cyclomatic-complexity budget.
//
// `transcriptSoftFailed` carries that function's own non-fatal outcome through
// unchanged. The transcript path can soft-fail for reasons that leave the media
// with no transcript but say nothing about recognition — an uncovered-language
// skip, or an STT provider failure with no media chunks — and those are precisely
// the cases where recognition is the only remaining source, so recognition still
// runs. Its already-persisted status="error" and error count are preserved either
// way, so the transcript is retried on the next incremental run even when
// recognition indexed annotations in the meantime.
func (s *Service) recognizeAndFinalizeMedia(ctx context.Context, doc model.Document, secretPatterns []*regexp.Regexp, noVideoRep, transcriptSoftFailed bool) (bool, error) {
	recognized, err := s.generateRecognitionRepresentation(ctx, doc)
	if err != nil {
		return transcriptSoftFailed, err
	}
	// #681: recognition itself was withheld, so the document is terminal. Same
	// reasoning as the transcript guard: a withheld document must not also be
	// branded an unsearchable-video error.
	if s.secretExcludedThisDoc {
		return false, nil
	}
	if noVideoRep && !recognized {
		// Only now is the verdict final: no sidecar, no derived transcript, no
		// multimodal keyframe chunks AND no recognition — the video is known but
		// unsearchable, so record it as a durable status="error" diagnostic
		// (#398/#495) while letting the run continue.
		s.getLogger().Printf("no representation produced for video %s: %v", doc.RelPath, errNoVideoRepresentation)
		s.addErrors(1)
		s.persistNonFatalDocError(ctx, doc, errNoVideoRepresentation, secretPatterns)
		// The document is now durably status="error" and counted; signal the caller
		// not to also credit it as indexed (issue #426).
		return true, nil
	}
	return transcriptSoftFailed, nil
}

// htmlRoutesToStructured reports whether an html document should be promoted to
// the structured extraction path (#556 / §7.4.A) instead of flat raw_text: true
// only when the active extractor is a structured (docling family) engine AND the
// per-format selection (§7.4.B.1) routes html to it. Every other case — no
// extractor, docling unavailable, or a pinned flat engine — keeps the guaranteed
// raw_text baseline, so html never regresses when docling is absent.
func (s *Service) htmlRoutesToStructured(relPath string) bool {
	if s.extractor == nil || s.repGen == nil {
		return false
	}
	if _, structured := s.extractor.(structuredExtractor); !structured {
		return false
	}
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(relPath)))
	return s.routeExtractionExt(ext) == routeStructured
}

// generateHTMLStructured produces the structured extracted_markdown representation
// for an html document via the active docling-family extractor (#556 / §7.4.A),
// preserving heading/table structure as region spans (html carries no page/bbox,
// so its region spans carry the section breadcrumb + label). It guarantees the
// §7.4.A baseline: if structured extraction errors or yields no parseable
// structure, it falls back to flat raw_text so html is never dropped. html has no
// media/transcript path, so it returns immediately after the text representation;
// the (nonFatalErrored, err) contract matches generateRepresentations.
func (s *Service) generateHTMLStructured(ctx context.Context, doc model.Document, content []byte, secretPatterns []*regexp.Regexp) (bool, error) {
	if se, ok := s.extractor.(structuredExtractor); ok {
		res, err := s.readOrComputeStructured(ctx, doc, content, se)
		switch {
		case err == nil && len(res.Blocks) > 0:
			if strings.TrimSpace(res.Markdown) == "" {
				// Blocks but no markdown: nothing to persist. Fall through to the
				// raw_text baseline rather than counting a phantom representation
				// (would inflate coverage; §7.4.A guarantees html is never dropped).
				s.getLogger().Printf("html structured extraction produced blocks but no markdown for %s, using raw_text baseline (§7.4.A)", doc.RelPath)
				break
			}
			if perr := s.persistStructuredRepresentation(ctx, doc, res); perr != nil {
				return false, perr
			}
			s.addRepresentations(1)
			return false, nil
		case err != nil:
			s.getLogger().Printf("html structured extraction failed for %s, falling back to raw_text baseline (§7.4.A): %v", doc.RelPath, err)
		default:
			s.getLogger().Printf("html structured extraction produced no structure for %s, using raw_text baseline (§7.4.A)", doc.RelPath)
		}
	}
	// Baseline (§7.4.A): flat raw_text so html is never dropped.
	if err := s.repGen.GenerateRawTextFromContent(ctx, doc, content); err != nil {
		return false, err
	}
	s.addRepresentations(1)
	const titleScanLimit = 4096
	titleContent := content
	if len(titleContent) > titleScanLimit {
		titleContent = titleContent[:titleScanLimit]
	}
	s.persistTitleIfFound(ctx, doc, string(titleContent))
	return false, nil
}

// generateTranscriptOrSidecar resolves a media document's transcript. Subtitle
// sidecar precedence (§8.6.4): a subtitle file (.vtt/.srt/.ttml) next to the
// media is ingested AS the transcript instead of running STT — an authored
// transcript is authoritative. `--force`/reindex overrides the gate, retiring
// any stale sidecar transcripts and re-running STT. Sidecar ingestion bypasses
// the quality gate (authored, not model-derived; §8.6.6/§8.6.7).
//
// It returns (nonFatalErrored, noVideoRepresentation, err). nonFatalErrored is
// true when a soft-failure path persisted the document as status="error" and
// counted it as an error itself, so the caller must not also credit it as
// indexed (issue #426). noVideoRepresentation is true when this video yielded
// no transcript, no sidecar and no media chunks — a PROVISIONAL verdict the
// caller finalizes only after recognition has had its chance, since recognition
// is an independent representation source (#622).
func (s *Service) generateTranscriptOrSidecar(ctx context.Context, doc model.Document, content []byte, secretPatterns []*regexp.Regexp, forceReindex, mediaProduced bool) (bool, bool, error) {
	if !forceReindex {
		ingested, err := s.ingestSidecarTranscripts(ctx, doc)
		if err != nil {
			return false, false, err
		}
		if ingested {
			return false, false, nil
		}
		// #681: the sidecar's cues matched a configured secret pattern, so nothing
		// was ingested AND the document is now withheld. Stop here rather than fall
		// through to STT, which would transcribe the same media and could brand the
		// withheld document with a provider error.
		if s.secretExcludedThisDoc {
			return false, false, nil
		}
	} else if err := s.retireStaleSidecarTranscripts(ctx, doc); err != nil {
		return false, false, err
	}

	// Route audio AND video (issue #495) through the same STT transcript path when
	// speech-to-text is configured. A video's audio track is extracted before it is
	// handed to the provider (see transcribe); audio is fed directly. When STT is
	// off (no transcriber) or the media yields no transcript, the no-representation
	// check below still surfaces an unsearchable video (#398).
	if isSidecarMediaType(doc.DocType) && s.transcriber != nil {
		// Honest-coverage floor, skip action (SPEC §8.2.1, #566): when the selected
		// STT model does not cover the pinned source language and
		// media.stt.on_uncovered_language=skip, suppress transcription rather than
		// emit degraded output. Handled in a helper so this function stays under the
		// cyclomatic-complexity budget; see skipUncoveredLanguageTranscript.
		if skip, suppressCredit := s.skipUncoveredLanguageTranscript(ctx, doc, mediaProduced); skip {
			return suppressCredit, false, nil
		}
		produced, err := s.generateTranscriptRepresentation(ctx, doc, content)
		if err != nil {
			// Provider/transient failures should not fail the entire ingest run.
			// Persistence/cache failures should still propagate.
			if errors.Is(err, ErrTranscriptProviderFailure) {
				s.getLogger().Printf("transcription skipped for %s: %v", doc.RelPath, err)
				s.addErrors(1)
				// #413: a genuine provider failure that left the document with no
				// representation of its own (no media chunks under multimodal
				// `augment`) must not stay status="ok" — that hid the unsearchable
				// audio/video from CorpusStats.Errors / RecentFailures /
				// FailureSummary and reported errors=0 after a restart. Persist it as
				// status="error" so the failure is durably visible and is retried on
				// the next incremental run, while STILL returning nil so the batch
				// continues. A document that DID produce media chunks stays "ok": it
				// remains searchable, so it is not a zero-representation failure.
				// Legitimately empty media never reaches here — an empty transcript
				// returns (false, nil) without ErrTranscriptProviderFailure.
				if !mediaProduced {
					s.persistNonFatalDocError(ctx, doc, err, secretPatterns)
					// The document is now durably status="error" and already counted
					// above; signal the caller not to also credit it as indexed
					// (issue #426). A doc that DID produce media chunks stays "ok"
					// and searchable, so it is still credited (returns false).
					return true, false, nil
				}
				return false, false, nil
			}
			return false, false, err
		}
		if produced {
			s.addRepresentations(1)
			return false, false, nil
		}
		// produced == false: the transcript was empty (silence) or the video has no
		// audio track to transcribe. Fall through so an otherwise unsearchable video
		// is still surfaced by the no-representation check below (#398/#495).
	}

	// #398/#495: a video with no subtitle sidecar (handled above), no derived
	// transcript, and no multimodal keyframe chunks has produced nothing HERE.
	// The verdict is only provisional though: recognition (#622) is an
	// independent representation source that has not run yet, so report the
	// condition upward and let the caller record the durable status="error"
	// diagnostic if recognition comes up empty too.
	if doc.DocType == "video" && !mediaProduced {
		return false, true, nil
	}
	return false, false, nil
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

// persistNonFatalDocError records a per-document failure that must NOT abort the
// batch run (the caller still returns nil) by re-upserting the document as
// status="error" with a redacted message. Without it, a document that produced
// zero representations because of a genuine provider failure would stay
// status="ok" and be invisible to CorpusStats.Errors / RecentFailures /
// FailureSummary — silently unsearchable, with errors=0 reported after a restart
// (#413). The upsert error is logged and swallowed because the original failure
// is the more actionable signal, mirroring the generateRepresentations error
// path. The live error counter is incremented separately by the caller.
func (s *Service) persistNonFatalDocError(ctx context.Context, doc model.Document, cause error, secretPatterns []*regexp.Regexp) {
	doc.Status = "error"
	doc.ErrorMessage = RedactSecretsInMessage(cause.Error(), secretPatterns)
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		s.getLogger().Printf("persist error status for %s: %v", doc.RelPath, err)
	} else {
		// Keep the live dedup map equal to the row this write produced (#691).
		s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
	}
	s.notifyDocumentError(doc)
}

// persistNonFatalDocSkip records a lenient §7.4.B.2 unsupported-format degradation
// as a DURABLE skip so the coverage gap survives the run that produced it (#584).
// A document no active extractor can read AND that produced no other searchable
// representation is recorded as documents.status="skipped" with the given
// skip_reason, so CorpusStats.SkipSummary — which aggregates status="skipped" rows
// — names it after a restart, not only via the in-run counter, and a file_skip
// event fires. Without it the document stayed status="ok": an unsearchable
// document mislabeled as indexed, invisible to the coverage report once its run
// ended. The upsert error is logged and swallowed (mirroring persistNonFatalDocError);
// the live skipped counter is incremented separately by the caller. The skip event
// still fires even if the upsert failed — the stream event is the only signal a
// `--json` consumer will see for it.
func (s *Service) persistNonFatalDocSkip(ctx context.Context, doc model.Document, reason string) {
	doc.Status = "skipped"
	doc.SkipReason = reason
	if err := s.store.UpsertDocument(ctx, doc); err != nil {
		s.getLogger().Printf("persist skipped status for %s: %v", doc.RelPath, err)
	} else {
		// Keep the live dedup map equal to the row this write produced (#691).
		s.notifyDocumentContentHash(doc.RelPath, doc.ContentHash)
	}
	s.notifyDocumentSkip(doc)
}

// notifyDocumentError invokes the registered per-document error callback with
// the already-redacted message. It fires even when the upsert above failed: the
// document did fail, and the stream event is the only signal an operator tailing
// `--json` will ever see for it. A panicking callback is contained so a buggy
// consumer cannot abort the ingest run.
func (s *Service) notifyDocumentError(doc model.Document) {
	s.onDocumentErrorMu.RLock()
	fn := s.onDocumentError
	s.onDocumentErrorMu.RUnlock()
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.getLogger().Printf("onDocumentError panic for %s (%s)", doc.RelPath, safePanicValue(r))
		}
	}()
	fn(doc.RelPath, doc.DocType, doc.ErrorMessage)
}

// handleArchiveDocumentAndNotify extracts an archive container's members and,
// only if that fully succeeded, raises the container's deferred `file_skip`.
// On failure the container was re-persisted as an error and its skipped credit
// withdrawn (addSkipped(-1)), so it must raise a `file_error` instead — the two
// events partition the never-indexed set (SPEC §3.2).
func (s *Service) handleArchiveDocumentAndNotify(ctx context.Context, doc model.Document, f DiscoveredFile, secretPatterns []*regexp.Regexp, forceReindex bool, seen map[string]struct{}, needsProcessing bool, finalContentHash string) error {
	if err := s.handleArchiveDocument(ctx, doc, f, secretPatterns, forceReindex, seen, needsProcessing, finalContentHash); err != nil {
		return err
	}
	s.notifyDocumentSkip(doc)
	return nil
}

// notifyDocumentSkip invokes the registered per-document skip callback. It is
// called only where the document's never-indexed status is final for this run,
// never merely where the `skipped` counter is incremented: an archive container
// is credited as skipped up front but reverts to an error if member extraction
// fails (addSkipped(-1)), and the spec forbids one document raising both a
// `file_skip` and a `file_error`.
//
// A blank SkipReason falls back to the doc_type mapping so pre-#570 rows, which
// were persisted before the skip_reason column existed, still report a reason
// rather than an empty string.
func (s *Service) notifyDocumentSkip(doc model.Document) {
	s.onDocumentSkipMu.RLock()
	fn := s.onDocumentSkip
	s.onDocumentSkipMu.RUnlock()
	if fn == nil {
		return
	}
	reason := doc.SkipReason
	if strings.TrimSpace(reason) == "" {
		if doc.Status == "secret_excluded" {
			reason = model.SkipReasonSecretExcluded
		} else {
			reason = skipReasonForDocType(doc.DocType)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			s.getLogger().Printf("onDocumentSkip panic for %s (%s)", doc.RelPath, safePanicValue(r))
		}
	}()
	fn(doc.RelPath, doc.DocType, reason)
}

// notifyDocumentContentHash reports the content_hash a document row now holds
// to the registered consumer (#691). Callers MUST invoke it only after the
// store write returned nil, so a consumer never sees a hash the corpus does not
// carry yet. A panicking consumer is contained: ingest must not die because a
// notification handler misbehaved, which mirrors the other document hooks.
func (s *Service) notifyDocumentContentHash(relPath, contentHash string) {
	s.onDocumentContentHashMu.RLock()
	fn := s.onDocumentContentHash
	s.onDocumentContentHashMu.RUnlock()
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.getLogger().Printf("onDocumentContentHash panic for %s (%s)", relPath, safePanicValue(r))
		}
	}()
	fn(relPath, contentHash)
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
	if s.repGen == nil {
		return nil
	}

	// #393: pandoc (T2) is a capability-activated engine for born-digital
	// office/markup/ebook formats. Route the doc's format first; a pandoc route is
	// served by the flat extracted_markdown path (no page/bbox provenance). This is
	// checked before the s.extractor nil-guard so pandoc works when it is the only
	// active engine (auto with no docling/OCR, or the pandoc pin).
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(doc.RelPath)))
	if s.pandocExtractor != nil && s.routeExtractionExt(ext) == routePandoc {
		return s.generatePandocMarkdownRepresentation(ctx, doc, content)
	}

	if s.extractor == nil {
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

	// #681: screen the extracted text BEFORE the title heuristic and before any
	// persistence. A credential that is only pixels in the source PDF or image is
	// plain text here, and this is the first point at which it could be indexed.
	if s.screenDerivedSecrets(ctx, doc, derivedKindExtraction, ocrText) {
		return nil
	}

	s.persistTitleIfFound(ctx, doc, ocrText)

	decision := s.screenOutputQuality(ctx, doc, qualityKindOCR, ocrText, quality.Context{
		Modality: quality.ModalityOCR,
	})

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeExtractedMarkdown,
		RepHash:     computeRepHash([]byte(ocrText)),
		MetaJSON:    s.extractionMetaJSON(ocrText),
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

	// #681: the rendered Markdown is the text this document will be searched by, so
	// it is screened before the title and before any persistence.
	if s.screenDerivedSecrets(ctx, doc, derivedKindExtraction, md) {
		return nil
	}

	if title := strings.TrimSpace(res.Title); title != "" {
		s.persistTitle(ctx, doc, title)
	} else {
		s.persistTitleIfFound(ctx, doc, md)
	}

	decision := s.screenOutputQuality(ctx, doc, qualityKindOCR, md, quality.Context{
		Modality: quality.ModalityOCR,
	})

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeExtractedMarkdown,
		RepHash:     computeRepHash([]byte(md)),
		MetaJSON:    s.extractionMetaJSON(md),
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
	if err := statefs.MkdirAll(cacheDir); err != nil {
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
		if wErr := statefs.WriteFile(cachePath, encoded); wErr != nil {
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

func (s *Service) extractionMetaJSON(text string) string {
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
	case *pandocExtractor:
		// provider "pandoc" already set from extractorProviderModel; no user command
		// is embedded (kept secret-free, unlike docling's command field).
	case *doclingServeExtractor:
		// Sanitized (scheme/host/path only): this is persisted per-document, so
		// any userinfo/query in the URL must not become durable metadata.
		meta["serve_url"] = SanitizeServeURL(ex.baseURL)
	case *mistral.Client:
		// provider/model already set from extractorProviderModel.
	default:
		meta["type"] = fmt.Sprintf("%T", s.extractor)
	}
	// §8.8: record a best-effort detected language for the extracted text. No
	// extractor reports a language today, so the meta["language"] guard is
	// forward-compatible: if a future extractor records a provider-declared
	// language ("declared", which outranks "detected"), the guard preserves it.
	// Stored as strings only (no confidence) so the map[string]string round-trips
	// through ocrIdentityFromMeta, which ignores language entirely — detection
	// never affects the OCR derivation identity.
	if _, has := meta["language"]; !has {
		if tag, _, ok := s.detectLanguage(text); ok {
			meta["language"] = tag
			meta["language_source"] = langSourceDetected
		}
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
	case *pandocExtractor:
		return "pandoc", ""
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

// generatePandocMarkdownRepresentation produces the extracted_markdown
// representation for a born-digital document via the pandoc engine (T2, #393). It
// mirrors the flat branch of generateOCRMarkdownRepresentation but records
// provider "pandoc" and, because pandoc emits no pages, chunks the Markdown by
// lines WITHOUT fabricating page/bbox provenance.
//
// #393 follow-up: section-breadcrumb spans (a SHOULD progressive enhancement per
// §7.4.B.1) are deferred — deriving them would require new Markdown-heading parsing
// machinery, so this ships the flat extracted_markdown without breadcrumb spans.
func (s *Service) generatePandocMarkdownRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	if s.repGen == nil || s.pandocExtractor == nil {
		return nil
	}
	md, err := s.readOrComputePandoc(ctx, doc, content)
	if err != nil {
		return err
	}
	md = strings.TrimSpace(md)
	if md == "" {
		return nil
	}

	// #681: pandoc converts a container format (docx, epub, odt) into the text this
	// document will be searched by, so it is screened before the title and before
	// any persistence.
	if s.screenDerivedSecrets(ctx, doc, derivedKindExtraction, md) {
		return nil
	}

	s.persistTitleIfFound(ctx, doc, md)

	decision := s.screenOutputQuality(ctx, doc, qualityKindOCR, md, quality.Context{
		Modality: quality.ModalityOCR,
	})

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeExtractedMarkdown,
		RepHash:     computeRepHash([]byte(md)),
		MetaJSON:    s.pandocExtractionMetaJSON(md),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return fmt.Errorf("upsert pandoc representation: %w", err)
	}

	segments := chunkRawTextByDocType(doc.DocType, md)
	if len(segments) == 0 {
		return nil
	}
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, decision); err != nil {
		return fmt.Errorf("persist pandoc chunks: %w", err)
	}
	return nil
}

// readOrComputePandoc returns the pandoc-converted Markdown for content, caching
// it under <state>/cache/pandoc keyed by content hash so re-indexing does not
// re-run pandoc. pandoc is a single deterministic engine with no model concept, so
// the bytes-only key is sufficient (its own cache dir isolates it from the OCR
// cache). A pandoc failure is wrapped as ErrOCRProviderFailure so it classifies as
// a retryable per-document OCR_FAILED in the run manifest (§14.4), mirroring
// readOrComputeOCR.
func (s *Service) readOrComputePandoc(ctx context.Context, doc model.Document, content []byte) (string, error) {
	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "pandoc")
	if err := statefs.MkdirAll(cacheDir); err != nil {
		return "", fmt.Errorf("create pandoc cache dir: %w", err)
	}
	cachePath := filepath.Join(cacheDir, computeContentHash(content)+".md")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), nil
	}
	md, err := s.pandocExtractor.Extract(ctx, doc.RelPath, content)
	if err != nil {
		return "", fmt.Errorf("%w: pandoc extract %s: %w", ErrOCRProviderFailure, doc.RelPath, err)
	}
	mdBytes := []byte(strings.ReplaceAll(strings.ReplaceAll(md, "\r\n", "\n"), "\r", "\n"))
	if err := statefs.WriteFile(cachePath, mdBytes); err != nil {
		return "", fmt.Errorf("write pandoc cache: %w", err)
	}
	return string(mdBytes), nil
}

// pandocCachePath returns the cache file readOrComputePandoc writes for content.
// The pandoc cache is keyed by content hash alone (pandoc is a single
// deterministic engine with no model concept), so no active-extractor key is
// mixed in as PurgeOCRCache does.
func (s *Service) pandocCachePath(content []byte) string {
	return filepath.Join(s.cfg.StateDir, "cache", "pandoc", computeContentHash(content)+".md")
}

// PurgePandocCache removes the pandoc cache entry for content, mirroring
// PurgeOCRCache for the pandoc engine (#393). Callers that gate the returned
// Markdown (e.g. the MCP secret-pattern gate, #407) use it so a refused result is
// never left persisted in {StateDir}/cache/pandoc. Missing files are ignored.
func (s *Service) PurgePandocCache(content []byte) {
	if err := os.Remove(s.pandocCachePath(content)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.getLogger().Printf("purge pandoc cache: %v", err)
	}
}

// pandocExtractionMetaJSON builds the per-document extracted_markdown meta_json for
// a pandoc extraction: provider "pandoc" (no model concept) plus a best-effort
// detected language (§8.8). It is separate from extractionMetaJSON because that
// switches on the PRIMARY s.extractor, which under `auto` is docling — so a pandoc
// extraction must not be mislabeled with the primary's provider. No user-supplied
// command is embedded, keeping diagnostics secret-free.
func (s *Service) pandocExtractionMetaJSON(text string) string {
	meta := map[string]string{"provider": "pandoc"}
	if tag, _, ok := s.detectLanguage(text); ok {
		meta["language"] = tag
		meta["language_source"] = langSourceDetected
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(encoded)
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

// Quality-gate screening kinds — the `kind` argument to screenOutputQuality.
// They select the canonical §14.4 error code recorded when a degenerate output
// is quarantined (§8.6.6). The literal values are also the diagnostic labels in
// the quarantine log line.
const (
	qualityKindOCR         = "ocr"
	qualityKindTranscript  = "transcript"
	qualityKindTranslation = "transcript-translation"
)

// qualityGateFailureCode maps a screening kind to its canonical §14.4 error code
// (§8.6.6): an OCR / transcript / translation output rejected by the gate is
// OCR_FAILED / TRANSCRIBE_FAILED / TRANSLATE_FAILED respectively. An unknown kind
// falls back to the generic EXTRACT_FAILED so the recorded code is always a valid
// §14.4 constant.
func qualityGateFailureCode(kind string) string {
	switch kind {
	case qualityKindOCR:
		return manifestErrOCRFailed
	case qualityKindTranscript:
		return manifestErrTranscribeFailed
	case qualityKindTranslation:
		return manifestErrTranslateFailed
	default:
		return manifestErrExtractFailed
	}
}

// screenOutputQuality runs the quality gate over generated text. It returns a
// zero-value (non-quarantine) decision when the gate is disabled (nil) or the
// verdict is clean, so callers can always insert via the returned decision. On
// a failed gate it logs a content-free warning, records the §8.6.6 per-document
// error (documents.status=error + canonical §14.4 code, non-fatal), and returns
// the chunk-level quarantine values. kind selects the canonical code and the
// diagnostic label; doc identifies the document to mark.
func (s *Service) screenOutputQuality(ctx context.Context, doc model.Document, kind, text string, qctx quality.Context) quarantineDecision {
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
	s.getLogger().Printf("quality gate quarantined %s output for %s: reason=%s", kind, doc.RelPath, reason)
	// §8.6.6: a failed output quality gate is a non-fatal per-document error — the
	// document is marked status=error with the canonical §14.4 code while indexing
	// continues. This is IN ADDITION to the chunk-level quarantine encoded in the
	// returned decision (embedding_status=error), which keeps the degenerate text
	// out of the embedding index.
	//
	// §8.6.12: while transcribing a multi-track selection the doc-error marking is
	// DEFERRED (deferGateDocError) so a gate rejection fails only that track; the
	// orchestrator marks the document error only if every selected track failed. The
	// chunk-level quarantine below is unaffected either way.
	if !s.deferGateDocError {
		s.recordQualityGateDocError(ctx, doc, kind, reason)
	}
	return quarantineDecision{
		quarantine: true,
		embErr:     embErr,
		category:   string(store.ErrorCategoryQualityGate),
	}
}

// recordQualityGateDocError persists the §8.6.6 per-document error for a
// quality-gate quarantine and accounts for it exactly once (issue #426):
//
//   - it marks documents.status=error via persistNonFatalDocError with a
//     content-free message that LEADS with the canonical §14.4 code, so the code
//     is queryable through error_message / RecentFailures (§15.6) without a store
//     schema change (the documents table has no error_code column, and §8.6.6 only
//     requires the document be "marked status=error with the appropriate code");
//   - it records the same canonical code on the active batch manifest outcome
//     (§8.6.11), whose record carries a dedicated machine-readable error_code;
//   - it increments the run error counter once and sets quarantinedThisDoc so the
//     scan loop suppresses the indexed credit (the document counts as exactly one
//     error, zero indexed — issue #426).
//
// It is idempotent per document: a document with several rejected representations
// (e.g. a transcript plus per-language translations) counts once and keeps the
// FIRST canonical code, while every rejected representation still quarantines its
// own chunks via the returned decision. The document's content_hash done-marker is
// blanked before the error upsert (#402/#413): the row is now status=error AND
// carries an empty hash, so the next incremental run re-derives it. Blanking is
// required for the two-phase derivation path, where deriveDocument re-reads the doc
// AFTER the transcription pass already stamped content_hash — without it a
// translation-gate rejection would persist status=error yet keep the stamped hash,
// so the unchanged-content gate would SKIP (never retry) the quarantined document.
// On the single-phase path the hash is already withheld (withholdContentHash), so
// blanking it again is a no-op; the two modes are now consistent. Retry re-screens
// the cached STT/OCR/translation output, not the provider — a bounded cost.
func (s *Service) recordQualityGateDocError(ctx context.Context, doc model.Document, kind string, reason quality.Reason) {
	code := qualityGateFailureCode(kind)
	// Content-free and deterministic (§8.6.6): the message is code + kind + the
	// enum reason only — no document text — so it carries no secrets. nil secret
	// patterns are therefore safe (persistNonFatalDocError still redacts, a no-op
	// here).
	msg := fmt.Sprintf("%s: output quality gate rejected %s output (%s)", code, kind, reason)
	// Manifest surface (§8.6.11): first canonical code wins, never clobbered.
	s.markActiveErrored(code, msg)
	if s.quarantinedThisDoc {
		// This document already recorded its canonical per-document error and was
		// counted; keep the first code and count it once (issue #426).
		return
	}
	s.quarantinedThisDoc = true
	s.addErrors(1)
	// Withhold the content_hash done-marker so the quarantined document is retried
	// next run in BOTH single-phase and two-phase modes (#402/#413). doc is a value
	// copy, so this affects only the persisted error row.
	doc.ContentHash = ""
	s.persistNonFatalDocError(ctx, doc, errors.New(msg), nil)
}

// transcriptExpectedLanguage returns the language tag the quality gate's
// language/script-mismatch detector should treat as expected for a transcript,
// or "" when the detector should self-skip.
//
// A corpus-pinned STT language (s.transcriptLanguage, resolved from the active
// provider profile per SPEC 8.1.3) is a provider HINT that biases
// transcription; it is NOT a per-file guarantee about content. Feeding it to
// the gate as ground truth quarantines legitimate other-language clips — e.g.
// an English interview in a corpus pinned to `ru` transcribes to ~100%
// off-script and is discarded (dir2mcp#439 F3), contradicting the
// general-purpose auto-detect mandate. So by default (MediaSTTLanguageStrict
// false) a pinned language does NOT drive quarantine and this returns "": the
// language detector self-skips while the repetition/gibberish/density detectors
// still catch degenerate output. Operators who have verified a genuinely
// single-language corpus can set media.stt.language_strict:true to restore
// strict enforcement. When STT language is auto (unpinned) s.transcriptLanguage
// is already "", so the flag is a no-op and behaviour is unchanged.
func (s *Service) transcriptExpectedLanguage() string {
	if !s.cfg.MediaSTTLanguageStrict {
		return ""
	}
	return s.transcriptLanguage
}

// trackContext describes ONE audio track selected for transcription (SPEC
// §8.6.12). audioIndex is the 0-based audio-relative stream index; stream carries
// the container's declared per-stream metadata (language/title, when known);
// warnExtras requests the legacy "only the first track was transcribed"
// diagnostic (emitted only on the default first-track path).
type trackContext struct {
	audioIndex int
	stream     avutil.AudioStream
	hasStream  bool
	warnExtras bool
}

// generateTranscriptRepresentation transcribes a media document (audio, or a
// video's extracted audio track — issue #495) and persists the source transcript
// representation(s) and their chunks. It returns (produced, err): produced is true
// when at least one transcript representation was actually persisted, so the caller
// can tell a real transcript from a legitimately empty one (silence, or a video with
// no audio track) and surface an otherwise-unsearchable video (#398). A returned err
// follows the same contract as generateRepresentations.
//
// Which audio tracks are transcribed is governed by media.stt.tracks (SPEC §8.6.12):
// the default `first` transcribes only track 0 (byte-for-byte today's behavior and
// cost), while `all` / an explicit index list additionally transcribe each selected
// track N ≥ 1 under a distinct `transcript@t<N>` rep_type.
func (s *Service) generateTranscriptRepresentation(ctx context.Context, doc model.Document, content []byte) (bool, error) {
	if s.repGen == nil || s.transcriber == nil {
		return false, nil
	}
	sel, err := config.ParseSTTTracks(s.cfg.MediaSTTTracks)
	if err != nil {
		// Should never happen (validated at startup), but fail closed rather than
		// silently transcribing the wrong track set.
		return false, err
	}
	// Default (and overwhelmingly common) single-track path: transcribe only the
	// first audio stream, keeping the eager quality-gate semantics and the honest
	// "additional tracks dropped" diagnostic exactly as before (§8.6.12: track 0 is
	// byte-for-byte unchanged).
	if sel.Mode == config.STTTracksFirst {
		produced, _, terr := s.transcribeAndPersistTrack(ctx, doc, content, trackContext{audioIndex: 0, warnExtras: true})
		return produced, terr
	}
	return s.generateSelectedTrackTranscripts(ctx, doc, content, sel)
}

// generateSelectedTrackTranscripts transcribes every audio track selected by an
// `all` / explicit-index media.stt.tracks selection (SPEC §8.6.12), in container
// stream order. Track-scoped failure semantics apply: a per-track transcription
// error or quality-gate rejection fails ONLY that track (its representation is
// dropped and recorded as honest coverage), and the DOCUMENT is reported as an
// error only if EVERY selected track failed — otherwise it is ready with whatever
// tracks succeeded.
func (s *Service) generateSelectedTrackTranscripts(ctx context.Context, doc model.Document, content []byte, sel config.STTTrackSelection) (bool, error) {
	info := s.probeTrackInfo(ctx, doc)
	indices := resolveTrackIndices(sel, info)
	if len(indices) == 0 {
		if len(info.AudioStreams) == 0 {
			// Unprobeable / no audio census (ffprobe absent, undecodable input): fall
			// back to the default first-track path so a media file still gets its
			// track-0 transcript rather than being silently skipped.
			produced, _, terr := s.transcribeAndPersistTrack(ctx, doc, content, trackContext{audioIndex: 0, warnExtras: false})
			return produced, terr
		}
		// The probe succeeded but an explicit index list matched no existing track in
		// THIS file (every listed index is past its track count — a per-file
		// condition, §8.6.12): transcribe nothing rather than falling back to an
		// unselected track. A video with no other representation is still caught by
		// the caller's no-representation check (#398).
		s.getLogger().Printf("media.stt.tracks: none of the selected tracks exist in %s (%d audio stream(s)); no transcript produced (§8.6.12)", doc.RelPath, len(info.AudioStreams))
		return false, nil
	}

	// Defer the eager per-document quality-gate error while looping so a rejected
	// track fails only that track (§8.6.12); the all-failed decision below marks the
	// document error at most once.
	s.deferGateDocError = true
	defer func() { s.deferGateDocError = false }()

	producedAny := false
	failed := 0
	var firstFailErr error
	for _, n := range indices {
		tc := trackContext{audioIndex: n}
		if n < len(info.AudioStreams) {
			tc.stream = info.AudioStreams[n]
			tc.hasStream = true
		}
		produced, rejected, terr := s.transcribeAndPersistTrack(ctx, doc, content, tc)
		if terr != nil {
			if isHardTranscriptError(terr) {
				// A persistence/cache failure is not track-scoped; abort the document.
				return producedAny, terr
			}
			// A provider/transient transcription failure is scoped to this track.
			failed++
			if firstFailErr == nil {
				firstFailErr = terr
			}
			s.logTrackDropped(doc, tc, terr.Error())
			continue
		}
		if rejected {
			failed++
			continue
		}
		if produced {
			producedAny = true
		}
	}

	// §8.6.6/§8.6.7 over the SELECTED track set: the document is an error only when
	// every selected track failed (the degenerate single-track case is one track, so
	// its failure is the document's). Surface it through the provider-failure channel
	// so the caller marks the document status=error and retries it next run, without
	// double-counting the video-no-representation path.
	if failed == len(indices) {
		if firstFailErr != nil {
			return false, firstFailErr
		}
		return false, fmt.Errorf("%w: every selected audio track of %s failed transcription (§8.6.12)", ErrTranscriptProviderFailure, doc.RelPath)
	}
	return producedAny, nil
}

// shiftSegmentsForLeadingSilence applies the optional leading-silence trim
// (dir2mcp#258) to a transcript's time spans and returns the offset it used, in
// milliseconds. It returns 0, leaving the spans untouched, when the trim is
// disabled, when ffmpeg is absent, or when no dead air was detected. The caller
// reuses the returned offset for the translated transcripts of the same track so
// source and translated time windows stay aligned. Split out of
// transcribeAndPersistTrack to keep it within the complexity budget.
func (s *Service) shiftSegmentsForLeadingSilence(ctx context.Context, doc model.Document, segments []chunkSegment) int {
	if !s.cfg.MediaTrimLeadingSilence {
		return 0
	}
	offset := s.detectLeadingSilence(ctx, doc)
	if offset <= 0 {
		return 0
	}
	trimOffsetMS := int(offset.Milliseconds())
	shiftTranscriptSpans(segments, trimOffsetMS)
	return trimOffsetMS
}

// transcribeAndPersistTrack transcribes ONE selected audio track and persists its
// source transcript representation (plus, in single-pass mode, its translations).
// It returns (produced, gateRejected, err): produced is true when a representation
// was persisted; gateRejected is true when the quality gate dropped this track's
// transcript under the deferred multi-track semantics (§8.6.12); err carries a
// provider/transient transcription failure or a hard persistence error. Track 0
// keeps the bare `transcript` rep_type and is byte-for-byte identical to the legacy
// single-track path; each additional track N ≥ 1 is persisted under
// `transcript@t<N>` with its container-declared track/language/label in meta_json.
func (s *Service) transcribeAndPersistTrack(ctx context.Context, doc model.Document, content []byte, tc trackContext) (bool, bool, error) {
	transcriptText, words, err := s.readTrackTranscript(ctx, doc, content, tc)
	if err != nil {
		return false, false, err
	}

	// Multi-track honesty (issue #567/#596): on the default first-track path the STT
	// above fed only the first audio stream to STT; surface any additional streams so
	// the selection is visible rather than silent. Fired BEFORE the empty-transcript
	// early return so a non-dialogue first track still warns about dropped tracks.
	if tc.warnExtras {
		s.warnMultiTrackAudio(ctx, doc)
	}

	transcriptText = strings.TrimSpace(transcriptText)
	if transcriptText == "" {
		return false, false, nil
	}

	// #681: a credential spoken on a call, or read out in a screen share, is audio
	// in the source file and plain text only here. Screen it before the quality
	// gate and before any persistence. The document is already withheld on return,
	// so this reports "no transcript produced" without a soft error: the run must
	// not also count the withheld document as an error.
	if s.screenDerivedSecrets(ctx, doc, derivedKindTranscript, transcriptText) {
		return false, false, nil
	}

	var duration time.Duration
	if d, derr := s.probeDuration(ctx, doc); derr == nil {
		duration = d
	}
	decision := s.screenOutputQuality(ctx, doc, qualityKindTranscript, transcriptText, quality.Context{
		Modality:         quality.ModalityTranscript,
		ExpectedLanguage: s.transcriptExpectedLanguage(),
		Duration:         duration,
	})
	// §8.6.12: under the deferred (multi-track) gate, a rejected track's transcript
	// is DROPPED entirely (recorded as honest coverage) rather than persisted with
	// quarantined chunks, so it never counts toward the document being "ready". On
	// the eager (default) path deferGateDocError is false and the legacy
	// persist-with-quarantine behavior below is preserved unchanged.
	if decision.quarantine && s.deferGateDocError {
		s.logTrackDropped(doc, tc, "output quality gate rejected the transcript")
		return false, true, nil
	}

	segments := chunkTranscriptByTimeWithWordsFiltered(transcriptText, words, s.captionWordFilter())
	// Apply the configured subtitle cue cleaning to the chunk text BEFORE
	// embedding (issues #545, #765), so whisper keyword-spam, hallucinated URL /
	// credit lines and repetition runs never pollute the index: the same cleaning
	// the export path applies to the sidecar. Off by default.
	segments = applyCueCleaningToSegments(segments, s.captionCleanOptions())
	if len(segments) == 0 {
		return false, false, nil
	}
	// Optional model-driven speaker diarization (SPEC §8.6.8): when active and a
	// diarizer is injected, attribute each segment to a speaker. This is metadata
	// only — it stamps span.Speaker/SpeakerLabel and never changes chunk text or
	// span bounds. With no diarizer (the default), this is a no-op and the
	// transcript is un-attributed exactly as today (sidecar <v> attribution is
	// unaffected). Run BEFORE the meta is built so diarized/speakers reflect the
	// attribution that is actually present on the segments.
	s.applyDiarization(ctx, doc, content, segments)

	meta := s.sttTranscriptMeta(distinctSpeakers(segments), transcriptText, segmentsHaveWordTiming(segments))
	applyTrackMeta(&meta, tc)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return false, false, fmt.Errorf("marshal transcript meta: %w", err)
	}
	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     TranscriptRepTypeForTrack(tc.audioIndex, ""),
		RepHash:     computeRepHash([]byte(transcriptText)),
		MetaJSON:    string(metaJSON),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	repID, err := s.repGen.store.UpsertRepresentation(ctx, rep)
	if err != nil {
		return false, false, fmt.Errorf("upsert transcript representation: %w", err)
	}

	// Optional leading-silence trim (dir2mcp#258): when enabled, subtract the
	// detected dead-air offset from every time span and word timestamp so the
	// transcript aligns to first speech. Disabled (default) leaves spans
	// untouched; ffmpeg absent / detection failure -> 0 offset -> no change. The
	// offset is captured so the SAME shift is applied to any translated
	// transcripts below, keeping source/translated time windows aligned.
	trimOffsetMS := s.shiftSegmentsForLeadingSilence(ctx, doc, segments)
	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, decision); err != nil {
		return false, false, fmt.Errorf("persist transcript chunks: %w", err)
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
	//
	// Under the two-phase split (SPEC §8.6.11) translation is the derivation pass's
	// job and runs in a later corpus-wide pass (see deriveDocument), so the
	// transcription pass stops here after persisting the source transcript. The
	// final set of representations is identical either way — only the ordering
	// differs. In single-pass mode (the default) it runs inline as before.
	if s.activePass == passTranscription {
		return true, false, nil
	}
	if err := s.translateTranscriptRepresentations(ctx, doc, content, transcriptText, duration, trimOffsetMS, tc.audioIndex); err != nil {
		s.getLogger().Printf("transcript translation skipped for %s: %v", doc.RelPath, err)
		s.addErrors(1)
		// §8.6.11/§14.4: record the canonical translation-failure code
		// (TRANSLATE_FAILED for a provider failure) on the run manifest so the
		// manifest faithfully reports the failed derivation. The source transcript
		// was already persisted and stays searchable, so documents.status is left
		// "ok" (best-effort translation, #426). This is a manifest-only signal and
		// a no-op when no batch run is active.
		code := manifestErrorCode(err)
		s.markActiveErrored(code, code+": transcript translation failed")
	}
	return true, false, nil
}

// readTrackTranscript resolves the transcript text (and optional per-word timing)
// for a single selected audio track (SPEC §8.6.12). Track 0 is transcribed via the
// exact legacy path (the document's bytes, with a video's default audio demuxed
// inline), so its cache key and result are byte-for-byte unchanged. An additional
// track N ≥ 1 is first demuxed to a compact per-track audio clip and transcribed as
// a standalone audio document, keying the transcribe cache on the extracted bytes so
// each track caches independently.
func (s *Service) readTrackTranscript(ctx context.Context, doc model.Document, content []byte, tc trackContext) (string, []model.TimedWord, error) {
	if tc.audioIndex <= 0 {
		return s.readOrComputeTranscriptWithWords(ctx, doc, content, "")
	}
	audio, err := s.extractTrackAudio(ctx, doc, content, tc.audioIndex)
	if err != nil {
		if errors.Is(err, avutil.ErrNoAudioStream) {
			// The requested track does not exist in this file: a track-scoped "no
			// transcript" (handled by the caller as an empty, non-fatal outcome), not
			// a provider failure.
			s.getLogger().Printf("media.stt.tracks: %s has no audio track %d to transcribe; skipping it (§8.6.12)", doc.RelPath, tc.audioIndex)
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("%w: extract audio track %d of %s: %w", ErrTranscriptProviderFailure, tc.audioIndex, doc.RelPath, err)
	}
	// Transcribe the extracted clip as a standalone audio document: DocType audio so
	// transcribe() does not re-demux, and an audio-suffixed rel_path so the provider
	// infers an audio MIME. The extracted bytes drive the transcribe cache key, so
	// each track's transcript caches independently of track 0 and its siblings.
	trackDoc := doc
	trackDoc.DocType = "audio"
	trackDoc.RelPath = trackAudioRelPath(doc.RelPath, tc.audioIndex)
	return s.readOrComputeTranscriptWithWords(ctx, trackDoc, audio, "")
}

// extractTrackAudio demuxes a specific audio-relative track to a compact STT-ready
// clip (SPEC §8.6.12), mirroring extractVideoAudioTrack but selecting the track by
// index via ExtractAudioTrackIndexFunc (default avutil.ExtractAudioTrackIndex). The
// in-memory bytes are staged to a temp file because avutil slices by path.
func (s *Service) extractTrackAudio(ctx context.Context, doc model.Document, content []byte, audioIndex int) ([]byte, error) {
	tmp, err := os.CreateTemp("", "dir2mcp-vaudio-*"+filepath.Ext(doc.RelPath))
	if err != nil {
		return nil, fmt.Errorf("stage media for audio track extraction: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write staged media: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("flush staged media: %w", err)
	}
	extract := s.ExtractAudioTrackIndexFunc
	if extract == nil {
		extract = avutil.ExtractAudioTrackIndex
	}
	return extract(ctx, tmpPath, audioIndex)
}

// probeTrackInfo probes doc's container/stream census for track selection (SPEC
// §8.6.12), resolving the media through the CorpusFS. It is best-effort: any error
// (ffprobe absent, undecodable input, localize failure) yields a zero MediaInfo so
// the caller degrades to the default first-track behavior.
func (s *Service) probeTrackInfo(ctx context.Context, doc model.Document) avutil.MediaInfo {
	probe := s.ProbeMediaInfoFunc
	if probe == nil {
		probe = avutil.ProbeMediaInfo
	}
	localPath, cleanup, err := s.corpusFS().Localize(ctx, doc.RelPath)
	if err != nil {
		return avutil.MediaInfo{}
	}
	defer cleanup()
	info, err := probe(ctx, localPath)
	if err != nil {
		return avutil.MediaInfo{}
	}
	return info
}

// resolveTrackIndices resolves a media.stt.tracks selection against a probed track
// census into the concrete, ordered (ascending, container stream order) set of
// audio-relative indices to transcribe (SPEC §8.6.12). `all` expands to every probed
// track; an explicit list keeps only in-range indices (an index past this file's
// track count is skipped here as a track-scoped no-op — a corpus mixes files with
// different track counts, so it is a per-file condition, not a global CONFIG_INVALID).
// With no probed audio streams the result is empty and the caller falls back to the
// default first-track path.
func resolveTrackIndices(sel config.STTTrackSelection, info avutil.MediaInfo) []int {
	n := len(info.AudioStreams)
	if n == 0 {
		return nil
	}
	switch sel.Mode {
	case config.STTTracksAll:
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	case config.STTTracksList:
		out := make([]int, 0, len(sel.Indices))
		for _, idx := range sel.Indices {
			if idx >= 0 && idx < n {
				out = append(out, idx)
			}
		}
		return out
	default: // STTTracksFirst
		return []int{0}
	}
}

// applyTrackMeta stamps the per-track fields onto a transcript's meta_json (SPEC
// §8.6.12). Track 0 is left untouched (absence of `track` ⇒ track 0), so a legacy
// single-track transcript's meta_json is byte-for-byte unchanged. An additional
// track records its 0-based index plus the container's declared language/label when
// present.
func applyTrackMeta(meta *transcriptMeta, tc trackContext) {
	if tc.audioIndex <= 0 {
		return
	}
	meta.Track = tc.audioIndex
	if tc.hasStream {
		// track_language is the container's OWN declared tag, recorded verbatim (only
		// trimmed): kept general-purpose with no hardcoded language remapping.
		meta.TrackLanguage = strings.TrimSpace(tc.stream.Language)
		meta.TrackLabel = strings.TrimSpace(tc.stream.Title)
	}
}

// logTrackDropped records a track-scoped failure as honest coverage (SPEC §8.6.12):
// a single greppable, content-free line naming the dropped track and the reason, so
// a per-track transcription/quality failure is visible rather than silent while the
// sibling tracks are retained.
func (s *Service) logTrackDropped(doc model.Document, tc trackContext, reason string) {
	desc := describeAudioStream(tc.audioIndex, tc.stream)
	s.getLogger().Printf("media.stt.tracks: dropped audio track %s of %s: %s (issue #567)", desc, doc.RelPath, reason)
}

// isHardTranscriptError reports whether a per-track transcription error is a HARD
// failure (persistence/cache/marshal) that must abort the whole document, as opposed
// to a provider/transient transcription failure (ErrTranscriptProviderFailure) that
// is scoped to the failing track under §8.6.12.
func isHardTranscriptError(err error) bool {
	return err != nil && !errors.Is(err, ErrTranscriptProviderFailure)
}

// trackAudioRelPath rewrites a media path to a per-track extracted-audio filename so
// the STT provider infers an audio MIME from the extension rather than a video
// container it would reject (mirrors videoAudioRelPath), while keeping the track
// index in the stem for human-identifiable staging (SPEC §8.6.12).
func trackAudioRelPath(relPath string, audioIndex int) string {
	base := strings.TrimSuffix(relPath, filepath.Ext(relPath))
	return fmt.Sprintf("%s.t%d.m4a", base, audioIndex)
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
// TranscriptRepTypeForTrack(track, lang) so it coexists with the source transcript,
// any sidecar per-language reps, and (for a multi-track selection) the other tracks'
// translations (§8.6.12 keys them transcript@t<N>-<lang>); routes through the output
// quality gate; and records source_language + translate provider/model in meta_json.
// track is the 0-based audio-relative index of the transcript being translated (0
// for the bare/default transcript).
func (s *Service) translateTranscriptRepresentations(ctx context.Context, doc model.Document, content []byte, sourceText string, duration time.Duration, trimOffsetMS, track int) error {
	if !s.translationConfigured() {
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
		if err := s.translateOneTranscript(ctx, doc, content, sourceText, sourceLang, lang, duration, trimOffsetMS, track); err != nil {
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
func (s *Service) translateOneTranscript(ctx context.Context, doc model.Document, content []byte, sourceText, sourceLang, targetLang string, duration time.Duration, trimOffsetMS, track int) error {
	// Source the English text either from the chat generator (line-by-line over
	// the source transcript) or, for engine=whisper, from Whisper's native
	// translate task (a second pass over the audio that re-segments with its own
	// timings). Both yield [mm:ss]-marked lines, so everything below — quality
	// gate, representation upsert, time-span chunking, export — is identical.
	var (
		translated      string
		translatedWords []model.TimedWord
		err             error
	)
	if s.translateEngine == "whisper" {
		translated, translatedWords, err = s.readOrComputeWhisperTranslation(ctx, doc, content)
	} else {
		translated, err = s.readOrComputeTranslation(ctx, content, sourceText, targetLang)
	}
	if err != nil {
		// §14.4: a translation provider/transport failure (chat OR whisper engine)
		// is classified TRANSLATE_FAILED — distinct from the transcript's
		// TRANSCRIBE_FAILED — when recorded on the run manifest (manifestErrorCode).
		return fmt.Errorf("%w: translate transcript for %s into %q: %w", ErrTranslateProviderFailure, doc.RelPath, targetLang, err)
	}
	translated = strings.TrimSpace(translated)
	if translated == "" {
		return nil
	}

	// #681: a translation is model output over the transcript, and a credential
	// survives translation verbatim. Screen it before the quality gate and before
	// any persistence.
	if s.screenDerivedSecrets(ctx, doc, derivedKindTranslation, translated) {
		return nil
	}

	// A translated transcript is model output, so it routes through the output
	// quality gate exactly like an STT transcript (anti-hallucination). The
	// expected language is the TARGET language: the gate's language detector
	// should accept the translated text, not the source.
	decision := s.screenOutputQuality(ctx, doc, qualityKindTranslation, translated, quality.Context{
		Modality:         quality.ModalityTranscript,
		ExpectedLanguage: targetLang,
		Duration:         duration,
	})

	// Chunk the translated text with the same word-aware transcript chunker as the
	// source path so its time spans line up with the source segments (the
	// translation preserves each segment's verbatim [mm:ss] marker) AND carry the
	// whisper-translate per-word timings, enabling broadcast segmentation of the
	// English track. translatedWords is nil for the chat engine, leaving that path
	// chunk-only as before.
	segments := chunkTranscriptByTimeWithWordsFiltered(translated, translatedWords, s.captionWordFilter())
	// Apply the configured subtitle cue cleaning to the translated chunk text
	// before embedding (issues #545, #765), consistent with the source transcript
	// path.
	segments = applyCueCleaningToSegments(segments, s.captionCleanOptions())
	// Bail BEFORE creating the representation when the translation was fully
	// stripped (drop/scrub emptied every segment): otherwise we would leave a
	// dangling rep row with no chunks and inflate the representation count. The
	// source transcript path bails before UpsertRepresentation for the same reason.
	if len(segments) == 0 {
		return nil
	}
	// Apply the same leading-silence trim offset as the source transcript so the
	// translated time windows stay aligned with the source (dir2mcp#258). A zero
	// offset (trim disabled / no silence detected) is a no-op.
	if trimOffsetMS > 0 {
		shiftTranscriptSpans(segments, trimOffsetMS)
	}

	meta := transcriptMeta{
		Source:            translationSource,
		Language:          targetLang,
		SourceLanguage:    sourceLang,
		TranslateProvider: s.translateProvider,
		TranslateModel:    s.translateModel,
		Timestamps:        true,
		// §8.6.9: a whisper-translate pass carries per-word timings onto the
		// translated segments; the chat engine leaves them empty (segment-only).
		Words: segmentsHaveWordTiming(segments),
	}
	// A translated transcript's effective language is its TARGET language (SPEC
	// §5.2/§8.8), which the operator chose (media.translate.target_langs, §16.2) —
	// an operator pin — so its provenance is "configured". It is recorded
	// independently of the §8.8 source precedence and is the value the §9.5 filter
	// matches (a request for the target language returns translations into it).
	if strings.TrimSpace(meta.Language) != "" {
		meta.LanguageSource = langSourceConfigured
	}
	// §8.6.12: a translation of an ADDITIONAL track (N ≥ 1) records the same 0-based
	// track index as its source transcript, so the derivative is attributable to the
	// track it came from. Track 0 omits it (byte-for-byte unchanged).
	if track > 0 {
		meta.Track = track
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal translated transcript meta: %w", err)
	}

	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     TranscriptRepTypeForTrack(track, targetLang),
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

	if err := s.repGen.upsertChunksForRepresentation(ctx, repID, "text", segments, decision); err != nil {
		return fmt.Errorf("persist translated transcript chunks: %w", err)
	}
	return nil
}

// GenerateTranscriptRepresentation exposes transcript generation for tests.
func (s *Service) GenerateTranscriptRepresentation(ctx context.Context, doc model.Document, content []byte) error {
	_, err := s.generateTranscriptRepresentation(ctx, doc, content)
	return err
}

// annotationPreview bounds the flattened annotation text returned to the caller
// as a preview, cutting on a rune boundary so a multi-byte character is never
// split.
func annotationPreview(flattened string) string {
	const maxPreviewRunes = 240
	if runes := []rune(flattened); len(runes) > maxPreviewRunes {
		return string(runes[:maxPreviewRunes]) + "..."
	}
	return flattened
}

// screenAnnotationForSecrets refuses an annotation whose JSON or flattened text
// matches a configured secret pattern (#681). Both forms are chunked and
// embedded, so both are screened before the persisting transaction opens. The
// JSON text contains the flattened text's values, but it is screened in its own
// right because it carries the keys and structure too, and because it is indexed
// even when indexFlattenedText is false.
//
// Two things differ from the ingest producers. First, this is a standalone MCP
// entry point (`dir2mcp_annotate`), not a step in a scan, so it opens its own
// per-document secret scope from configuration. Second, it REFUSES the annotation
// without withholding the source document: the annotation is model output ABOUT
// an already-scanned document, so a match here says the model wrote a credential
// shape, not that the corpus file holds one. Retiring a healthy document's
// representations on that evidence would be wrong.
//
// The refusal is an error, not an empty preview: this tool otherwise answers
// "stored": true and echoes the annotation back to the caller. The returned
// sentinel and its message carry no part of the matched text.
func (s *Service) screenAnnotationForSecrets(doc model.Document, jsonText, flattened string) error {
	patterns, err := compileSecretPatterns(s.cfg.SecretPatterns)
	if err != nil {
		return fmt.Errorf("compile secret patterns: %w", err)
	}
	s.beginDocumentSecretScope(patterns)
	if !s.derivedTextHasSecret(jsonText) && !s.derivedTextHasSecret(flattened) {
		return nil
	}
	s.getLogger().Printf(
		"secret policy: refusing to store the annotation for %s; it matched a configured security.secret_patterns entry",
		doc.RelPath,
	)
	return ErrSecretExcluded
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

	if err := s.screenAnnotationForSecrets(doc, jsonText, trimmed); err != nil {
		return "", err
	}
	preview := annotationPreview(flattened)

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
	if err := statefs.MkdirAll(cacheDir); err != nil {
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
		// §14.4: a provider/transport OCR failure is classified OCR_FAILED (not the
		// generic EXTRACT_FAILED) once it reaches the run manifest via
		// manifestErrorCode.
		return "", fmt.Errorf("%w: document extract %s: %w", ErrOCRProviderFailure, doc.RelPath, err)
	}

	ocrBytes := []byte(strings.ReplaceAll(strings.ReplaceAll(ocrText, "\r\n", "\n"), "\r", "\n"))
	if err := statefs.WriteFile(cachePath, ocrBytes); err != nil {
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

// CanExtractSourceText reports whether an active extraction engine produces
// source text for doc's format on the on-demand annotation path — i.e. the
// per-format route (§7.4.B.1) is a real engine (structured, pandoc, or flat OCR)
// and the engine that route needs is wired. It is the single routing-aware guard
// the MCP layer consults so it does not re-implement selectExtractionRoute. A
// pandoc-routed born-digital format is covered when pandoc is active even if the
// primary extractor is nil.
func (s *Service) CanExtractSourceText(doc model.Document) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(doc.RelPath)))
	switch s.routeExtractionExt(ext) {
	case routePandoc:
		return s.pandocExtractor != nil
	case routeStructured, routeFlatOCR:
		return s.extractor != nil
	default:
		return false
	}
}

// ExtractSourceText returns the plain extracted Markdown for doc's content,
// routing born-digital office/markup/ebook formats through the pandoc engine
// (T2, #393) and every other format through the primary OCR/extractor. It is the
// on-demand annotation source path's single entry point, mirroring how
// generateOCRMarkdownRepresentation routes at index time, so the MCP layer does
// not duplicate the routing decision. Callers must have set the primary
// extractor (SetDocumentExtractor) and activated pandoc (ActivatePandocEngine),
// and should guard with CanExtractSourceText first. The computed text is written
// to the corresponding cache (OCR or pandoc); PurgeExtractionCache removes it.
func (s *Service) ExtractSourceText(ctx context.Context, doc model.Document, content []byte) (string, error) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(doc.RelPath)))
	if s.pandocExtractor != nil && s.routeExtractionExt(ext) == routePandoc {
		return s.readOrComputePandoc(ctx, doc, content)
	}
	return s.readOrComputeOCR(ctx, doc, content)
}

// PurgeExtractionCache removes the cache entry ExtractSourceText wrote for doc's
// content, routing to the pandoc cache for pandoc-routed born-digital formats and
// the OCR cache otherwise. The MCP secret-pattern gate (#407) uses it so a refused
// extraction is never left persisted on disk, regardless of which engine produced
// it.
func (s *Service) PurgeExtractionCache(doc model.Document, content []byte) {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(doc.RelPath)))
	if s.pandocExtractor != nil && s.routeExtractionExt(ext) == routePandoc {
		s.PurgePandocCache(content)
		return
	}
	s.PurgeOCRCache(content)
}

// PurgeOCRCache removes the OCR cache entry for the given content, using the
// same active-extractor key readOrComputeOCR writes under. Callers that gate the
// returned OCR text (e.g. the MCP secret-pattern gate) use this so a refused
// result is never left persisted in {StateDir}/cache/ocr. Missing files are
// ignored.
func (s *Service) PurgeOCRCache(content []byte) {
	cachePath := filepath.Join(s.cfg.StateDir, "cache", "ocr", s.ocrCacheKey(content)+".md")
	if err := os.Remove(cachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.getLogger().Printf("purge ocr cache: %v", err)
	}
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
	if err := statefs.MkdirAll(cacheDir); err != nil {
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
	if err := statefs.WriteFile(cachePath, transcriptBytes); err != nil {
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

// readOrComputeWhisperTranslation produces the English transcript for
// media.translate.engine=whisper by re-decoding the SOURCE audio with Whisper's
// native translate task (task=translate). Unlike the chat path it deliberately
// ignores the source transcript text: Whisper re-segments the audio and returns
// its own [mm:ss] English lines with their own timings. The result is cached
// under {StateDir}/cache/transcribe with a "-translate" discriminator so it
// never collides with the source-language transcribe cache for the same bytes.
func (s *Service) readOrComputeWhisperTranslation(ctx context.Context, doc model.Document, content []byte) (string, []model.TimedWord, error) {
	if s.translateSTT == nil {
		return "", nil, errors.New("whisper translate transcriber not configured")
	}

	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "transcribe")
	if err := statefs.MkdirAll(cacheDir); err != nil {
		return "", nil, fmt.Errorf("create transcript cache dir: %w", err)
	}

	// Whisper translate always targets English; key on the media bytes + STT
	// identity (transcriptCacheKey, matching the source transcript so a provider
	// change re-derives) plus a "-translate" discriminator and the en suffix so
	// the file is distinct from the source transcript's cache entry. Per-word
	// timings ride along in a .words.json sidecar (as the source transcribe path
	// does) so the English track can be broadcast-segmented, not just chunked.
	// The windowing size changes the produced timings, so fold it into the key —
	// otherwise a changed media.translate.whisper_window_sec would silently reuse
	// stale text/words from a different window setting.
	base := s.transcriptCacheKey(content) + "-translate" + TranscriptLangSuffix("en")
	if s.cfg.MediaTranslateWhisperWindowSec > 0 {
		base += fmt.Sprintf("-w%d", s.cfg.MediaTranslateWhisperWindowSec)
	}
	cachePath := filepath.Join(cacheDir, base+".txt")
	wordsPath := filepath.Join(cacheDir, base+".words.json")
	if cached, err := os.ReadFile(cachePath); err == nil {
		return string(cached), readCachedWords(wordsPath), nil
	}

	translated, words, err := s.translateStructuredWindowed(ctx, doc, content)
	if err != nil {
		// A whisper-translate decode is a TRANSLATION failure (§14.4 TRANSLATE_FAILED),
		// not a transcript one — tag it with the translate sentinel directly so the
		// error chain is unambiguous (previously it carried ErrTranscriptProviderFailure
		// and relied on manifestErrorCode's ordering to still classify it correctly).
		return "", nil, fmt.Errorf("%w: whisper-translate %s: %w", ErrTranslateProviderFailure, doc.RelPath, err)
	}
	translatedBytes := []byte(strings.ReplaceAll(strings.ReplaceAll(translated, "\r\n", "\n"), "\r", "\n"))
	if err := statefs.WriteFile(cachePath, translatedBytes); err != nil {
		return "", nil, fmt.Errorf("write whisper-translate cache: %w", err)
	}
	s.writeCachedWords(wordsPath, words)
	shouldEnforceAfterWrite := s.markOCRCacheWrite()
	if shouldEnforceAfterWrite {
		// Participate in the same cache-policy enforcement as the source
		// transcript write (readOrComputeTranscriptWithWords) so the translate
		// cache is bounded under the same operational policy and cannot grow
		// unbounded.
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
	return string(translatedBytes), words, nil
}

// transcribeWith runs the given transcriber, preferring the structured
// (word-timing) capability when available. Providers that do not implement
// model.StructuredTranscriber return no words, leaving downstream behaviour
// unchanged. transcribe and translateStructured both delegate here, differing
// only in which configured transcriber they pass.
func (s *Service) transcribeWith(ctx context.Context, stt model.Transcriber, relPath string, content []byte) (string, []model.TimedWord, error) {
	if st, ok := stt.(model.StructuredTranscriber); ok {
		res, err := st.TranscribeStructured(ctx, relPath, content)
		if err != nil {
			return "", nil, err
		}
		return res.Text, res.Words, nil
	}
	text, err := stt.Transcribe(ctx, relPath, content)
	if err != nil {
		return "", nil, err
	}
	return text, nil, nil
}

// translateStructured runs the whisper translate transcriber, preferring the
// structured (word-timing) capability so the English track carries per-word
// timings for broadcast segmentation; it degrades to text-only for a translate
// transcriber that does not implement model.StructuredTranscriber.
func (s *Service) translateStructured(ctx context.Context, doc model.Document, content []byte) (string, []model.TimedWord, error) {
	return s.transcribeWith(ctx, s.translateSTT, doc.RelPath, content)
}

// transcribe calls the configured transcriber, preferring the structured
// (word-timing) capability when available. Providers that do not implement
// model.StructuredTranscriber return no words, leaving downstream behaviour
// unchanged.
//
// A video document (issue #495) has no audio-only container that STT providers
// accept directly, so its audio track is demuxed/re-encoded to a compact clip via
// ffmpeg first and handed to the provider under an audio filename; audio documents
// are passed through unchanged. A video with no audio track degrades to an empty
// transcript (no error) so the caller records "no transcript" rather than failing.
func (s *Service) transcribe(ctx context.Context, doc model.Document, content []byte) (string, []model.TimedWord, error) {
	relPath := doc.RelPath
	if doc.DocType == "video" {
		audio, err := s.extractVideoAudioTrack(ctx, doc, content)
		if err != nil {
			if errors.Is(err, avutil.ErrNoAudioStream) {
				s.getLogger().Printf("no audio track to transcribe in video %s; skipping STT", doc.RelPath)
				return "", nil, nil
			}
			return "", nil, err
		}
		content = audio
		relPath = videoAudioRelPath(doc.RelPath)
	}
	return s.transcribeWith(ctx, s.transcriber, relPath, content)
}

// extractVideoAudioTrack demuxes a video's audio track to a compact STT-ready
// clip (issue #495). avutil slices by file path, so the in-memory bytes are staged
// to a temp file (mirroring the windowed-translate path) before extraction. It
// propagates avutil.ErrNoAudioStream so the caller can degrade a soundless video
// gracefully, and avutil.ErrToolNotFound when ffmpeg is absent.
func (s *Service) extractVideoAudioTrack(ctx context.Context, doc model.Document, content []byte) ([]byte, error) {
	tmp, err := os.CreateTemp("", "dir2mcp-vaudio-*"+filepath.Ext(doc.RelPath))
	if err != nil {
		return nil, fmt.Errorf("stage video for audio extraction: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write staged video: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("flush staged video: %w", err)
	}
	extract := s.ExtractAudioTrackFunc
	if extract == nil {
		extract = avutil.ExtractAudioTrack
	}
	return extract(ctx, tmpPath)
}

// videoAudioRelPath rewrites a video's path to the extracted audio clip's filename
// so the STT provider infers an audio MIME type from the extension rather than a
// video container it would reject (issue #495). ExtractAudioTrack emits an m4a
// (AAC) clip, so the suffix matches the actual bytes handed to the provider.
func videoAudioRelPath(relPath string) string {
	return strings.TrimSuffix(relPath, filepath.Ext(relPath)) + ".m4a"
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
	if err := statefs.WriteFile(path, raw); err != nil {
		s.getLogger().Printf("write transcript words cache (%s): %v", path, err)
	}
}

// ReadOrComputeTranscript exposes transcript cache lookup/computation for tests.
func (s *Service) ReadOrComputeTranscript(ctx context.Context, doc model.Document, content []byte, language string) (string, error) {
	return s.readOrComputeTranscript(ctx, doc, content, language)
}

// PurgeTranscriptCache removes the transcript cache entry (and its word-timing
// sidecar) for the given content+language, using the same active-STT key
// readOrComputeTranscriptWithWords writes under. Callers that gate the returned
// transcript (e.g. the MCP secret-pattern gate) use this so refused text is
// never left persisted in {StateDir}/cache/transcribe. Missing files are
// ignored.
func (s *Service) PurgeTranscriptCache(content []byte, language string) {
	cacheDir := filepath.Join(s.cfg.StateDir, "cache", "transcribe")
	key := s.transcriptCacheKey(content)
	base := key + TranscriptLangSuffix(language)
	// Also purge the whisper-translate English track (+ its .words.json sidecar):
	// readOrComputeWhisperTranslation keys on the same content bytes with a
	// "-translate" discriminator and the en suffix, and gated (e.g. secret-pattern
	// refused) translated text must not be left persisted after a purge.
	translateBase := key + "-translate" + TranscriptLangSuffix("en")
	for _, p := range []string{
		base + ".txt", base + ".words.json",
		translateBase + ".txt", translateBase + ".words.json",
	} {
		if err := os.Remove(filepath.Join(cacheDir, p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.getLogger().Printf("purge transcript cache: %v", err)
		}
	}
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
