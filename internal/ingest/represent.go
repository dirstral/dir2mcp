package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/ingest/docling"
	"github.com/dirstral/dir2mcp/internal/model"
)

// transcriptTimestampBracketedRe matches leading timestamps in [mm:ss] or
// [hh:mm:ss] form.
var transcriptTimestampBracketedRe = regexp.MustCompile(`^\s*\[(\d{1,2}):(\d{2})(?::(\d{2}))?\]\s*(.*)$`)

// transcriptTimestampBareRe matches leading timestamps in mm:ss or hh:mm:ss
// form without brackets.
var transcriptTimestampBareRe = regexp.MustCompile(`^\s*(\d{1,2}):(\d{2})(?::(\d{2}))?(?:\s+|$)(.*)$`)

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
)

// RepresentationGenerator handles creation of representations from documents
type RepresentationGenerator struct {
	store model.RepresentationStore
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
func (rg *RepresentationGenerator) GenerateRawText(ctx context.Context, doc model.Document, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat file %s: %w", doc.RelPath, err)
	}
	if info.Size() > defaultMaxFileSizeBytes {
		return fmt.Errorf("file %s too large (%d bytes); limit %d", doc.RelPath, info.Size(), defaultMaxFileSizeBytes)
	}

	// Read file content first so we can delegate to the new helper which
	// accepts pre-loaded bytes.  This keeps the original behaviour intact
	// while allowing callers that already have the content to avoid the I/O.
	content, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("read file %s: %w", doc.RelPath, err)
	}
	return rg.GenerateRawTextFromContent(ctx, doc, content)
}

// GenerateRawTextFromContent behaves like GenerateRawText but takes the
// document bytes as an argument.  This is useful when the caller already
// loaded the file (e.g. during a scan) and wants to avoid re-reading it.
// The absolute path is no longer required; callers that previously had it
// simply read the file to supply the content.  Removing the parameter
// simplifies the API and avoids unused variable warnings.
func (rg *RepresentationGenerator) GenerateRawTextFromContent(ctx context.Context, doc model.Document, content []byte) error {
	// Guard against huge files to avoid OOM.  We mirror the same limit used by
	// discovery since raw-text ingestion should follow the same policy.
	if int64(len(content)) > defaultMaxFileSizeBytes {
		return fmt.Errorf("file %s too large (%d bytes); limit %d", doc.RelPath, len(content), defaultMaxFileSizeBytes)
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
		if err := rg.upsertChunksForRepresentationWithStore(ctx, tx, repID, indexKindForDocType(doc.DocType), segments); err != nil {
			return err
		}
		return nil
	})
}

// GenerateMediaChunk emits a single media chunk for a whole-file media
// document (currently images) so it is embedded directly from its bytes via
// the multimodal embedder (SPEC 8.1.7), rather than via extracted text. The
// chunk carries no text; the embedding worker reads its bytes from MediaRef
// (the corpus rel_path). Provenance is a page-1 span. contentHash dedups the
// representation across unchanged re-ingests.
func (rg *RepresentationGenerator) GenerateMediaChunk(ctx context.Context, doc model.Document, contentHash string) error {
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
		chunk := model.Chunk{
			RepID:           repID,
			Ordinal:         0,
			Text:            "",
			IndexKind:       "text", // media shares the text vector space
			EmbeddingStatus: "pending",
			Modality:        doc.DocType, // "image" today; audio/video/pdf later
			MediaRef:        doc.RelPath,
		}
		if _, err := tx.InsertChunkWithSpans(ctx, chunk, []model.Span{{Kind: "page", Page: 1}}); err != nil {
			return fmt.Errorf("insert media chunk: %w", err)
		}
		if err := tx.SoftDeleteChunksFromOrdinal(ctx, repID, 1); err != nil {
			return fmt.Errorf("soft delete stale media chunks: %w", err)
		}
		return nil
	})
}

func (rg *RepresentationGenerator) upsertChunksForRepresentation(ctx context.Context, repID int64, indexKind string, segments []chunkSegment) error {
	// wrap the entire operation in a transaction so we don't end up with a
	// partial set of chunks if an insertion fails halfway through.  The store
	// implementation handles beginning/committing/rolling back the tx.
	return rg.store.WithTx(ctx, func(tx model.RepresentationStore) error {
		return rg.upsertChunksForRepresentationWithStore(ctx, tx, repID, indexKind, segments)
	})
}

func (rg *RepresentationGenerator) upsertChunksForRepresentationWithStore(ctx context.Context, st model.RepresentationStore, repID int64, indexKind string, segments []chunkSegment) error {
	for i, seg := range segments {
		chunk := model.Chunk{
			RepID:           repID,
			Ordinal:         i,
			Text:            seg.Text,
			TextHash:        computeRepHash([]byte(seg.Text)),
			IndexKind:       indexKind,
			EmbeddingStatus: "pending",
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
	// Salvage any invalid UTF-8 by replacing with U+FFFD.
	if !utf8.Valid(content) {
		out := strings.ToValidUTF8(string(content), "\uFFFD")
		content = []byte(out)
	}

	// Normalize line endings: convert \r\n and \r to \n
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	content = bytes.ReplaceAll(content, []byte("\r"), []byte("\n"))

	return content
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
	return chunkTextByChars(content, 2500, 250, 200)
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
// comparable granularity.
const (
	structuredChunkMaxChars     = 2500
	structuredChunkOverlapChars = 250
	structuredChunkMinChars     = 200
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
	bufSection   []string
}

func newStructuredChunker() *structuredChunker {
	maxChars, overlapChars, minChars := normalizeTextChunkParams(
		structuredChunkMaxChars, structuredChunkOverlapChars, structuredChunkMinChars)
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
	}
	c.bufText.WriteString(b.Text)
	c.buf = append(c.buf, b)
	if c.bufText.Len() >= c.maxChars {
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

	if len(segments) == 0 {
		trimmed := strings.TrimSpace(content)
		if trimmed == "" {
			return nil
		}
		return splitTranscriptSegmentWithTiming(trimmed, 0, estimateTranscriptDurationMS(trimmed))
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
	if minutes < 0 || minutes > 59 || seconds < 0 || seconds > 59 {
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
	if len(m) != 5 {
		m = transcriptTimestampBareRe.FindStringSubmatch(trimmed)
	}
	if len(m) != 5 {
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

	totalMS := ((hours * 3600) + (minutes * 60) + seconds) * 1000
	return totalMS, strings.TrimSpace(m[4]), true
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
		out = append(out, chunkSegment{
			Text: text,
			Span: model.Span{
				Kind:      "lines",
				StartLine: start + 1,
				EndLine:   end,
			},
		})
		if end == len(lines) {
			break
		}
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
