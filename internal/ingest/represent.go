package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest/docling"
	"github.com/dirstral/dir2mcp/internal/langdetect"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/subtitle"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// transcriptTimestampBracketedRe matches leading timestamps in [mm:ss],
// [hh:mm:ss], or either form with an optional sub-second fraction of 1-3 digits
// ([mm:ss.mmm]). The fraction preserves millisecond precision so distinct
// in-second segments do not collapse onto one marker (issue #431); a bare
// whole-second marker parses as .000, keeping backward compatibility.
var transcriptTimestampBracketedRe = regexp.MustCompile(`^\s*\[(\d+):(\d{2})(?::(\d{2}))?(?:\.(\d{1,3}))?\]\s*(.*)$`)

// transcriptTimestampBareRe matches the same timestamps as
// transcriptTimestampBracketedRe but without the surrounding brackets.
var transcriptTimestampBareRe = regexp.MustCompile(`^\s*(\d+):(\d{2})(?::(\d{2}))?(?:\.(\d{1,3}))?(?:\s+|$)(.*)$`)

const (
	// RepTypeRawText is the representation type for raw text content
	RepTypeRawText = "raw_text"
	// RepTypeMedia is a chunk embedded directly from source media bytes via
	// the multimodal embedder (SPEC 8.1.7), as opposed to extracted/transcribed
	// text. The chunk has no text body; its bytes live at the document path.
	RepTypeMedia = "media"
	// RepTypeExtractedMarkdown is the representation type for extractor-generated markdown
	RepTypeExtractedMarkdown = "extracted_markdown"
	// RepTypeOCRMarkdown is retained as a backward-compatible alias.
	RepTypeOCRMarkdown = RepTypeExtractedMarkdown
	// RepTypeTranscript is the representation type for audio transcripts
	RepTypeTranscript = "transcript"
	// RepTypeAnnotationJSON is the representation type for structured annotations
	RepTypeAnnotationJSON = "annotation_json"
	// RepTypeAnnotationText is the representation type for flattened annotation text
	RepTypeAnnotationText = "annotation_text"
	// RepTypeRecognition is the representation type for recognition-backend
	// annotations over media (design 0004): time-ranged statements persisted
	// as one chunk per annotation with a `time` span.
	RepTypeRecognition = "recognition"
)

// TranscriptRepType returns the rep_type for a transcript in the given language.
// The empty (source/undifferentiated) language keeps the bare "transcript"
// rep_type so it remains interchangeable with the STT transcript; each
// non-empty language gets a distinct, suffixed rep_type ("transcript-en") so
// per-language transcripts coexist under the store's UNIQUE(doc_id, rep_type)
// constraint instead of overwriting one another (spec §8.6.2/§8.6.4 keying).
// The matching read-side query (store.TranscriptRepresentations) selects both
// the bare and the suffixed forms.
func TranscriptRepType(language string) string {
	return RepTypeTranscript + TranscriptLangSuffix(language)
}

// TrackRepQualifier returns the rep_type track qualifier for a 0-based
// AUDIO-relative track index (SPEC §8.6.12). Track 0 (the container's first
// audio stream) returns the empty string so it keeps the BARE "transcript"
// rep_type — a single-track corpus is byte-for-byte unchanged and never
// re-derives on upgrade. Each additional track N ≥ 1 returns "@t<N>", producing
// the distinct "transcript@t<N>" rep_type. The "@" delimiter never appears in a
// BCP-47 language suffix (which is "-"-prefixed), so a full rep-key
// transcript[@t<N>][-<lang>] parses the track qualifier and the language suffix
// independently.
func TrackRepQualifier(track int) string {
	if track <= 0 {
		return ""
	}
	return fmt.Sprintf("@t%d", track)
}

// TranscriptRepTypeForTrack composes a transcript rep_type for a given
// 0-based audio track and optional language (SPEC §8.6.12/§8.6.2):
// transcript[@t<N>][-<lang>]. Track 0 with an empty language yields the bare
// "transcript"; track 2 into English yields "transcript@t2-en".
func TranscriptRepTypeForTrack(track int, language string) string {
	return RepTypeTranscript + TrackRepQualifier(track) + TranscriptLangSuffix(language)
}

// RepresentationGenerator handles creation of representations from documents
type RepresentationGenerator struct {
	store model.RepresentationStore
	// langDetectEnabled turns on best-effort language auto-detection for
	// raw_text representations (SPEC §8.8). Default false; the Service enables it
	// from config. A raw_text rep has no operator pin or source declaration, so
	// detection is the only language signal (recorded as language_source=detected).
	langDetectEnabled bool
	// contextualizer generates the per-chunk, document-aware context contextual
	// retrieval prepends to a chunk's EMBED input (SPEC §8.1.8). Nil (default)
	// means the feature is off — the Service binds it only when contextual
	// retrieval is effectively enabled.
	contextualizer ChunkContextualizer
	// maxFileBytes is the resolved `ingest.max_file_mb` cap in bytes, set by the
	// Service from the single resolver (ResolvedMaxFileBytes). Zero means "not set"
	// and selects the shared 10 MiB default bound.
	//
	// Before #830 the raw-text gate compared against a HARD-CODED 10 MiB instead,
	// so the operator's setting was not the one enforced: with the 20 MiB default a
	// 15 MiB text file passed discovery and then failed here, and the error did not
	// name the reason. The configured value is authoritative now.
	maxFileBytes int64
}

// SetMaxFileBytes plumbs the resolved `ingest.max_file_mb` cap (in bytes) into the
// raw-text size gate and the raw-text read (#830). Callers pass
// ResolvedMaxFileBytes(cfg) so this gate cannot enforce a different number from
// discovery and the source reads.
func (rg *RepresentationGenerator) SetMaxFileBytes(maxBytes int64) {
	rg.maxFileBytes = maxBytes
}

// rawTextCapBytes is the resolved cap the raw-text paths enforce. The shared
// resolver turns an unset value into the default bound and clamps an absurd one,
// so `cap+1` never overflows into a negative limit.
func (rg *RepresentationGenerator) rawTextCapBytes() int64 {
	return corpusfs.ResolveReadCapBytes(rg.maxFileBytes)
}

// SetLanguageDetection enables or disables best-effort raw_text language
// auto-detection (SPEC §8.8). Off by default so callers that do not opt in keep
// the prior behavior (no recorded language).
func (rg *RepresentationGenerator) SetLanguageDetection(enabled bool) {
	rg.langDetectEnabled = enabled
}

// detectedLanguageMeta is the meta_json recorded on a raw_text representation
// whose language was auto-detected (SPEC §5.2/§8.8). It is marshaled only on the
// detection-succeeded path; when detection is disabled or yields unknown,
// rawTextLanguageMeta returns "" directly and this struct is never built.
type detectedLanguageMeta struct {
	Language           string  `json:"language,omitempty"`
	LanguageSource     string  `json:"language_source,omitempty"`
	LanguageConfidence float64 `json:"language_confidence,omitempty"`
}

// rawTextLanguageMeta returns the meta_json for a raw_text representation when
// language detection is enabled and succeeds, else "" (unknown — a first-class,
// non-error state). Detection never fails ingestion. The detected tag is NOT
// part of any derivation identity (raw_text has none; its rep_hash is content
// only), so a detector change can refresh the language without re-embedding.
func (rg *RepresentationGenerator) rawTextLanguageMeta(text string) string {
	if !rg.langDetectEnabled {
		return ""
	}
	tag, confidence, ok := langdetect.Detect(text, langdetect.DefaultMinConfidence)
	if !ok {
		return ""
	}
	encoded, err := json.Marshal(detectedLanguageMeta{
		Language:           tag,
		LanguageSource:     langSourceDetected,
		LanguageConfidence: confidence,
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}

// RepresentationGenerator handles creation of representations from documents.
// The backing store must satisfy model.RepresentationStore which is defined
// in the model package so that both ingest and store can depend on the same
// interface without forming a cyclic dependency.

// (no local interface required – model.RepresentationStore already declares
// UpsertRepresentation, InsertChunkWithSpans, SoftDeleteChunksFromOrdinal and
// WithTx.)

// NewRepresentationGenerator creates a new representation generator
//
// The provided store must be non-nil.  A nil store would otherwise lead to a
// nil-pointer panic later when methods like GenerateRawText are invoked.  By
// validating up-front we fail fast with a clear message helping callers
// diagnose the issue.
func NewRepresentationGenerator(store model.RepresentationStore) *RepresentationGenerator {
	if store == nil {
		// Mention the concrete interface type so callers can more easily
		// correlate the panic with the constructor signature.  The previous
		// message simply said “nil representationStore” which is vague when
		// reading from code; by spelling out model.RepresentationStore the
		// panic makes the required parameter clearer.
		panic("NewRepresentationGenerator: nil model.RepresentationStore provided")
	}
	return &RepresentationGenerator{store: store}
}

// GenerateRawText creates a raw_text representation for text-based documents.
// It reads the file content, normalizes to UTF-8, and stores it as a representation.
//
// According to SPEC §7.4:
// - For code/text/md/data/html doc types
// - Normalize to UTF-8 with \n line endings
// - Route code → index_kind=code, others → index_kind=text
//
// The read is bounded at cap+1 bytes (#830). It used to be a stat followed by an
// unbounded os.ReadFile, and the stat could not constrain the read: a file that
// grew between the two was pulled into memory whole, at whatever size it had
// reached. The stat is gone because it measured nothing the bounded read does not
// decide for itself.
func (rg *RepresentationGenerator) GenerateRawText(ctx context.Context, doc model.Document, absPath string) error {
	capBytes := rg.rawTextCapBytes()
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("open file %s: %w", doc.RelPath, err)
	}
	defer func() { _ = f.Close() }()
	// cap+1: the extra byte separates a file of exactly the cap (admitted) from one
	// byte past it (refused). The prefix is dropped rather than indexed, so half a
	// document is never stored as though it were the whole one.
	content, err := io.ReadAll(io.LimitReader(f, capBytes+1))
	if err != nil {
		return fmt.Errorf("read file %s: %w", doc.RelPath, err)
	}
	if int64(len(content)) > capBytes {
		return rg.rawTextOverCapError(doc.RelPath, capBytes)
	}
	return rg.GenerateRawTextFromContent(ctx, doc, content)
}

// rawTextOverCapError renders the over-cap verdict for the raw-text paths. It
// wraps ErrFileTooLarge so §14.4 classification keeps reporting FILE_TOO_LARGE,
// and it names the setting the operator has to change. It does NOT report the
// file's size: the read stopped at cap+1 by design, so the size is unknown here,
// and reading to the end just to measure it would spend the resources the bound
// exists to save.
func (rg *RepresentationGenerator) rawTextOverCapError(relPath string, capBytes int64) error {
	return fmt.Errorf("%w: file %s passed the ingest.max_file_mb cap (%d bytes)", ErrFileTooLarge, relPath, capBytes)
}

// GenerateRawTextFromContent behaves like GenerateRawText but takes the
// document bytes as an argument.  This is useful when the caller already
// loaded the file (e.g. during a scan) and wants to avoid re-reading it.
// The absolute path is no longer required; callers that previously had it
// simply read the file to supply the content.  Removing the parameter
// simplifies the API and avoids unused variable warnings.
func (rg *RepresentationGenerator) GenerateRawTextFromContent(ctx context.Context, doc model.Document, content []byte) error {
	// Guard against huge files to avoid OOM, against the cap the operator
	// CONFIGURED (#830). This gate used to compare against a hard-coded 10 MiB, so
	// with the 20 MiB default a 15 MiB text file was admitted by discovery and then
	// refused here: the operator's `ingest.max_file_mb` was not the number enforced.
	// The configured value wins, so a file between 10 MiB and the cap is indexed.
	if capBytes := rg.rawTextCapBytes(); int64(len(content)) > capBytes {
		return rg.rawTextOverCapError(doc.RelPath, capBytes)
	}

	// Validate and normalize UTF-8
	normalizedContent := normalizeUTF8(content)

	// Compute representation hash
	repHash := computeRepHash(normalizedContent)

	// Create representation
	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeRawText,
		RepHash:     repHash,
		MetaJSON:    rg.rawTextLanguageMeta(string(normalizedContent)),
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}

	segments := chunkRawTextByDocType(doc.DocType, string(normalizedContent))
	// A non-empty source should never silently produce zero chunks. If this
	// happens, surface an error so the caller can mark/document the failure.
	if strings.TrimSpace(string(normalizedContent)) != "" && len(segments) == 0 {
		return fmt.Errorf("chunking produced zero segments for non-empty %s", doc.RelPath)
	}
	return rg.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, rep)
		if err != nil {
			return fmt.Errorf("upsert representation: %w", err)
		}
		if err := rg.upsertChunksForRepresentationWithStore(ctx, tx, repID, indexKindForDocType(doc.DocType), segments, quarantineDecision{}); err != nil {
			return err
		}
		return nil
	})
}

// PersistSummary writes a `summary` representation and its chunk(s) (SPEC §5.2,
// hierarchical retrieval §9.7). The summary text is chunked with the same
// markdown/text chunker documents use, so a summary longer than one chunk is
// split rather than truncated, and every chunk is inserted on the TEXT logical
// axis (§6.1) with embedding_status=pending — the existing embedding worker then
// embeds it as an ADDITIVE vector in the same space as the document's fine
// chunks (no embed-identity change, §8.1.4).
//
// Stale chunks from a previous, longer summary are soft-deleted, so a re-derived
// summary never leaves orphaned coarse vectors behind.
func (rg *RepresentationGenerator) PersistSummary(ctx context.Context, rep model.Representation, summaryText string) error {
	if strings.TrimSpace(summaryText) == "" {
		return fmt.Errorf("summary text must be non-empty")
	}
	segments := chunkRawTextByDocType("md", summaryText)
	if len(segments) == 0 {
		return fmt.Errorf("chunking produced zero segments for the summary of doc %d", rep.DocID)
	}
	return rg.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, rep)
		if err != nil {
			return fmt.Errorf("upsert summary representation: %w", err)
		}
		return rg.upsertChunksForRepresentationWithStore(ctx, tx, repID, "text", segments, quarantineDecision{})
	})
}

// GenerateMediaChunks emits one media chunk per span of a media document so
// each is embedded directly from its bytes via the multimodal embedder (SPEC
// 8.1.7), rather than via extracted text. The caller decides the unit: an
// image is a single `page` span, a PDF is one `page` span per page, and audio/
// video are one `time` span per window. The embedding worker extracts that
// page/segment from the file at MediaRef before embedding. Each chunk carries
// no text and exactly one span; an empty spans slice produces no chunks.
// contentHash dedups the representation across unchanged re-ingests.
func (rg *RepresentationGenerator) GenerateMediaChunks(ctx context.Context, doc model.Document, contentHash string, spans []model.Span) error {
	if len(spans) == 0 {
		return nil
	}
	rep := model.Representation{
		DocID:       doc.DocID,
		RepType:     RepTypeMedia,
		RepHash:     contentHash,
		CreatedUnix: time.Now().Unix(),
		Deleted:     false,
	}
	return rg.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, err := tx.UpsertRepresentation(ctx, rep)
		if err != nil {
			return fmt.Errorf("upsert media representation: %w", err)
		}
		for i, span := range spans {
			chunk := model.Chunk{
				RepID:           repID,
				Ordinal:         i,
				Text:            "",
				IndexKind:       "text", // media shares the text vector space
				EmbeddingStatus: "pending",
				Modality:        doc.DocType, // "image" | "pdf" | "audio" | "video"
				MediaRef:        doc.RelPath,
			}
			if _, err := tx.InsertChunkWithSpans(ctx, chunk, []model.Span{span}); err != nil {
				return fmt.Errorf("insert media chunk (ordinal %d): %w", i, err)
			}
		}
		if err := tx.SoftDeleteChunksFromOrdinal(ctx, repID, len(spans)); err != nil {
			return fmt.Errorf("soft delete stale media chunks: %w", err)
		}
		return nil
	})
}

// quarantineDecision is the subset of the ingest quarantine decision the
// chunk writer needs: when quarantine is set, chunks are inserted already-failed
// (embedding_status=error) with the given embedding_error/error_category so the
// embedding worker never embeds them (spec 0.16.0). The zero value inserts
// healthy chunks (embedding_status=pending), preserving prior behaviour.

func (rg *RepresentationGenerator) upsertChunksForRepresentation(ctx context.Context, repID int64, indexKind string, segments []chunkSegment, decision quarantineDecision) error {
	// wrap the entire operation in a transaction so we don't end up with a
	// partial set of chunks if an insertion fails halfway through.  The store
	// implementation handles beginning/committing/rolling back the tx.
	return rg.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		return rg.upsertChunksForRepresentationWithStore(ctx, tx, repID, indexKind, segments, decision)
	})
}

func (rg *RepresentationGenerator) upsertChunksForRepresentationWithStore(ctx context.Context, st model.RepresentationStore, repID int64, indexKind string, segments []chunkSegment, decision quarantineDecision) error {
	embeddingStatus := "pending"
	if decision.quarantine {
		embeddingStatus = "error"
	}
	// Contextual retrieval (SPEC §8.1.8, issue #330): resolve each chunk's
	// document-aware context BEFORE the loop so the parent-document text is built
	// once. Off by default — with no contextualizer bound this is a no-op and
	// every chunk records embedding_mode=disabled, exactly as before.
	contexts := rg.chunkContexts(ctx, segments)
	for i, seg := range segments {
		chunk := model.Chunk{
			RepID:           repID,
			Ordinal:         i,
			Text:            seg.Text,
			TextHash:        computeRepHash([]byte(seg.Text)),
			IndexKind:       indexKind,
			EmbeddingStatus: embeddingStatus,
			EmbeddingError:  decision.embErr,
			ErrorCategory:   decision.category,
			Context:         contexts[i].text,
			EmbeddingMode:   contexts[i].mode,
		}
		// A kind-less span carries no provenance (e.g. a structured chunk whose
		// source elements exposed no page); persist the chunk with no span row
		// rather than fabricating one.
		var spans []model.Span
		if strings.TrimSpace(seg.Span.Kind) != "" {
			spans = []model.Span{seg.Span}
		}
		if _, err := st.InsertChunkWithSpans(ctx, chunk, spans); err != nil {
			return fmt.Errorf("insert chunk %d: %w", i, err)
		}
	}
	if err := st.SoftDeleteChunksFromOrdinal(ctx, repID, len(segments)); err != nil {
		return fmt.Errorf("soft delete stale chunks: %w", err)
	}
	return nil
}

// chunkContextState is one chunk's resolved contextual-retrieval outcome: the
// generated context (empty unless generation succeeded) and the durable
// per-chunk embedding_mode that disambiguates it (SPEC §5.3/§8.1.8).
type chunkContextState struct {
	text string
	mode string
}

// SetContextualizer binds the per-chunk context generator used by contextual
// retrieval (SPEC §8.1.8). Nil (the default) leaves the feature off: every chunk
// records embedding_mode=disabled with no context, and the chunk writer is
// byte-for-byte what it was before the feature existed.
func (rg *RepresentationGenerator) SetContextualizer(c ChunkContextualizer) {
	rg.contextualizer = c
}

// chunkContexts resolves the document-aware context for every segment of one
// representation, returning states aligned 1:1 with segments.
//
// The "parent document" is the representation's own text — the segments joined
// in order — so the generator sees the whole document a chunk came from without
// the chunk writer needing the upstream source bytes. That joined text is also
// what the cache key hashes (design 0004 §3), so a change ANYWHERE in the
// document re-derives every one of its chunk contexts.
//
// Generation is FAIL-OPEN PER CHUNK (SPEC §8.1.8): a generator error leaves that
// chunk with no context and mode=fallback — it embeds raw, in the same vector
// space as its contextualized neighbours — instead of failing ingest. A fallback
// chunk is retried on the next scan while contextualization stays on.
func (rg *RepresentationGenerator) chunkContexts(ctx context.Context, segments []chunkSegment) []chunkContextState {
	states := make([]chunkContextState, len(segments))
	for i := range states {
		states[i].mode = model.EmbeddingModeDisabled
	}
	if rg.contextualizer == nil || len(segments) == 0 {
		return states
	}
	docText := joinSegmentTexts(segments)
	for i, seg := range segments {
		if strings.TrimSpace(seg.Text) == "" {
			continue // nothing to situate; stays disabled, embeds raw
		}
		generated, err := rg.contextualizer.Contextualize(ctx, docText, seg.Text)
		if err != nil || strings.TrimSpace(generated) == "" {
			// Never log the chunk text or the generated context (they are corpus
			// content); the error alone is what an operator needs.
			states[i].mode = model.EmbeddingModeFallback
			continue
		}
		states[i] = chunkContextState{text: generated, mode: model.EmbeddingModeContextualized}
	}
	return states
}

// joinSegmentTexts reconstructs the parent-document text a representation's
// chunks were cut from, in chunk order.
func joinSegmentTexts(segments []chunkSegment) string {
	parts := make([]string, 0, len(segments))
	for _, seg := range segments {
		parts = append(parts, seg.Text)
	}
	return strings.Join(parts, "\n")
}

// binarySniffLen bounds how many leading bytes looksLikeBinaryContent inspects.
const binarySniffLen = 8000

// looksLikeBinaryContent reports whether content appears to be binary rather
// than text, using the same NUL-byte heuristic git uses: a NUL byte within the
// leading window is a strong, very-low-false-positive signal of binary data
// (plain text — source code, CSV/TSV, JSON/JSONL, XML, YAML, TOML — never
// contains one). It guards the raw-text path (SPEC §7.4 "data" docs) against
// indexing binary blobs — e.g. Parquet, which classifies as "data" — as
// U+FFFD replacement-character soup (#398).
func looksLikeBinaryContent(content []byte) bool {
	n := len(content)
	if n > binarySniffLen {
		n = binarySniffLen
	}
	return bytes.IndexByte(content[:n], 0x00) >= 0
}

// normalizeUTF8 ensures content is valid UTF-8 and normalizes line endings to \n
// Invalid byte sequences are replaced with the Unicode replacement character
// and the resulting slice is returned.  The previous signature returned an
// error that was never produced; simplifying to a single return value makes
// callers easier to work with.
func normalizeUTF8(content []byte) []byte {
	return NormalizeUTF8(content)
}

// NormalizeUTF8 ensures content is valid UTF-8 and uses LF line endings.
func NormalizeUTF8(content []byte) []byte {
	// Transcode from a detected source encoding (BOM / UTF-16 / legacy
	// single-byte) to UTF-8 and strip a leading UTF-8 BOM before validation
	// (#417). Without this, a UTF-16 file reads as NUL-interleaved garbage and a
	// Latin-1/Windows-1252 file loses every accented byte to U+FFFD.
	content = decodeToUTF8(content)

	// Salvage any residual invalid UTF-8 by replacing with U+FFFD.
	if !utf8.Valid(content) {
		out := strings.ToValidUTF8(string(content), "\uFFFD")
		content = []byte(out)
	}

	// Normalize line endings: convert \r\n and \r to \n
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	content = bytes.ReplaceAll(content, []byte("\r"), []byte("\n"))

	return content
}

// Byte-order marks for the Unicode encodings decodeToUTF8 recognizes.
var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16BE = []byte{0xFE, 0xFF}
	bomUTF16LE = []byte{0xFF, 0xFE}
)

// decodeToUTF8 transcodes content to UTF-8 from a detected source encoding
// before normalization (SPEC \u00A77.4 "normalize to UTF-8"). Detection is
// deterministic and script-agnostic \u2014 it improves CJK, Cyrillic and Latin text
// uniformly and hardcodes no locale:
//   - a Unicode BOM (UTF-8, UTF-16 LE/BE) is authoritative: the BOM is consumed
//     and, for UTF-16, the payload is transcoded;
//   - a BOM-less UTF-16 stream is recognized from its NUL-interleaving pattern.
//     UTF-16-encoded ASCII/Latin text is "valid UTF-8" only because its padding
//     NUL bytes are themselves valid UTF-8, so this MUST run before utf8.Valid;
//   - otherwise valid UTF-8 is kept as-is;
//   - remaining invalid-UTF-8 bytes are treated as a legacy single-byte encoding
//     (Windows-1252, the superset of ISO-8859-1) rather than destroyed into
//     U+FFFD, so accented Latin bytes (\u00E9=0xE9, \u00FC=0xFC) survive.
//
// It never fails: a transcode error falls back to the original bytes so the
// caller's U+FFFD salvage still applies.
func decodeToUTF8(content []byte) []byte {
	if len(content) == 0 {
		return content
	}
	switch {
	case bytes.HasPrefix(content, bomUTF8):
		return content[len(bomUTF8):]
	case bytes.HasPrefix(content, bomUTF16LE):
		return transcodeUTF16(content[len(bomUTF16LE):], unicode.LittleEndian)
	case bytes.HasPrefix(content, bomUTF16BE):
		return transcodeUTF16(content[len(bomUTF16BE):], unicode.BigEndian)
	}
	if endian, ok := sniffUTF16(content); ok {
		return transcodeUTF16(content, endian)
	}
	if utf8.Valid(content) {
		return content
	}
	// Invalid UTF-8 with no NUL padding: a legacy single-byte encoding. Decode
	// as Windows-1252 (a superset of ISO-8859-1 over 0xA0-0xFF) so accented text
	// is recovered instead of replaced with U+FFFD.
	if out, _, err := transform.Bytes(charmap.Windows1252.NewDecoder(), content); err == nil {
		return out
	}
	return content
}

// transcodeUTF16 decodes UTF-16 bytes of the given endianness to UTF-8. The BOM,
// if any, has already been consumed by the caller, so IgnoreBOM is used.
//
// A BOM or the NUL-interleaving heuristic has already classified this stream as
// UTF-16, so the output must be proper UTF-8 text or U+FFFD replacement chars —
// never the raw interleaved-NUL bytes. Returning the raw bytes on error would be
// unsafe: NUL padding is itself valid UTF-8, so those bytes pass utf8.Valid and
// the caller's U+FFFD salvage never fires, leaving NUL-interleaved garbage. On a
// transform error (e.g. an odd trailing byte that leaves an incomplete final
// code unit) we therefore emit the successfully decoded prefix followed by a
// single U+FFFD for the stray unit.
func transcodeUTF16(content []byte, endian unicode.Endianness) []byte {
	dec := unicode.UTF16(endian, unicode.IgnoreBOM).NewDecoder()
	out, _, err := transform.Bytes(dec, content)
	if err != nil {
		return append(out, "�"...)
	}
	return out
}

// sniffUTF16MinBytes is the smallest input sniffUTF16 will classify; below it the
// NUL-distribution signal is too weak to trust.
const sniffUTF16MinBytes = 4

// sniffUTF16NULPercent is the minimum share (percent) of bytes at a single parity
// that must be NUL for a BOM-less UTF-16 classification.
const sniffUTF16NULPercent = 30

// sniffUTF16 detects a BOM-less UTF-16 stream from its NUL-byte distribution:
// UTF-16-encoded ASCII/Latin text pads every code unit with a 0x00 byte at a
// fixed parity (odd offsets for little-endian, even for big-endian). A large,
// parity-aligned share of NULs is a strong signal; plain UTF-8 text never
// contains NUL, so this does not misfire on legitimate UTF-8. It is deliberately
// conservative (script-agnostic, no locale assumptions).
func sniffUTF16(content []byte) (unicode.Endianness, bool) {
	// Truncate the parity window to an even length so a stray trailing byte
	// (corruption, padding, or an incomplete final code unit) does not defeat
	// detection of an otherwise UTF-16 stream. The dropped odd byte is decoded
	// best-effort (as U+FFFD) by transcodeUTF16.
	n := len(content) &^ 1
	if n < sniffUTF16MinBytes {
		return unicode.LittleEndian, false
	}
	if n > binarySniffLen {
		n = binarySniffLen &^ 1 // keep the window even so parity is meaningful
	}
	var evenNUL, oddNUL int
	for i := 0; i < n; i++ {
		if content[i] != 0x00 {
			continue
		}
		if i%2 == 0 {
			evenNUL++
		} else {
			oddNUL++
		}
	}
	units := n / 2 // one NUL padding byte per UTF-16 code unit at most
	threshold := units * sniffUTF16NULPercent / 100
	switch {
	case oddNUL > evenNUL && oddNUL >= threshold:
		return unicode.LittleEndian, true
	case evenNUL > oddNUL && evenNUL >= threshold:
		return unicode.BigEndian, true
	default:
		return unicode.LittleEndian, false
	}
}

// ShouldGenerateRawText determines if a document should have raw_text representation.
// According to SPEC §7.4, raw_text is generated for:
// - code (go, rs, py, js, ts, java, c, cpp, etc.)
// - text
// - md (markdown)
// - data (json, yaml, toml, etc.)
// - html
func ShouldGenerateRawText(docType string) bool {
	switch docType {
	case "code", "text", "md", "data", "html":
		return true
	default:
		return false
	}
}

// ShouldGenerateExtractedMarkdown determines if a document type should use the
// configured document extractor to generate markdown-like text.
func ShouldGenerateExtractedMarkdown(docType string) bool {
	switch docType {
	case "pdf", "image", "document":
		return true
	default:
		return false
	}
}

// Ingest package chunking parameters.  These constants are the values used
// internally when breaking up transcripts and text into smaller pieces for
// indexing.  They are exported so that tests (and potentially other packages)
// can reason about the limits without duplicating magic numbers.
const (
	// TranscriptChunkMaxChars is the maximum number of runes that will appear in
	// any single chunk produced by chunkTranscriptSegmentWithTiming.  The
	// implementation enforces this bound before trimming whitespace, so the
	// actual text length may be smaller but will never exceed this value.
	TranscriptChunkMaxChars = 1200

	// TranscriptChunkOverlapChars is the number of runes that overlap between
	// adjacent chunks when a transcript segment is split.  Overlap helps ensure
	// that context is preserved across chunk boundaries.
	TranscriptChunkOverlapChars = 120

	// TranscriptChunkMinChars is the minimum number of runes that a non-terminal
	// chunk must contain.  Segments shorter than this threshold are merged with
	// the next window unless they are the final one.
	TranscriptChunkMinChars = 80
)

type chunkSegment struct {
	Text string
	Span model.Span
}

// ChunkSegment is a public test-friendly representation of a chunk span pair.
type ChunkSegment struct {
	Text string
	Span model.Span
}

func indexKindForDocType(docType string) string {
	if docType == "code" {
		return "code"
	}
	return "text"
}

func chunkRawTextByDocType(docType, content string) []chunkSegment {
	if docType == "code" {
		return chunkCodeByLines(content, 200, 30)
	}
	return chunkTextByChars(content, effectiveTextChunkMaxChars, effectiveTextChunkOverlapChars, effectiveTextChunkMinChars)
}

func chunkOCRByPages(content string) []chunkSegment {
	pages := strings.Split(content, "\f")
	out := make([]chunkSegment, 0, len(pages))
	for i, page := range pages {
		page = strings.TrimSpace(page)
		if page == "" {
			continue
		}
		out = append(out, chunkSegment{
			Text: page,
			Span: model.Span{
				Kind: "page",
				Page: i + 1,
			},
		})
	}
	return out
}

// Structured-document chunking parameters (spec §7.5 size constraints). They
// mirror the raw-text defaults so structured and plain text chunk at a
// comparable granularity. These are the historical defaults used when no
// chunking.* config is set; ConfigureChunking may override the effective values
// below.
const (
	structuredChunkMaxChars     = 2500
	structuredChunkOverlapChars = 250
	structuredChunkMinChars     = 200
)

// approxCharsPerToken converts a chunking.*_tokens budget (config is expressed
// in tokens) into the chunker's rune budget. It is a deliberately rough
// heuristic (~4 characters per token for typical prose): the goal is that
// changing chunking.max_tokens moves chunk sizes proportionally, not exact
// tokenizer parity.
const approxCharsPerToken = 4

// Effective text/structured chunk sizing (rune budgets). They default to the
// historical hardcoded values above, so a corpus with no chunking.* config
// chunks byte-identically to before. ConfigureChunking overrides them from
// chunking.max_tokens / chunking.overlap_tokens when those are set. The text and
// structured chunkers share one window so plain and structured documents chunk
// at a comparable granularity (read by chunkRawTextByDocType and
// newStructuredChunker).
var (
	effectiveTextChunkMaxChars     = structuredChunkMaxChars
	effectiveTextChunkOverlapChars = structuredChunkOverlapChars
	effectiveTextChunkMinChars     = structuredChunkMinChars
)

// ConfigureChunking applies chunking.max_tokens / chunking.overlap_tokens to the
// text and structured chunkers. Token budgets are converted to rune budgets via
// approxCharsPerToken. When maxTokens <= 0 (unset) the historical defaults are
// restored, so existing corpora chunk exactly as before and the call is
// idempotent. Callers overlay already-validated config here at startup, before
// ingestion begins; it is not safe to call concurrently with active chunking.
func ConfigureChunking(maxTokens, overlapTokens int) {
	if maxTokens <= 0 {
		effectiveTextChunkMaxChars = structuredChunkMaxChars
		effectiveTextChunkOverlapChars = structuredChunkOverlapChars
		effectiveTextChunkMinChars = structuredChunkMinChars
		return
	}
	maxChars := maxTokens * approxCharsPerToken
	overlapChars := 0
	if overlapTokens > 0 {
		overlapChars = overlapTokens * approxCharsPerToken
	}
	// Scale the minimum-chunk floor to the configured window (preserving the
	// default min:max proportion) so a small window does not drop every
	// non-terminal chunk, and never let it reach the window size.
	minChars := maxChars * structuredChunkMinChars / structuredChunkMaxChars
	if minChars < 1 {
		minChars = 1
	}
	if minChars >= maxChars {
		minChars = maxChars - 1
	}
	// Clamp overlap below the window so the raw-text path (chunkRawTextByDocType,
	// which passes these effective vars straight to chunkTextByChars without
	// normalizeTextChunkParams) can never receive overlap >= max — that would
	// stall/loop chunking. Defense-in-depth even though Validate() rejects it.
	if overlapChars < 0 {
		overlapChars = 0
	}
	if overlapChars >= maxChars {
		overlapChars = maxChars - 1
	}
	effectiveTextChunkMaxChars = maxChars
	effectiveTextChunkOverlapChars = overlapChars
	effectiveTextChunkMinChars = minChars
}

// Code chunking is primarily line-based, but a single window (or a single very
// long line, e.g. a minified JS/CSS bundle) must still be bounded by characters
// so it stays within the embedder's input limit. codeChunkMaxChars is a rune
// budget, kept consistent with the structured/text chunkers.
const (
	codeChunkMaxChars     = structuredChunkMaxChars
	codeChunkOverlapChars = structuredChunkOverlapChars
)

// chunkStructuredBlocks implements section/element-aware chunking over the
// ordered blocks of a structured document (spec §7.5): consecutive blocks
// sharing the same section breadcrumb are grouped, then split by size; tables
// are kept atomic (never merged with surrounding text nor split). Each segment
// carries a region span (page range + primary-page bbox + section), degrading
// to a page span when a block lacks a bounding box.
func chunkStructuredBlocks(blocks []docling.Block) []chunkSegment {
	c := newStructuredChunker()
	for _, b := range blocks {
		if strings.TrimSpace(b.Text) == "" {
			continue
		}
		if b.Label == docling.LabelTable {
			// Tables are atomic: flush any pending prose, then emit the table
			// as a single segment regardless of size.
			c.flush()
			c.emitAtomic(b)
			continue
		}
		c.add(b)
	}
	c.flush()
	return c.out
}

// structuredChunker accumulates consecutive same-section blocks and flushes
// them into size-bounded segments, each tagged with a region span. It exists to
// keep chunkStructuredBlocks simple (one branch per block kind) by moving the
// buffer bookkeeping behind add/flush/emitAtomic.
type structuredChunker struct {
	maxChars     int
	overlapChars int
	minChars     int
	lastPage     int
	out          []chunkSegment
	buf          []docling.Block
	bufText      strings.Builder
	bufRunes     int
	bufSection   []string
}

func newStructuredChunker() *structuredChunker {
	maxChars, overlapChars, minChars := normalizeTextChunkParams(
		effectiveTextChunkMaxChars, effectiveTextChunkOverlapChars, effectiveTextChunkMinChars)
	return &structuredChunker{
		maxChars:     maxChars,
		overlapChars: overlapChars,
		minChars:     minChars,
		// 0 = no page observed yet; spanForBlocks must not fabricate page 1.
		lastPage: 0,
	}
}

// add appends a prose block to the buffer, starting a new chunk when the
// section breadcrumb changes and flushing when the buffer reaches max size.
func (c *structuredChunker) add(b docling.Block) {
	if len(c.buf) > 0 && !sameSection(c.bufSection, b.Section) {
		c.flush()
	}
	if len(c.buf) == 0 {
		c.bufSection = b.Section
	}
	if c.bufText.Len() > 0 {
		c.bufText.WriteString("\n\n")
		c.bufRunes += 2
	}
	c.bufText.WriteString(b.Text)
	c.bufRunes += utf8.RuneCountInString(b.Text)
	c.buf = append(c.buf, b)
	// maxChars is a rune budget; compare against the accumulated rune count, not
	// strings.Builder.Len() (bytes) — otherwise multibyte text (Cyrillic, CJK)
	// flushes at a fraction of the intended size and over-fragments chunks.
	if c.bufRunes >= c.maxChars {
		c.flush()
	}
}

// emitAtomic appends a single block as its own chunk (used for tables).
func (c *structuredChunker) emitAtomic(b docling.Block) {
	c.out = append(c.out, chunkSegment{
		Text: b.Text,
		Span: spanForBlocks([]docling.Block{b}, &c.lastPage),
	})
}

// flush turns the buffered blocks into one or more size-bounded segments
// sharing a single region span, then resets the buffer.
func (c *structuredChunker) flush() {
	text := strings.TrimSpace(c.bufText.String())
	buffered := c.buf
	c.buf = nil
	c.bufText.Reset()
	c.bufRunes = 0
	c.bufSection = nil
	if text == "" || len(buffered) == 0 {
		return
	}
	span := spanForBlocks(buffered, &c.lastPage)
	for _, piece := range splitTextBySize(text, c.maxChars, c.overlapChars, c.minChars) {
		c.out = append(c.out, chunkSegment{Text: piece, Span: span})
	}
}

// ChunkStructuredBlocks splits structured document blocks using the same
// section-aware policy as ingestion. Exported so tests can exercise the logic
// directly (mirrors ChunkTextByChars/ChunkCodeByLines).
func ChunkStructuredBlocks(blocks []docling.Block) []ChunkSegment {
	raw := chunkStructuredBlocks(blocks)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// splitTextBySize breaks text into size-bounded pieces, reusing the char
// chunker but discarding its line spans (structured chunks carry region spans).
func splitTextBySize(text string, maxChars, overlapChars, minChars int) []string {
	segs := chunkTextByChars(text, maxChars, overlapChars, minChars)
	if len(segs) == 0 {
		return []string{text}
	}
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, s.Text)
	}
	return out
}

// sameSection reports whether two section breadcrumbs are identical.
func sameSection(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// spanForBlocks derives a span for a chunk from its source blocks: a region
// span (page range + union bbox on the primary page + section) when a bounding
// box is available, otherwise a page span on the primary (or last-seen) page.
// lastPage carries forward the most recent *observed* page (0 = none seen yet)
// so a block lacking its own page still cites the surrounding page rather than a
// fabricated page 1. When no page has ever been observed it returns a kind-less
// span, which the chunk writer persists with no provenance row (spec: a region
// citation degrades to page-level, never to a made-up page).
func spanForBlocks(bs []docling.Block, lastPage *int) model.Span {
	startPage, endPage, primary := blockPageRange(bs)
	section, label := blockSectionLabel(bs)
	union := unionBBoxOnPage(bs, primary)

	if primary > 0 {
		*lastPage = primary
	}
	if union != nil {
		return model.Span{Kind: "region", Region: &model.RegionSpan{
			StartPage: startPage,
			EndPage:   endPage,
			BBox:      union,
			Section:   section,
			Label:     label,
		}}
	}
	page := primary
	if page <= 0 {
		page = *lastPage
	}
	if page <= 0 {
		// No page ever observed: emit no provenance rather than inventing one.
		return model.Span{}
	}
	return model.Span{Kind: "page", Page: page}
}

// blockPageRange returns the first, last, and primary (first in reading order)
// page across the blocks, ignoring blocks without a page.
func blockPageRange(bs []docling.Block) (startPage, endPage, primary int) {
	for _, b := range bs {
		if b.Page <= 0 {
			continue
		}
		if startPage == 0 || b.Page < startPage {
			startPage = b.Page
		}
		if b.Page > endPage {
			endPage = b.Page
		}
		if primary == 0 {
			primary = b.Page
		}
	}
	return startPage, endPage, primary
}

// blockSectionLabel returns the first non-empty section breadcrumb and the
// first block label across the blocks.
func blockSectionLabel(bs []docling.Block) (section []string, label string) {
	for _, b := range bs {
		if label == "" {
			label = b.Label
		}
		if section == nil && len(b.Section) > 0 {
			section = b.Section
		}
	}
	return section, label
}

// unionBBoxOnPage returns the smallest rectangle enclosing all block bounding
// boxes on the primary page, or nil when none have a box there.
func unionBBoxOnPage(bs []docling.Block, primary int) *model.BBox {
	var union *model.BBox
	for _, b := range bs {
		if b.Page != primary || b.BBox == nil {
			continue
		}
		mb := model.BBox{
			Page: b.BBox.Page, L: b.BBox.L, T: b.BBox.T,
			R: b.BBox.R, B: b.BBox.B, CoordOrigin: b.BBox.CoordOrigin,
		}
		if union == nil {
			cp := mb
			union = &cp
			continue
		}
		union.L = minF(union.L, mb.L)
		union.T = minF(union.T, mb.T)
		union.R = maxF(union.R, mb.R)
		union.B = maxF(union.B, mb.B)
	}
	return union
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// chunkTranscriptByTimeWithWords chunks a transcript exactly like
// chunkTranscriptByTime (same chunks, same text, same time spans — see spec
// §8.6.1: word timing is metadata only and MUST NOT add chunks or change text),
// then attaches each per-word timestamp to the chunk whose time span contains
// the word's start. Words are stored relative-to-nothing — their absolute ms
// offsets are kept as {t,d,w} on the chunk's span. A nil/empty words slice
// produces output byte-for-byte identical to chunkTranscriptByTime, so a
// provider without word timing is unaffected.
func chunkTranscriptByTimeWithWords(content string, words []model.TimedWord) []chunkSegment {
	return chunkTranscriptByTimeWithWordsFiltered(content, words, nil)
}

// chunkTranscriptByTimeWithWordsFiltered is chunkTranscriptByTimeWithWords with
// an optional caption word filter (config media.filter_words) applied to each
// chunk's text after chunking. A nil/inactive filter leaves the output
// byte-for-byte identical. Word timing is attached after filtering so words tied
// to a dropped (now-empty) chunk are not retained.
func chunkTranscriptByTimeWithWordsFiltered(content string, words []model.TimedWord, filter *subtitle.WordFilter) []chunkSegment {
	segs := chunkTranscriptByTime(content)
	segs = applyWordFilterToSegments(segs, filter)
	// Filter the word list at the SOURCE, before attaching it to spans. The
	// segment Text filter above only cleans Text; Span.Words is rebuilt into
	// broadcast cues, so leaving filtered phrases in the word list would
	// re-introduce exactly the boilerplate/credit/watermark phrases the filter
	// stripped. Dropping the matched words here fixes the leak for every
	// word-consumer (broadcast cues and any future one), not just at export.
	words = filterTimedWords(words, filter)
	if len(words) == 0 || len(segs) == 0 {
		return segs
	}
	attachWordsToTimeSpans(segs, words)
	return segs
}

// filterTimedWords removes the per-word timestamps whose tokens fall inside an
// active filter phrase (using the same WordFilter that strips segment text), so
// anything that later rebuilds text from these words is already clean. A
// nil/inactive filter or empty word list returns the input unchanged. Timing on
// the surviving words is preserved verbatim.
func filterTimedWords(words []model.TimedWord, filter *subtitle.WordFilter) []model.TimedWord {
	if !filter.Active() || len(words) == 0 {
		return words
	}
	tokens := make([]string, len(words))
	for i, w := range words {
		tokens[i] = w.Word
	}
	keep := filter.FilterTokens(tokens)
	out := make([]model.TimedWord, 0, len(words))
	for i, w := range words {
		if keep[i] {
			out = append(out, w)
		}
	}
	return out
}

// applyWordFilterToSegments strips configured filter phrases from each segment's
// text (case-insensitive substring removal) and drops segments whose text is
// empty after filtering. A nil/inactive filter returns segs unchanged so the
// empty-config path is a no-op. Spans (timing) are preserved verbatim on the
// surviving segments.
func applyWordFilterToSegments(segs []chunkSegment, filter *subtitle.WordFilter) []chunkSegment {
	if !filter.Active() {
		return segs
	}
	out := make([]chunkSegment, 0, len(segs))
	for _, seg := range segs {
		filtered := filter.Apply(seg.Text)
		if strings.TrimSpace(filtered) == "" {
			continue
		}
		seg.Text = filtered
		out = append(out, seg)
	}
	return out
}

// applyCueCleaningToSegments is the ingest-time cue-cleaning pass: it applies
// the configured media.subtitles.* hallucination filters (drop_urls,
// drop_phrases, scrub_phrases, collapse_repeats) to each chunk segment's text
// before embedding, using the very same subtitle primitives the export pass
// invokes (subtitle.IsURLCue, DropSet.IsSpam/Scrub, subtitle.RepeatCollapser).
// #545 moved drop_phrases/scrub_phrases here; #765 moved drop_urls/
// collapse_repeats, which had stayed export-only, so a hallucinated URL cue or a
// repetition artifact was stripped from the exported sidecar and left embedded:
// retrievable, and citable for a span where nobody said anything.
//
// The segment IS the cue unit on both sides, which is what makes "the index and
// the exported sidecar agree cue-for-cue" literally true: export re-derives its
// cues from these stored chunks (BuildCues over TranscriptSpanChunks), so a
// verdict made per-segment here is a verdict made on exactly the text export
// will clean. It also means a merged chunk is dropped whole when any of its cues
// trips drop_urls, which is precisely what export does with the same chunk; both
// passes are opt-in for that reason.
//
// Order is deliberate and mirrors subtitle.CleanCues step for step, because the
// passes do NOT commute:
//   - drop_urls first: it is a whole-segment verdict on the ORIGINAL text. Run
//     after a scrub, an excision could remove the very token that identified the
//     segment as a credit line, so the same segment would be dropped on export
//     and kept in the index.
//   - drop_phrases before scrub_phrases: a wholly-spam segment must be dropped
//     outright rather than scrubbed down to a punctuation husk that then reads
//     as an empty-but-present cue.
//   - collapse_repeats LAST, on the post-scrub text: two segments that differ
//     only by a leaked hallucination phrase are the same cue once scrubbed, and
//     a run must be counted on the text that is actually stored. Counting first
//     would miss that run in the index while export (which scrubs first) drops
//     it, and the two would disagree.
//
// media.subtitles.glossary is deliberately NOT applied here: SPEC §8.6.2 pins it
// as a deterministic, export-time find/replace on already-rendered cue text, so
// rewriting indexed text with it is a spec change, not a bug fix. Were it ever
// added, it would have to run AFTER the collapse (a rewrite can make two
// distinct texts identical and fabricate a run), exactly as CleanCues orders it.
//
// Inactive options return segs unchanged, so the off-by-default path is a
// byte-for-byte no-op. Spans (timing) are preserved verbatim on surviving
// segments.
func applyCueCleaningToSegments(segs []chunkSegment, opts subtitle.CleanOptions) []chunkSegment {
	if !opts.Active() {
		return segs
	}
	out := make([]chunkSegment, 0, len(segs))
	collapse := subtitle.NewRepeatCollapser(opts.CollapseRepeats)
	for _, seg := range segs {
		cleaned, keep := cleanSegment(seg, opts)
		if !keep {
			continue
		}
		// Every surviving segment must be fed to the collapser, in order, or the
		// run counter loses track of which repeat is the Nth.
		if collapse.Drop(cleaned.Text) {
			continue
		}
		out = append(out, cleaned)
	}
	return out
}

// cleanSegment applies the per-segment half of the cleaning (the passes that
// judge a segment on its own, independent of its neighbours) to one chunk
// segment and reports whether it survives. The stateful collapse pass is the
// caller's job. A segment no rule matched is returned verbatim, byte for byte.
func cleanSegment(seg chunkSegment, opts subtitle.CleanOptions) (chunkSegment, bool) {
	if strings.TrimSpace(seg.Text) == "" {
		return seg, false
	}
	if opts.DropURLs && subtitle.IsURLCue(seg.Text) {
		return seg, false
	}
	if opts.Drop.IsSpam(seg.Text) {
		return seg, false
	}
	if !opts.Scrub.Active() {
		return seg, true
	}
	scrubbed := opts.Scrub.Scrub(seg.Text)
	if strings.TrimSpace(scrubbed) == "" {
		return seg, false
	}
	// Only touch a segment the scrub actually changed. filterWordSpansToText
	// is an approximate multiset token match, so re-running it on an
	// UNCHANGED segment risks silently dropping a word timing on any
	// tokenization/punctuation mismatch (numerals, contractions) — for every
	// segment when scrub_phrases is set, not just scrubbed ones. An unchanged
	// segment keeps its Text and Span.Words verbatim.
	if scrubbed != seg.Text {
		seg.Text = scrubbed
		// Excise the scrubbed words from the per-word timings too: Span.Words is
		// later rebuilt into broadcast cues / other word-level consumers, so
		// leaving the removed phrase's words here would re-introduce the spam the
		// scrub just removed from Text.
		seg.Span.Words = filterWordSpansToText(seg.Span.Words, scrubbed)
	}
	return seg, true
}

// filterWordSpansToText drops per-word timings whose token is no longer present
// in text, so a scrubbed segment's Span.Words carries only its surviving words.
// Matching is a case-insensitive multiset over whitespace tokens with surrounding
// punctuation trimmed, so a word kept N times in text keeps N of its timings (and
// a word removed by the scrub, absent from text, keeps none). Empty input is
// returned unchanged.
func filterWordSpansToText(words []model.WordSpan, text string) []model.WordSpan {
	if len(words) == 0 {
		return words
	}
	counts := make(map[string]int)
	for _, tok := range strings.Fields(text) {
		counts[normalizeWordToken(tok)]++
	}
	out := make([]model.WordSpan, 0, len(words))
	for _, w := range words {
		key := normalizeWordToken(w.W)
		if key != "" && counts[key] > 0 {
			counts[key]--
			out = append(out, w)
		}
	}
	return out
}

// normalizeWordToken lowercases a token and trims surrounding punctuation so a
// whisper word token ("Aju?bei,") compares equal to its occurrence in rebuilt
// segment text.
func normalizeWordToken(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), ".,!?;:…»«()[]{}\"'“”‘’-—"))
}

// shiftTranscriptSpans subtracts offsetMS from every "time" span's bounds and
// from every attached word timestamp, clamping at 0 (dir2mcp#258 leading-silence
// trim). It mutates segs in place and is deterministic for a given input. A
// non-positive offset is a no-op; non-time spans are left untouched. Bounds stay
// valid (EndMS > StartMS); a span that would collapse keeps a 1ms width so the
// downstream EndMS > StartMS invariant holds.
func shiftTranscriptSpans(segs []chunkSegment, offsetMS int) {
	if offsetMS <= 0 {
		return
	}
	for i := range segs {
		if segs[i].Span.Kind != "time" {
			continue
		}
		segs[i].Span.StartMS = clampShift(segs[i].Span.StartMS, offsetMS)
		segs[i].Span.EndMS = clampShift(segs[i].Span.EndMS, offsetMS)
		if segs[i].Span.EndMS <= segs[i].Span.StartMS {
			segs[i].Span.EndMS = segs[i].Span.StartMS + 1
		}
		for w := range segs[i].Span.Words {
			segs[i].Span.Words[w].T = clampShift(segs[i].Span.Words[w].T, offsetMS)
		}
	}
}

// clampShift subtracts offsetMS from ms, never returning below 0.
func clampShift(ms, offsetMS int) int {
	if shifted := ms - offsetMS; shifted > 0 {
		return shifted
	}
	return 0
}

// attachWordsToTimeSpans assigns words to the chunk whose "time" span covers the
// word's start time, mutating each chunk's span.Words in place. Words are
// processed in order; a word is placed in the first chunk for which
// StartMS <= word.start < EndMS (the final time chunk also catches a word at or
// beyond its EndMS so trailing words are not dropped). Chunks without a time
// span are skipped. The chunk text and span bounds are never modified.
func attachWordsToTimeSpans(segs []chunkSegment, words []model.TimedWord) {
	for _, w := range words {
		idx := timeSpanIndexForWord(segs, w.StartMS)
		if idx < 0 {
			continue
		}
		dur := w.EndMS - w.StartMS
		if dur < 0 {
			dur = 0
		}
		segs[idx].Span.Words = append(segs[idx].Span.Words, model.WordSpan{
			T: w.StartMS,
			D: dur,
			W: w.Word,
		})
	}
}

// segmentsHaveWordTiming reports whether any transcript segment carries a
// populated per-word timing array on its span (SPEC §8.6.9). It drives the
// transcript-level meta_json `words` granularity flag: true iff at least one
// segment has word-level timing attached, so a consumer can tell word timing is
// available without inspecting every span. It never affects chunking.
func segmentsHaveWordTiming(segs []chunkSegment) bool {
	for i := range segs {
		if len(segs[i].Span.Words) > 0 {
			return true
		}
	}
	return false
}

// timeSpanIndexForWord returns the index of the time-spanned chunk that owns a
// word starting at startMS, or -1 when no chunk does. A word inside [StartMS,
// EndMS) belongs to that chunk; a word at/after the last time chunk's EndMS is
// assigned to that last time chunk so trailing words are retained.
func timeSpanIndexForWord(segs []chunkSegment, startMS int) int {
	lastTime := -1
	for i := range segs {
		if segs[i].Span.Kind != "time" {
			continue
		}
		lastTime = i
		if startMS >= segs[i].Span.StartMS && startMS < segs[i].Span.EndMS {
			return i
		}
	}
	return lastTime
}

func chunkTranscriptByTime(content string) []chunkSegment {
	// normalize line endings just like NormalizeUTF8 does; this ensures both
	// "\r\n" and lone "\r" are converted to "\n" before we split.  we
	// also salvage any invalid UTF-8 sequences, although the transcript
	// generator normally produces valid UTF-8.
	normalized := string(normalizeUTF8([]byte(content)))
	lines := strings.Split(normalized, "\n")
	type transcriptSegment struct {
		startMS int
		text    string
	}
	segments := make([]transcriptSegment, 0, len(lines))
	var current *transcriptSegment
	// sawTimestamp records whether ANY line carried a real [mm:ss] marker. When
	// none did, every segment's startMS is a synthetic default and there are no
	// real anchors to time against, so we must not fabricate spans (issue #431
	// item d).
	sawTimestamp := false

	pushCurrent := func() {
		if current == nil {
			return
		}
		text := strings.TrimSpace(current.text)
		if text == "" {
			current = nil
			return
		}
		segments = append(segments, transcriptSegment{startMS: current.startMS, text: text})
		current = nil
	}

	for _, line := range lines {
		startMS, text, ok := parseTranscriptTimestamp(line)
		if ok {
			sawTimestamp = true
			pushCurrent()
			current = &transcriptSegment{startMS: startMS, text: strings.TrimSpace(text)}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if current == nil {
			current = &transcriptSegment{startMS: 0, text: trimmed}
		} else if current.text == "" {
			current.text = trimmed
		} else {
			current.text += "\n" + trimmed
		}
	}
	pushCurrent()

	// No timestamp markers anywhere: the transcriber returned plain text with no
	// real timing (e.g. a Gemini-style STT backend). Emit the chunks WITHOUT a
	// time span rather than fabricating a char-weighted window that would present
	// invented timestamps as real (issue #431 item d). This also covers the
	// degenerate len(segments)==0 case, which by definition had no markers.
	if !sawTimestamp {
		trimmed := strings.TrimSpace(content)
		if trimmed == "" {
			return nil
		}
		if len(segments) == 0 {
			return splitTranscriptSegmentWithoutTiming(trimmed)
		}
		out := make([]chunkSegment, 0, len(segments))
		for i := range segments {
			out = append(out, splitTranscriptSegmentWithoutTiming(segments[i].text)...)
		}
		return out
	}

	out := make([]chunkSegment, 0, len(segments))
	for i := range segments {
		endMS := segments[i].startMS + estimateTranscriptDurationMS(segments[i].text)
		if i+1 < len(segments) && segments[i+1].startMS >= segments[i].startMS {
			endMS = segments[i+1].startMS
		}
		out = append(out, splitTranscriptSegmentWithTiming(segments[i].text, segments[i].startMS, endMS)...)
	}
	return out
}

func estimateTranscriptDurationMS(text string) int {
	words := len(strings.Fields(text))
	// Rough speaking pace: ~200 words/min => 300ms per word.
	estimated := words * 300
	if estimated < 1000 {
		return 1000
	}
	return estimated
}

func splitTranscriptSegmentWithTiming(text string, startMS, endMS int) []chunkSegment {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if endMS <= startMS {
		endMS = startMS + 1
	}

	// use the exported constants so callers (including tests) can reason about
	// the underlying configuration without duplicating numbers.  The constants
	// are unrolled here rather than passing a struct to keep the original
	// implementation simple.
	parts := chunkTextByChars(text, TranscriptChunkMaxChars, TranscriptChunkOverlapChars, TranscriptChunkMinChars)
	if len(parts) == 0 {
		parts = []chunkSegment{{Text: text}}
	}

	duration := endMS - startMS
	out := make([]chunkSegment, 0, len(parts))

	// weight time slices by chunk character (rune) length rather than
	// uniform percentages. compute the total character count and fall back to
	// equal division if the result would be zero. using rune counts ensures
	// that multi-byte UTF‑8 characters are treated consistently with
	// chunkTextByChars.
	counts := make([]int, len(parts))
	totalChars := 0
	for i, part := range parts {
		cnt := utf8.RuneCountInString(part.Text)
		counts[i] = cnt
		totalChars += cnt
	}
	if totalChars == 0 {
		// nothing to measure, just do the old uniform split
		for i, part := range parts {
			partStart := startMS + (duration*i)/len(parts)
			partEnd := startMS + (duration*(i+1))/len(parts)
			if partEnd <= partStart {
				partEnd = partStart + 1
			}
			out = append(out, chunkSegment{
				Text: part.Text,
				Span: model.Span{
					Kind:    "time",
					StartMS: partStart,
					EndMS:   partEnd,
				},
			})
		}
		return out
	}

	cumChars := 0
	for i, part := range parts {
		partStart := startMS + (duration*cumChars)/totalChars
		cumChars += counts[i]
		partEnd := startMS + (duration*cumChars)/totalChars
		if partEnd <= partStart {
			partEnd = partStart + 1
		}
		out = append(out, chunkSegment{
			Text: part.Text,
			Span: model.Span{
				Kind:    "time",
				StartMS: partStart,
				EndMS:   partEnd,
			},
		})
	}
	return out
}

// splitTranscriptSegmentWithoutTiming chunks text with the same size policy as
// splitTranscriptSegmentWithTiming but leaves every chunk's Span zero-valued
// (empty Kind), so no time bounds are attached. It is used when a transcript
// carried no timestamp markers at all: timing is genuinely absent, and an
// empty-Kind span makes the chunker persist the chunk with no span row (rather
// than a fabricated one) and makes subtitle export skip it — honest omission
// over invented timing (issue #431 item d).
func splitTranscriptSegmentWithoutTiming(text string) []chunkSegment {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	parts := chunkTextByChars(text, TranscriptChunkMaxChars, TranscriptChunkOverlapChars, TranscriptChunkMinChars)
	if len(parts) == 0 {
		return []chunkSegment{{Text: text}}
	}
	out := make([]chunkSegment, 0, len(parts))
	for _, part := range parts {
		out = append(out, chunkSegment{Text: part.Text})
	}
	return out
}

func parseMMSSComponents(m []string) (minutes, seconds int, ok bool) {
	var err error
	minutes, err = strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, false
	}
	seconds, err = strconv.Atoi(m[2])
	if err != nil {
		return 0, 0, false
	}
	// minutes here is the TOTAL minutes of an [mm:ss] marker (no hour field), so
	// a transcript longer than an hour renders e.g. [75:30]; accept any
	// non-negative minute count. Only seconds are bounded to a single unit.
	if minutes < 0 || seconds < 0 || seconds > 59 {
		return 0, 0, false
	}
	return minutes, seconds, true
}

func parseHHMMSSComponents(m []string) (hours, minutes, seconds int, ok bool) {
	var err error
	hours, err = strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, 0, false
	}
	minutes, err = strconv.Atoi(m[2])
	if err != nil {
		return 0, 0, 0, false
	}
	seconds, err = strconv.Atoi(m[3])
	if err != nil {
		return 0, 0, 0, false
	}
	if hours < 0 || minutes < 0 || minutes > 59 || seconds < 0 || seconds > 59 {
		return 0, 0, 0, false
	}
	return hours, minutes, seconds, true
}

func parseTranscriptTimestamp(line string) (int, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return 0, "", false
	}

	m := transcriptTimestampBracketedRe.FindStringSubmatch(trimmed)
	if len(m) != 6 {
		m = transcriptTimestampBareRe.FindStringSubmatch(trimmed)
	}
	if len(m) != 6 {
		return 0, "", false
	}

	var hours, minutes, seconds int
	if m[3] == "" {
		// format was mm:ss
		var ok bool
		minutes, seconds, ok = parseMMSSComponents(m)
		if !ok {
			return 0, "", false
		}
	} else {
		// format was hh:mm:ss
		var ok bool
		hours, minutes, seconds, ok = parseHHMMSSComponents(m)
		if !ok {
			return 0, "", false
		}
	}

	totalMS := ((hours*3600)+(minutes*60)+seconds)*1000 + fractionToMS(m[4])
	return totalMS, strings.TrimSpace(m[5]), true
}

// fractionToMS converts the optional decimal-second fraction captured from a
// transcript marker (1-3 digits, no leading dot) into milliseconds. The fraction
// is a tenths/hundredths/thousandths value, so it is right-padded to three
// digits: "5" -> 500, "05" -> 50, "250" -> 250. An empty fraction (a bare
// whole-second "[mm:ss]" marker) yields 0, preserving backward-compatible
// parsing of markers written before sub-second precision existed.
func fractionToMS(frac string) int {
	if frac == "" {
		return 0
	}
	for len(frac) < 3 {
		frac += "0"
	}
	ms, err := strconv.Atoi(frac)
	if err != nil {
		return 0
	}
	return ms
}

func chunkCodeByLines(content string, maxLines, overlapLines int) []chunkSegment {
	if maxLines <= 0 {
		maxLines = 200
	}
	if overlapLines < 0 {
		overlapLines = 0
	}
	if overlapLines >= maxLines {
		overlapLines = maxLines - 1
	}

	if strings.TrimSpace(content) == "" {
		return nil
	}
	lines := strings.Split(content, "\n")

	step := maxLines - overlapLines
	if step <= 0 {
		step = 1
	}

	out := make([]chunkSegment, 0, (len(lines)/step)+1)
	for start := 0; start < len(lines); start += step {
		end := start + maxLines
		if end > len(lines) {
			end = len(lines)
		}
		if start >= end {
			break
		}
		text := strings.Join(lines[start:end], "\n")
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		out = appendCodeWindow(out, text, start, end)
		if end == len(lines) {
			break
		}
	}
	return out
}

// appendCodeWindow emits a line-window's text as one chunk, or—when the window
// exceeds the rune cap (e.g. a minified single-line bundle)—splits it further by
// characters so no chunk overruns the embedder input limit. Sub-segment line
// numbers are mapped back into the window's absolute [start+1, end] range.
func appendCodeWindow(out []chunkSegment, text string, start, end int) []chunkSegment {
	if utf8.RuneCountInString(text) <= codeChunkMaxChars {
		return append(out, chunkSegment{
			Text: text,
			Span: model.Span{
				Kind:      "lines",
				StartLine: start + 1,
				EndLine:   end,
			},
		})
	}
	for _, sub := range chunkTextByChars(text, codeChunkMaxChars, codeChunkOverlapChars, 1) {
		out = append(out, chunkSegment{
			Text: sub.Text,
			Span: model.Span{
				Kind:      "lines",
				StartLine: start + sub.Span.StartLine,
				EndLine:   start + sub.Span.EndLine,
			},
		})
	}
	return out
}

// ChunkCodeByLines splits code content using the same policy as ingestion.
func ChunkCodeByLines(content string, maxLines, overlapLines int) []ChunkSegment {
	raw := chunkCodeByLines(content, maxLines, overlapLines)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

func normalizeTextChunkParams(maxChars, overlapChars, minChars int) (int, int, int) {
	if maxChars <= 0 {
		maxChars = 2500
	}
	if overlapChars < 0 {
		overlapChars = 0
	}
	if overlapChars >= maxChars {
		overlapChars = maxChars - 1
	}
	if minChars <= 0 {
		minChars = 1
	}
	return maxChars, overlapChars, minChars
}

func chunkTextByChars(content string, maxChars, overlapChars, minChars int) []chunkSegment {
	maxChars, overlapChars, minChars = normalizeTextChunkParams(maxChars, overlapChars, minChars)

	runes := []rune(content)
	if len(runes) == 0 {
		return nil
	}

	step := maxChars - overlapChars
	if step <= 0 {
		step = 1
	}

	// Precompute line starts (rune offsets) for line-span mapping.
	lineStarts := []int{0}
	for i, r := range runes {
		if r == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	out := make([]chunkSegment, 0, (len(runes)/step)+1)
	for start := 0; start < len(runes); start += step {
		end := start + maxChars
		if end > len(runes) {
			end = len(runes)
		}
		if start >= end {
			break
		}

		segmentRunes := runes[start:end]
		segmentText := strings.TrimSpace(string(segmentRunes))
		if len([]rune(segmentText)) < minChars && end != len(runes) {
			continue
		}
		if segmentText == "" {
			continue
		}

		startLine := lineNumberForOffset(lineStarts, start)
		endLine := lineNumberForOffset(lineStarts, end-1)
		out = append(out, chunkSegment{
			Text: segmentText,
			Span: model.Span{
				Kind:      "lines",
				StartLine: startLine,
				EndLine:   endLine,
			},
		})
		if end == len(runes) {
			break
		}
	}
	return out
}

// ChunkTextByChars splits text content using the same policy as ingestion.
func ChunkTextByChars(content string, maxChars, overlapChars, minChars int) []ChunkSegment {
	raw := chunkTextByChars(content, maxChars, overlapChars, minChars)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ChunkRawText is an exported wrapper over chunkRawTextByDocType (which uses the
// process-level effective chunk sizes set by ConfigureChunking) so tests in the
// tests/ tree can exercise the raw-text path directly.
func ChunkRawText(docType, content string) []ChunkSegment {
	raw := chunkRawTextByDocType(docType, content)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ChunkTranscriptByTime is an exported helper wrapping chunkTranscriptByTime and
// converting the unexported segment type to the public ChunkSegment.  It is
// primarily provided so that tests can exercise the chunking logic directly.
func ChunkTranscriptByTime(content string) []ChunkSegment {
	raw := chunkTranscriptByTime(content)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ChunkTranscriptByTimeWithWords is the exported counterpart of
// chunkTranscriptByTimeWithWords, exposed for tests in the tests/ tree. With a
// nil/empty words slice it is identical to ChunkTranscriptByTime.
func ChunkTranscriptByTimeWithWords(content string, words []model.TimedWord) []ChunkSegment {
	raw := chunkTranscriptByTimeWithWords(content, words)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ChunkTranscriptByTimeFiltered is the exported counterpart of
// chunkTranscriptByTimeWithWordsFiltered (no word timing), exposed for tests in
// the tests/ tree. A nil/inactive filter is identical to ChunkTranscriptByTime.
func ChunkTranscriptByTimeFiltered(content string, filter *subtitle.WordFilter) []ChunkSegment {
	raw := chunkTranscriptByTimeWithWordsFiltered(content, nil, filter)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ChunkTranscriptByTimeWithWordsFiltered is the exported counterpart of
// chunkTranscriptByTimeWithWordsFiltered, exposed for tests in the tests/ tree.
// It applies both the word-timing attachment and the caption word filter, so the
// attached Span.Words are already stripped of filtered phrases. A nil/inactive
// filter with nil words is identical to ChunkTranscriptByTime.
func ChunkTranscriptByTimeWithWordsFiltered(content string, words []model.TimedWord, filter *subtitle.WordFilter) []ChunkSegment {
	raw := chunkTranscriptByTimeWithWordsFiltered(content, words, filter)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ApplyDropScrubToSegments applies just the drop_phrases/scrub_phrases half of
// the ingest cue cleaning, exposed for tests in the tests/ tree. It is a thin
// wrapper over ApplyCueCleaningToSegments so the drop/scrub coverage predating
// #765 keeps exercising the production path. Nil/inactive sets return the input
// unchanged.
func ApplyDropScrubToSegments(segs []ChunkSegment, drop, scrub *subtitle.DropSet) []ChunkSegment {
	return ApplyCueCleaningToSegments(segs, subtitle.CleanOptions{Drop: drop, Scrub: scrub})
}

// ApplyCueCleaningToSegments is the exported counterpart of
// applyCueCleaningToSegments, exposed for tests in the tests/ tree. It applies
// the configured media.subtitles.* cue cleaning to chunk segments exactly as the
// ingest path does after chunking (before persistence/embedding). Inactive
// options return the input unchanged.
func ApplyCueCleaningToSegments(segs []ChunkSegment, opts subtitle.CleanOptions) []ChunkSegment {
	raw := make([]chunkSegment, len(segs))
	for i, seg := range segs {
		raw[i] = chunkSegment(seg)
	}
	cleaned := applyCueCleaningToSegments(raw, opts)
	out := make([]ChunkSegment, 0, len(cleaned))
	for _, seg := range cleaned {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ShiftTranscriptSpans is the exported counterpart of shiftTranscriptSpans,
// exposed for tests in the tests/ tree. It subtracts offsetMS from every "time"
// span's bounds and attached word timestamps (clamped at 0) and returns the
// shifted segments; the input slice is not modified.
func ShiftTranscriptSpans(segs []ChunkSegment, offsetMS int) []ChunkSegment {
	raw := make([]chunkSegment, len(segs))
	for i, seg := range segs {
		raw[i] = chunkSegment(seg)
		if seg.Span.Words != nil {
			raw[i].Span.Words = append([]model.WordSpan(nil), seg.Span.Words...)
		}
	}
	shiftTranscriptSpans(raw, offsetMS)
	out := make([]ChunkSegment, len(raw))
	for i, seg := range raw {
		out[i] = ChunkSegment(seg)
	}
	return out
}

// chunkSubtitleCues turns parsed subtitle cues into time-spanned transcript
// chunks. Unlike chunkTranscriptByTime (which estimates timing from a flat text
// transcript), the cues already carry authoritative [StartMS, EndMS] windows, so
// timing is taken verbatim — the source of the deterministic, stable citations a
// sidecar transcript provides. Adjacent cues are merged greedily until the
// accumulated text would exceed TranscriptChunkMaxChars; the merged chunk's span
// is [first cue start, last merged cue end]. Cues are processed in their given
// order (callers sort by start time first), so the output is deterministic.
func chunkSubtitleCues(cues []subtitle.Cue) []chunkSegment {
	return chunkSubtitleCuesFiltered(cues, nil)
}

// chunkSubtitleCuesFiltered is chunkSubtitleCues with an optional caption word
// filter (config media.filter_words) applied to each cue's text before merging.
// A cue empty after filtering is dropped (it never contributes to a chunk), so
// it neither adds text nor extends a merged chunk's time span. A nil/inactive
// filter is a no-op, leaving the output identical to the unfiltered path.
//
// When cues carry WebVTT <v Name> speaker attribution (SPEC §8.6.8) the merge is
// additionally split on speaker boundary so every produced chunk has a single
// speaker, and the chunk's "time" span carries a stable per-transcript speaker
// id (S1, S2, …) plus the human-readable label. Speaker ids are assigned in
// first-appearance order over the (caller-sorted, by start time) cues, so they
// are deterministic and stable across re-indexing. Speaker attribution is
// metadata only: it never changes chunk text or span bounds, and a transcript
// with no <v> tags produces byte-identical output to before.
func chunkSubtitleCuesFiltered(cues []subtitle.Cue, filter *subtitle.WordFilter) []chunkSegment {
	out := make([]chunkSegment, 0, len(cues))
	ids := newSpeakerIDAssigner()
	var (
		buf          []string
		startMS      int
		endMS        int
		haveOpen     bool
		bufLen       int
		speaker      string // stable id (S1…) of the open chunk; "" when undiarized
		speakerLabel string // human-readable name of the open chunk
	)
	flush := func() {
		if !haveOpen {
			return
		}
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		if text != "" {
			span := model.Span{Kind: "time", StartMS: startMS, EndMS: endMS, Speaker: speaker, SpeakerLabel: speakerLabel}
			if span.EndMS <= span.StartMS {
				span.EndMS = span.StartMS + 1
			}
			out = append(out, chunkSegment{Text: text, Span: span})
		}
		buf = buf[:0]
		bufLen = 0
		haveOpen = false
		speaker = ""
		speakerLabel = ""
	}

	for _, cue := range cues {
		text := strings.TrimSpace(cue.Text)
		if filter.Active() {
			text = strings.TrimSpace(filter.Apply(text))
		}
		if text == "" {
			continue
		}
		cueID, cueLabel := ids.assign(cue.Speaker)
		cueLen := utf8.RuneCountInString(text)
		// flush() joins buffered cue texts with "\n", so budget one rune for the
		// separator that will precede this cue when the buffer is non-empty;
		// otherwise a merged chunk can exceed TranscriptChunkMaxChars by the
		// number of joins. sepLen is 0 when no chunk is open (the first cue needs
		// no separator) and resets to 0 after a flush.
		sepLen := 0
		if haveOpen {
			sepLen = 1
		}
		// Start a new chunk when the buffer (plus the join separator) would
		// overflow the transcript chunk size OR when the speaker changes (so a
		// chunk never mixes two speakers); a single oversized cue still becomes
		// its own chunk.
		if haveOpen && (bufLen+sepLen+cueLen > TranscriptChunkMaxChars || cueID != speaker) {
			flush()
			sepLen = 0
		}
		if !haveOpen {
			startMS = cue.StartMS
			haveOpen = true
			speaker = cueID
			speakerLabel = cueLabel
		}
		buf = append(buf, text)
		bufLen += sepLen + cueLen
		endMS = cue.EndMS
	}
	flush()
	return out
}

// speakerIDAssigner maps a human-readable WebVTT voice name to a stable
// per-transcript speaker id (S1, S2, …) in first-appearance order (SPEC §8.6.8).
// Because the cues it is fed are sorted by start time, the mapping is
// deterministic and reproducible across re-indexing of the same transcript.
type speakerIDAssigner struct {
	byName map[string]string
	next   int
}

func newSpeakerIDAssigner() *speakerIDAssigner {
	return &speakerIDAssigner{byName: map[string]string{}}
}

// assign returns the stable id and the human-readable label for a cue's voice
// name. An empty name yields ("", "") so an undiarized cue carries no
// attribution. The first time a name is seen it is allocated the next S-id; the
// same name always maps to the same id thereafter.
func (a *speakerIDAssigner) assign(name string) (id, label string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ""
	}
	if existing, ok := a.byName[name]; ok {
		return existing, name
	}
	a.next++
	id = fmt.Sprintf("S%d", a.next)
	a.byName[name] = id
	return id, name
}

// ChunkSubtitleCues exposes chunkSubtitleCues for tests, converting the
// unexported segment type to the public ChunkSegment.
func ChunkSubtitleCues(cues []subtitle.Cue) []ChunkSegment {
	raw := chunkSubtitleCues(cues)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

// ChunkSubtitleCuesFiltered exposes chunkSubtitleCuesFiltered for tests. A
// nil/inactive filter is identical to ChunkSubtitleCues.
func ChunkSubtitleCuesFiltered(cues []subtitle.Cue, filter *subtitle.WordFilter) []ChunkSegment {
	raw := chunkSubtitleCuesFiltered(cues, filter)
	out := make([]ChunkSegment, 0, len(raw))
	for _, seg := range raw {
		out = append(out, ChunkSegment(seg))
	}
	return out
}

func lineNumberForOffset(lineStarts []int, offset int) int {
	// Keep original edge-case behavior
	if offset <= 0 {
		return 1
	}
	// Locate first index where lineStarts[i] > offset using binary search.
	// The desired line number is the index of the greatest entry <= offset,
	// which corresponds to the returned index from Search (first > offset).
	idx := sort.Search(len(lineStarts), func(i int) bool {
		return lineStarts[i] > offset
	})
	if idx == 0 {
		// offset is less than or equal to the first entry; return first line
		return 1
	}
	// idx is the first index with a start greater than offset; the line is idx
	return idx
}
