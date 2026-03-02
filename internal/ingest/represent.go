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

	"dir2mcp/internal/model"
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
	// RepTypeOCRMarkdown is the representation type for OCR-generated markdown
	RepTypeOCRMarkdown = "ocr_markdown"
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
		if _, err := st.InsertChunkWithSpans(ctx, chunk, []model.Span{seg.Span}); err != nil {
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

	hours := 0
	minutes := 0
	seconds := 0
	var err error
	if m[3] == "" {
		// format was mm:ss
		minutes, err = strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		seconds, err = strconv.Atoi(m[2])
		if err != nil {
			return 0, "", false
		}
		if minutes < 0 || minutes > 59 || seconds < 0 || seconds > 59 {
			return 0, "", false
		}
	} else {
		// format was hh:mm:ss
		hours, err = strconv.Atoi(m[1])
		if err != nil {
			return 0, "", false
		}
		minutes, err = strconv.Atoi(m[2])
		if err != nil {
			return 0, "", false
		}
		seconds, err = strconv.Atoi(m[3])
		if err != nil {
			return 0, "", false
		}
		if hours < 0 || minutes < 0 || minutes > 59 || seconds < 0 || seconds > 59 {
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

func chunkTextByChars(content string, maxChars, overlapChars, minChars int) []chunkSegment {
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
