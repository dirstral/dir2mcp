package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
)

func TestNewRepresentationGeneratorNil(t *testing.T) {
	// ensure constructor fails early when given a nil store
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic for nil store")
		} else if !strings.Contains(fmt.Sprint(r), "nil model.RepresentationStore") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	_ = ingest.NewRepresentationGenerator(nil)
}

func TestNormalizeUTF8(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected []byte
	}{
		{
			name:     "already valid UTF-8 with LF",
			input:    []byte("hello\nworld"),
			expected: []byte("hello\nworld"),
		},
		{
			name:     "CRLF to LF",
			input:    []byte("hello\r\nworld"),
			expected: []byte("hello\nworld"),
		},
		{
			name:     "CR to LF",
			input:    []byte("hello\rworld"),
			expected: []byte("hello\nworld"),
		},
		{
			name:     "mixed line endings",
			input:    []byte("line1\r\nline2\rline3\nline4"),
			expected: []byte("line1\nline2\nline3\nline4"),
		},
		{
			name:     "empty content",
			input:    []byte{},
			expected: []byte{},
		},
		{
			name:     "valid UTF-8 with special chars",
			input:    []byte("Hello 世界 🌍"),
			expected: []byte("Hello 世界 🌍"),
		},
		{
			name:  "invalid UTF-8 salvaged to U+FFFD",
			input: []byte{0x81}, // undefined in Windows-1252, no BOM, not UTF-16
			// A lone 0x81 is invalid UTF-8 and maps to no Windows-1252 codepoint,
			// so it decodes to a single U+FFFD (0xEF,0xBF,0xBD).
			expected: []byte{0xEF, 0xBF, 0xBD},
		},
		{
			name: "UTF-16LE BOM then a lone padding byte",
			// 0xFF 0xFE is the UTF-16LE BOM; the trailing 0x00 is an incomplete
			// (odd) code unit and decodes to a single U+FFFD.
			input:    []byte{0xFF, 0xFE, 0x00},
			expected: []byte{0xEF, 0xBF, 0xBD},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ingest.NormalizeUTF8(tt.input)
			if !bytes.Equal(result, tt.expected) {
				t.Errorf("ingest.NormalizeUTF8() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestShouldGenerateRawText(t *testing.T) {
	tests := []struct {
		name     string
		docType  string
		expected bool
	}{
		// Should generate raw_text
		{"code", "code", true},
		{"text", "text", true},
		{"markdown", "md", true},
		{"data", "data", true},
		{"html", "html", true},

		// Should NOT generate raw_text
		{"pdf", "pdf", false},
		{"image", "image", false},
		{"audio", "audio", false},
		{"document", "document", false},
		{"archive", "archive", false},
		{"binary_ignored", "binary_ignored", false},
		{"unknown", "unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ingest.ShouldGenerateRawText(tt.docType)
			if result != tt.expected {
				t.Errorf("ingest.ShouldGenerateRawText(%q) = %v, want %v", tt.docType, result, tt.expected)
			}
		})
	}
}

func TestShouldGenerateExtractedMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		docType  string
		expected bool
	}{
		{"pdf", "pdf", true},
		{"image", "image", true},
		{"document", "document", true},
		{"text", "text", false},
		{"code", "code", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ingest.ShouldGenerateExtractedMarkdown(tt.docType)
			if got != tt.expected {
				t.Fatalf("ShouldGenerateExtractedMarkdown(%q)=%v want=%v", tt.docType, got, tt.expected)
			}
		})
	}
}

func TestRepTypeConstants(t *testing.T) {
	// Verify constants are defined with expected values per SPEC
	tests := []struct {
		name     string
		constant string
		expected string
	}{
		{"raw_text", ingest.RepTypeRawText, "raw_text"},
		{"extracted_markdown", ingest.RepTypeExtractedMarkdown, "extracted_markdown"},
		{"transcript", ingest.RepTypeTranscript, "transcript"},
		{"annotation_json", ingest.RepTypeAnnotationJSON, "annotation_json"},
		{"annotation_text", ingest.RepTypeAnnotationText, "annotation_text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.expected {
				t.Errorf("Constant %s = %q, want %q", tt.name, tt.constant, tt.expected)
			}
		})
	}
}

// Example integration test structure (implementation would be in a separate file)
func TestRepresentationGeneratorIntegration(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := ingest.NewRepresentationGenerator(st)
	doc := model.Document{
		DocID:   1,
		RelPath: "main.go",
		DocType: "code",
	}

	tmp := filepath.Join(t.TempDir(), "main.go")
	content := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	if err := rg.GenerateRawText(context.Background(), doc, tmp); err != nil {
		t.Fatalf("GenerateRawText failed: %v", err)
	}
	if st.upsertCount != 1 {
		t.Fatalf("expected 1 representation upsert, got %d", st.upsertCount)
	}
	if len(st.chunks) == 0 {
		t.Fatalf("expected chunks to be inserted")
	}
	if st.softDeleteCall == 0 {
		t.Fatalf("expected stale-chunk cleanup call")
	}
}

func TestGenerateRawTextFromContentPrefersGivenBytes(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := ingest.NewRepresentationGenerator(st)
	doc := model.Document{DocID: 1, RelPath: "foo.txt", DocType: "text"}

	provided := []byte("provided content")
	if err := rg.GenerateRawTextFromContent(context.Background(), doc, provided); err != nil {
		t.Fatalf("GenerateRawTextFromContent failed: %v", err)
	}
	if st.upsertCount != 1 {
		t.Fatalf("expected 1 representation upsert, got %d", st.upsertCount)
	}
	// compute hash of provided content to ensure it was used
	hash := ingest.ComputeRepHash(ingest.NormalizeUTF8(provided))
	if len(st.reps) == 0 {
		t.Fatalf("no representation recorded, expected at least one")
	}
	if st.reps[0].RepHash != hash {
		t.Fatalf("representation hash %q does not match provided content hash %q", st.reps[0].RepHash, hash)
	}
}

func TestGenerateRawText_DetectsLanguageWhenEnabled(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := ingest.NewRepresentationGenerator(st)
	rg.SetLanguageDetection(true)
	doc := model.Document{DocID: 1, RelPath: "notes.txt", DocType: "text"}
	content := []byte("The annual report describes how the committee carefully reviewed every application and then approved the budget for the coming financial year across all of the regional offices.")
	if err := rg.GenerateRawTextFromContent(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateRawTextFromContent failed: %v", err)
	}
	if len(st.reps) == 0 {
		t.Fatal("no representation recorded")
	}
	var meta struct {
		Language       string  `json:"language"`
		LanguageSource string  `json:"language_source"`
		Confidence     float64 `json:"language_confidence"`
	}
	if err := json.Unmarshal([]byte(st.reps[0].MetaJSON), &meta); err != nil {
		t.Fatalf("meta_json %q is not valid JSON: %v", st.reps[0].MetaJSON, err)
	}
	if meta.Language != "en" {
		t.Errorf("detected language = %q, want en (meta=%q)", meta.Language, st.reps[0].MetaJSON)
	}
	if meta.LanguageSource != "detected" {
		t.Errorf("language_source = %q, want detected", meta.LanguageSource)
	}
	if meta.Confidence <= 0 {
		t.Errorf("language_confidence = %v, want > 0", meta.Confidence)
	}
}

func TestGenerateRawText_NoLanguageWhenDetectionDisabled(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := ingest.NewRepresentationGenerator(st) // detection off by default
	doc := model.Document{DocID: 1, RelPath: "notes.txt", DocType: "text"}
	content := []byte("The annual report describes how the committee reviewed every application and approved the budget for the coming financial year.")
	if err := rg.GenerateRawTextFromContent(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateRawTextFromContent failed: %v", err)
	}
	if len(st.reps) == 0 {
		t.Fatal("no representation recorded")
	}
	if strings.TrimSpace(st.reps[0].MetaJSON) != "" {
		t.Errorf("detection disabled should record no meta_json, got %q", st.reps[0].MetaJSON)
	}
}

func TestGenerateRawTextFromContent_UnicodeDashesProduceChunks(t *testing.T) {
	st := &fakeRepStore{failAfter: -1}
	rg := ingest.NewRepresentationGenerator(st)
	doc := model.Document{DocID: 1, RelPath: "flow.md", DocType: "md"}

	content := []byte("x402 – Payment Flow\navoid hard‑coding secrets.\n")
	if err := rg.GenerateRawTextFromContent(context.Background(), doc, content); err != nil {
		t.Fatalf("GenerateRawTextFromContent failed: %v", err)
	}
	if len(st.reps) == 0 {
		t.Fatal("expected at least one representation")
	}
	if len(st.chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	if !strings.Contains(st.chunks[0].Text, "–") || !strings.Contains(st.chunks[0].Text, "‑") {
		t.Fatalf("expected unicode dashes to be preserved, got %q", st.chunks[0].Text)
	}
}

func TestGenerateRawTextTooLarge(t *testing.T) {
	// use failAfter=-1 to ensure the fake store does not inject failures for
	// this test, which only verifies oversized-file rejection before any
	// chunks are inserted.
	st := &fakeRepStore{failAfter: -1}
	rg := ingest.NewRepresentationGenerator(st)
	doc := model.Document{DocID: 1, RelPath: "large.txt", DocType: "text"}

	// create a file just above the defaultMaxFileSizeBytes limit
	tmp := filepath.Join(t.TempDir(), "large.txt")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if err := f.Truncate(ingest.DefaultMaxFileSizeBytes() + 1); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			t.Fatalf("close temp file after truncate failure: %v", closeErr)
		}
		t.Fatalf("truncate file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	err = rg.GenerateRawText(context.Background(), doc, tmp)
	if err == nil {
		t.Fatalf("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestChunkCodeByLines(t *testing.T) {
	content := strings.Repeat("line\n", 260)
	chunks := ingest.ChunkCodeByLines(content, 200, 30)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	if chunks[0].Span.Kind != "lines" {
		t.Fatalf("expected lines span kind, got %q", chunks[0].Span.Kind)
	}
	if chunks[0].Span.StartLine != 1 || chunks[0].Span.EndLine != 200 {
		t.Fatalf("unexpected first chunk span: %+v", chunks[0].Span)
	}
	if chunks[1].Span.StartLine != 171 {
		t.Fatalf("expected overlap start line 171, got %d", chunks[1].Span.StartLine)
	}
}

// TestChunkCodeByLines_MinifiedSingleLineSplitByChars pins that a minified
// single-long-line bundle (no newlines) is bounded by characters rather than
// emitted as one giant chunk that would exceed the embedder input limit.
func TestChunkCodeByLines_MinifiedSingleLineSplitByChars(t *testing.T) {
	const maxCodeChars = 2500
	// One physical line, ~6000 runes, well over the per-chunk char cap.
	content := strings.Repeat("a", 6000)
	chunks := ingest.ChunkCodeByLines(content, 200, 30)
	if len(chunks) < 2 {
		t.Fatalf("expected the oversize single line to split into multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Span.Kind != "lines" {
			t.Errorf("chunk %d span kind = %q, want lines", i, c.Span.Kind)
		}
		if c.Span.StartLine != 1 || c.Span.EndLine != 1 {
			t.Errorf("chunk %d span = %+v, want single-line [1,1]", i, c.Span)
		}
		if n := utf8.RuneCountInString(c.Text); n > maxCodeChars {
			t.Errorf("chunk %d has %d runes, exceeds cap %d", i, n, maxCodeChars)
		}
	}
}

// TestChunkCodeByLines_LongLineWithinWindowSplit pins that an oversize line
// inside a multi-line window is still split by characters, with every chunk
// staying under the cap.
func TestChunkCodeByLines_LongLineWithinWindowSplit(t *testing.T) {
	const maxCodeChars = 2500
	content := "short header\n" + strings.Repeat("b", 8000) + "\nshort footer"
	chunks := ingest.ChunkCodeByLines(content, 200, 30)
	if len(chunks) < 2 {
		t.Fatalf("expected oversize window to split, got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c.Text); n > maxCodeChars {
			t.Errorf("chunk %d has %d runes, exceeds cap %d", i, n, maxCodeChars)
		}
	}
}

func TestChunkTranscriptByTimeLineEndingNormalization(t *testing.T) {
	// Ensure both CRLF and lone CR are treated as line breaks when chunking.
	input := "[00:00] first\r[00:05] second\r\n[00:10] third"
	chunks := ingest.ChunkTranscriptByTime(input)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if chunks[0].Text != "first" || chunks[1].Text != "second" || chunks[2].Text != "third" {
		t.Errorf("unexpected chunk texts: %+v", chunks)
	}
}

func TestChunkTranscriptByTimeBracketMatching(t *testing.T) {
	// Malformed timestamps should not be recognized; we expect a single
	// fallback chunk containing the original text.
	cases := []struct {
		input    string
		wantText string
	}{
		{"[00:00 missing", "[00:00 missing"},
		{"00:00] stray", "00:00] stray"},
	}
	for _, tt := range cases {
		chunks := ingest.ChunkTranscriptByTime(tt.input)
		if len(chunks) != 1 {
			t.Fatalf("expected 1 chunk for %q, got %d", tt.input, len(chunks))
		}
		if chunks[0].Text != tt.wantText {
			t.Errorf("unexpected text for %q: %q", tt.input, chunks[0].Text)
		}
	}

	// A well-formed timestamp remains recognized so we know the regex still
	// works when brackets are properly paired.
	valid := "[01:02:03] good"
	chunks := ingest.ChunkTranscriptByTime(valid)
	if len(chunks) != 1 || chunks[0].Text != "good" {
		t.Errorf("valid timestamp not parsed correctly: %+v", chunks)
	}

	// Unbracketed timestamps should still be recognized.
	bare := "00:07 bare format works"
	chunks = ingest.ChunkTranscriptByTime(bare)
	if len(chunks) != 1 || chunks[0].Text != "bare format works" {
		t.Errorf("bare timestamp not parsed correctly: %+v", chunks)
	}
}

func TestChunkTranscriptByTime_SplitsLongTimestampWindow(t *testing.T) {
	longText := strings.Repeat("alpha beta gamma delta ", 600)
	input := "[00:00] " + longText + "\n[01:00] tail"
	chunks := ingest.ChunkTranscriptByTime(input)
	if len(chunks) < 2 {
		t.Fatalf("expected long transcript to be split, got %d chunk(s)", len(chunks))
	}
	if chunks[0].Span.Kind != "time" {
		t.Fatalf("expected time span, got %+v", chunks[0].Span)
	}
	if chunks[0].Span.StartMS != 0 {
		t.Fatalf("expected first chunk at 0ms, got %+v", chunks[0].Span)
	}
	if chunks[0].Span.EndMS <= chunks[0].Span.StartMS {
		t.Fatalf("expected positive duration span, got %+v", chunks[0].Span)
	}
	// ensure we respect the same chunking parameters defined in the ingest
	// package; previously a hardcoded 1300 was used which could drift if the
	// implementation changed.  The limit here corresponds to
	// ingest.TranscriptChunkMaxChars (see splitTranscriptSegmentWithTiming).
	if len([]rune(chunks[0].Text)) > ingest.TranscriptChunkMaxChars {
		t.Fatalf("expected first split chunk to be bounded, got %d runes (limit %d)",
			len([]rune(chunks[0].Text)), ingest.TranscriptChunkMaxChars)
	}
}

// TestChunkTranscriptByTime_NoTimestampsOmitsTiming asserts that a transcript
// with no [mm:ss] markers (e.g. a Gemini-style STT backend that returns plain
// text without timing) is still chunked into text, but the chunks carry NO time
// span rather than a fabricated char-weighted window. Presenting invented
// timestamps as real is the bug fixed by issue #431 item d: honest omission
// beats fabrication.
func TestChunkTranscriptByTime_NoTimestampsOmitsTiming(t *testing.T) {
	input := strings.Repeat("plain transcript text without timestamps ", 300)
	chunks := ingest.ChunkTranscriptByTime(input)
	if len(chunks) == 0 {
		t.Fatal("expected text chunks even without timing")
	}
	for i, ch := range chunks {
		if strings.TrimSpace(ch.Text) == "" {
			t.Fatalf("chunk %d has empty text", i)
		}
		if ch.Span.Kind != "" {
			t.Fatalf("chunk %d expected no span kind (timing absent), got %+v", i, ch.Span)
		}
		if ch.Span.StartMS != 0 || ch.Span.EndMS != 0 {
			t.Fatalf("chunk %d expected zero (unset) time bounds, got %+v", i, ch.Span)
		}
	}
}

// TestChunkTranscriptByTime_SubSecondSegmentsDoNotCollapse asserts that two
// segments starting within the same whole second keep distinct, ordered start
// times and non-collapsed spans once the marker carries millisecond precision
// (issue #431 item c). Before the fix both markers floored to [00:05] and the
// first span collapsed to a 1 ms sliver, mis-routing its words to the next span.
func TestChunkTranscriptByTime_SubSecondSegmentsDoNotCollapse(t *testing.T) {
	input := "[00:05.250] first segment here\n[00:05.750] second segment here\n[00:06.000] third segment here"
	chunks := ingest.ChunkTranscriptByTime(input)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d: %+v", len(chunks), chunks)
	}
	wantStarts := []int{5250, 5750, 6000}
	for i, ch := range chunks {
		if ch.Span.Kind != "time" {
			t.Fatalf("chunk %d expected time span, got %+v", i, ch.Span)
		}
		if ch.Span.StartMS != wantStarts[i] {
			t.Fatalf("chunk %d StartMS = %d, want %d", i, ch.Span.StartMS, wantStarts[i])
		}
		if ch.Span.EndMS <= ch.Span.StartMS {
			t.Fatalf("chunk %d span collapsed: %+v", i, ch.Span)
		}
	}
	// The first two must not share a start (the same-second collapse bug).
	if chunks[0].Span.StartMS == chunks[1].Span.StartMS {
		t.Fatalf("sub-second segments collapsed to the same start: %d", chunks[0].Span.StartMS)
	}
	// The first segment's real end must reach the second's start (no 1 ms sliver).
	if chunks[0].Span.EndMS != chunks[1].Span.StartMS {
		t.Fatalf("chunk 0 EndMS = %d, want %d (adjacent to chunk 1 start)", chunks[0].Span.EndMS, chunks[1].Span.StartMS)
	}
}

// TestFormatTranscriptTimestamp covers the shared marker formatter every STT
// backend uses: whole seconds render as [mm:ss] (backward compatible) and
// sub-second offsets render as [mm:ss.mmm], and both round-trip through the
// chunker's parser to the original millisecond value (issue #431 item c).
func TestFormatTranscriptTimestamp(t *testing.T) {
	cases := []struct {
		ms   int
		want string
	}{
		{0, "[00:00]"},
		{5000, "[00:05]"},
		{65000, "[01:05]"},
		{5250, "[00:05.250]"},
		{5050, "[00:05.050]"},
		{5005, "[00:05.005]"},
		{5500, "[00:05.500]"},
		// >1h transcripts (interviews): total minutes, no hour wrapping, must
		// still round-trip (regression for the >59-minute parse rejection).
		{3600000, "[60:00]"},
		{3630500, "[60:30.500]"},
		{7200000, "[120:00]"},
	}
	for _, tc := range cases {
		got := model.FormatTranscriptTimestamp(tc.ms)
		if got != tc.want {
			t.Fatalf("FormatTranscriptTimestamp(%d) = %q, want %q", tc.ms, got, tc.want)
		}
		// Round-trip: the marker must chunk to a time span starting at tc.ms.
		chunks := ingest.ChunkTranscriptByTime(got + " word one two three")
		if len(chunks) == 0 {
			t.Fatalf("no chunks for marker %q", got)
		}
		if chunks[0].Span.Kind != "time" || chunks[0].Span.StartMS != tc.ms {
			t.Fatalf("marker %q parsed to %+v, want time span StartMS=%d", got, chunks[0].Span, tc.ms)
		}
	}
}

// TestChunkTranscriptByTime_MultiLineNoTimestampsOmitsTiming asserts the
// honest-omission path holds even when a no-timing transcript spans several
// lines: every chunk is emitted with no span rather than an invented window
// (issue #431 item d).
func TestChunkTranscriptByTime_MultiLineNoTimestampsOmitsTiming(t *testing.T) {
	input := "first line of speech\nsecond line of speech\nthird line of speech"
	chunks := ingest.ChunkTranscriptByTime(input)
	if len(chunks) == 0 {
		t.Fatal("expected text chunks even without timing")
	}
	for i, ch := range chunks {
		if ch.Span.Kind != "" {
			t.Fatalf("chunk %d expected no span kind (timing absent), got %+v", i, ch.Span)
		}
	}
}

func TestChunkTextByChars(t *testing.T) {
	content := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 200)
	chunks := ingest.ChunkTextByChars(content, 250, 25, 50)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len([]rune(c.Text)) > 250 {
			t.Fatalf("chunk %d exceeds max chars: %d", i, len([]rune(c.Text)))
		}
		if c.Span.Kind != "lines" {
			t.Fatalf("chunk %d has unexpected span kind %q", i, c.Span.Kind)
		}
	}
}

type fakeRepStore struct {
	upsertCount int
	nextRepID   int64
	reps        []model.Representation
	chunks      []model.Chunk
	// store all spans that have been passed in so tests can inspect them
	spans []model.Span
	// when non-zero, InsertChunkWithSpans will enforce this span count
	expectedSpanCount int
	softDeleteCall    int
	// failAfter simulates a failure after inserting this many chunks (0-based)
	// negative means never fail.
	failAfter int
}

func (s *fakeRepStore) UpsertRepresentation(_ context.Context, rep model.Representation) (int64, error) {
	s.upsertCount++
	if s.nextRepID == 0 {
		s.nextRepID = 1
	}
	if rep.DocID <= 0 {
		return 0, fmt.Errorf("invalid doc id")
	}
	// record rep for later inspection
	s.reps = append(s.reps, rep)
	currentID := s.nextRepID
	s.nextRepID++
	return currentID, nil
}

func (s *fakeRepStore) InsertChunkWithSpans(_ context.Context, chunk model.Chunk, spans []model.Span) (int64, error) {
	if chunk.RepID <= 0 {
		return 0, fmt.Errorf("invalid rep id")
	}
	// simulate failure injection before doing any work
	if s.failAfter >= 0 && len(s.chunks) == s.failAfter {
		return 0, fmt.Errorf("injected failure at chunk %d", s.failAfter)
	}
	// if an expected span count has been configured, enforce it
	if s.expectedSpanCount != 0 && len(spans) != s.expectedSpanCount {
		return 0, fmt.Errorf("expected %d span(s), got %d", s.expectedSpanCount, len(spans))
	}
	// require at least one span
	if len(spans) < 1 {
		return 0, fmt.Errorf("expected at least one span")
	}

	// assign ID before storing so callers can correlate
	chunk.ChunkID = uint64(len(s.chunks) + 1)

	// record the chunk and all provided spans so later assertions can examine them
	s.chunks = append(s.chunks, chunk)
	s.spans = append(s.spans, spans...)
	return int64(chunk.ChunkID), nil
}

func (s *fakeRepStore) SoftDeleteChunksFromOrdinal(_ context.Context, repID int64, fromOrdinal int) error {
	if repID <= 0 || fromOrdinal < 0 {
		return fmt.Errorf("invalid soft delete args")
	}
	s.softDeleteCall++
	return nil
}

// WithTx implements a very basic transaction emulation.  We snapshot mutable
// fields and restore them if the callback returns an error, effectively
// rolling back.  Initially we only needed to track chunks/spans/soft deletes,
// but later tests can inspect representations as well so we must ensure
// UpsertRepresentation mutations also roll back.
func (s *fakeRepStore) WithTx(ctx context.Context, fn func(tx model.RepresentationStore) error) error {
	origChunks := append([]model.Chunk(nil), s.chunks...)
	origSpans := append([]model.Span(nil), s.spans...)
	origSoft := s.softDeleteCall
	origUpsert := s.upsertCount
	origReps := append([]model.Representation(nil), s.reps...)
	origNext := s.nextRepID

	err := fn(s)
	if err != nil {
		s.chunks = origChunks
		s.spans = origSpans
		s.softDeleteCall = origSoft
		s.upsertCount = origUpsert
		s.reps = origReps
		s.nextRepID = origNext
	}
	return err
}

func TestUpsertChunksForRepresentationTransaction(t *testing.T) {
	st := &fakeRepStore{failAfter: 1}
	rg := ingest.NewRepresentationGenerator(st)
	doc := model.Document{DocID: 42, RelPath: "main.go", DocType: "code"}
	content := []byte(strings.Repeat("line\n", 260))
	err := rg.GenerateRawTextFromContent(context.Background(), doc, content)
	if err == nil {
		t.Fatal("expected error from failing chunk insert")
	}
	if len(st.chunks) != 0 {
		t.Fatalf("expected no chunks after rollback, got %d", len(st.chunks))
	}
	if st.softDeleteCall != 0 {
		t.Fatalf("expected no soft-delete call after rollback")
	}
}
