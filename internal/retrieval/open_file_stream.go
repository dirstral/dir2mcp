package retrieval

import (
	"bufio"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/model"
)

// This file holds the bounded read path for open_file (issue #690).
//
// open_file publishes a bounded answer: max_chars is clamped to 50000 runes.
// The raw-source path used to read the COMPLETE local file or S3 object into
// memory, convert the full byte slice to a string, scan that string, slice it,
// and only then truncate. A source that grew after indexing, or a large remote
// text file, therefore cost unbounded memory for a strictly bounded answer.
//
// The selectors below stream the source instead. Each one retains at most one
// read budget of bytes, and it discards every other byte after it has looked at
// it. Peak memory is therefore a function of max_chars, not of source size.
// The shape follows internal/protocol.ReadLimitedResponseBody (PR #803): read
// through io.LimitReader, keep one unit past the cap so an overflow is visible,
// and give the caller a clear result instead of a confusing failure.

const (
	// openFileReadChunkBytes is the fixed working buffer that moves source
	// bytes through the selectors and the secret scanner. It bounds the
	// per-read allocation, and it is large enough that one syscall serves many
	// lines of ordinary text.
	openFileReadChunkBytes = 64 << 10

	// secretScanOverlapBytes is the number of trailing bytes that carry from
	// one scanned chunk into the next. A secret that lands on a chunk boundary
	// is still matched, because the scanner always sees the boundary inside one
	// window. 4 KiB is far longer than the shortest match of any credential
	// pattern the product ships or an operator writes.
	secretScanOverlapBytes = 4 << 10

	// secretScanMarginBytes is how far past the answer the scanner keeps
	// reading. The margin covers a secret that starts inside the answer window
	// and continues past its end, and it holds the scan to a fixed cost instead
	// of the size of the source.
	//
	// The value matches the ingest-time secret sample (64 KiB, see
	// internal/ingest.secretScanSampleBytes). open_file always reads from the
	// first byte of the source, so every request scans at least the same head
	// bytes that ingest scans. A document that ingest withholds can therefore
	// never be served by the tool, which is what SPEC 15.4 requires.
	secretScanMarginBytes = 64 << 10
)

// errSecretMatch reports that a secret pattern matched the source. It travels
// out of the reader, so a match stops the read at once instead of after the
// whole source has moved. The caller maps it to model.ErrForbidden, which is
// the error the tool contract publishes.
var errSecretMatch = errors.New("retrieval: source matches a secret pattern")

// openFileReadBudgetBytes returns the number of source bytes that a selector
// may retain to build a maxChars-rune answer.
//
// The budget is NOT maxChars. A caller asks for characters, and the bytes that
// carry those characters depend on the encoding: one UTF-8 rune takes up to
// utf8.UTFMax bytes. maxChars*utf8.UTFMax bytes therefore always hold at least
// maxChars runes, whatever the text. One extra rune of slack makes an
// over-length source visible: when a selector fills the budget, the retained
// text holds more than maxChars runes, so the rune truncation that follows both
// cuts the answer to the published size and reports truncated=true. The slack
// also keeps a rune that is split at the budget edge out of the answer, because
// that partial rune always sits past position maxChars.
func openFileReadBudgetBytes(maxChars int) int {
	if maxChars <= 0 {
		maxChars = 1
	}
	return (maxChars + 1) * utf8.UTFMax
}

// spanSelection is the bounded result of a streaming selector.
type spanSelection struct {
	// text is the selected content. It never exceeds the read budget.
	text string
	// truncated reports that the selector stopped at the budget, so the source
	// holds more content for this span than the answer carries.
	truncated bool
}

// ctxReader stops a read as soon as the request context is done. A large local
// file and a large remote object both move through many Read calls, so the
// cancellation lands within one chunk instead of after the whole source.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// secretScanner applies the configured secret patterns to a stream instead of
// to one buffered string. It keeps only a small overlap window, so every byte
// that open_file reads is covered while memory stays flat.
//
// One property differs from a whole-string match: a pattern that is anchored to
// the start or the end of the text (^ or $ without the multi-line flag) sees a
// chunk boundary as a text boundary. The drift is fail-closed, because it can
// only refuse content that a whole-string match would have served. It never
// serves content that a whole-string match would have refused.
type secretScanner struct {
	patterns []*regexp.Regexp
	// tail holds the last bytes of the previous chunk, and window joins that
	// tail to the current chunk. Both buffers are reused across chunks, so a
	// scan of a large source allocates nothing per chunk.
	tail   []byte
	window []byte
}

func newSecretScanner(patterns []*regexp.Regexp) *secretScanner {
	active := make([]*regexp.Regexp, 0, len(patterns))
	for _, re := range patterns {
		if re != nil {
			active = append(active, re)
		}
	}
	return &secretScanner{patterns: active}
}

// enabled reports whether any pattern is configured. With no pattern the read
// stops as soon as the answer is complete, so an object store never sends the
// bytes past the requested window.
func (sc *secretScanner) enabled() bool {
	return sc != nil && len(sc.patterns) > 0
}

// scan checks one chunk together with the tail of the previous chunk.
func (sc *secretScanner) scan(p []byte) error {
	if !sc.enabled() || len(p) == 0 {
		return nil
	}
	window := p
	if len(sc.tail) > 0 {
		sc.window = append(sc.window[:0], sc.tail...)
		sc.window = append(sc.window, p...)
		window = sc.window
	}
	for _, re := range sc.patterns {
		if re.Match(window) {
			return errSecretMatch
		}
	}
	sc.keepTail(window)
	return nil
}

// keepTail stores the last secretScanOverlapBytes bytes of the scanned window.
func (sc *secretScanner) keepTail(window []byte) {
	keep := window
	if len(keep) > secretScanOverlapBytes {
		keep = keep[len(keep)-secretScanOverlapBytes:]
	}
	sc.tail = append(sc.tail[:0], keep...)
}

// scanMargin reads secretScanMarginBytes past the answer and keeps none of
// them, so a secret that begins inside the answer window and runs past its end
// is still matched. With no pattern configured the function returns at once and
// those bytes stay unread.
func (sc *secretScanner) scanMargin(r io.Reader) error {
	if !sc.enabled() {
		return nil
	}
	// io.Discard reads through a pooled buffer, so the margin keeps no bytes
	// and allocates nothing per chunk.
	if _, err := io.Copy(io.Discard, io.LimitReader(r, secretScanMarginBytes)); err != nil {
		return err
	}
	return nil
}

// scanningReader feeds every byte that a selector reads to the secret scanner
// before the selector sees it. The scanner therefore covers the stream in
// order, across both the selection phase and the margin phase.
type scanningReader struct {
	r       io.Reader
	scanner *secretScanner
}

func (s *scanningReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		if scanErr := s.scanner.scan(p[:n]); scanErr != nil {
			return n, scanErr
		}
	}
	return n, err
}

// boundedBuilder collects selected text up to a hard byte limit. It reports
// when it is full, so a selector can stop reading instead of growing.
type boundedBuilder struct {
	b     strings.Builder
	limit int
	full  bool
	// wroteLine records that writeLine has run at least once. An empty first
	// line still needs the separator before the second line, so the flag cannot
	// be replaced by a length test.
	wroteLine bool
}

func newBoundedBuilder(limit int) *boundedBuilder {
	if limit < 0 {
		limit = 0
	}
	return &boundedBuilder{limit: limit}
}

// writeString appends as much of s as the limit allows.
func (bb *boundedBuilder) writeString(s string) {
	if bb.full || s == "" {
		return
	}
	room := bb.limit - bb.b.Len()
	if room <= 0 {
		bb.full = true
		return
	}
	if len(s) > room {
		s = s[:room]
		bb.full = true
	}
	bb.b.WriteString(s)
	if bb.b.Len() >= bb.limit {
		bb.full = true
	}
}

// writeLine appends one line, with a newline separator before every line but
// the first. It mirrors strings.Join(lines, "\n").
func (bb *boundedBuilder) writeLine(line string) {
	if bb.wroteLine {
		bb.writeString("\n")
	}
	bb.wroteLine = true
	bb.writeString(line)
}

func (bb *boundedBuilder) String() string { return bb.b.String() }

func (bb *boundedBuilder) len() int { return bb.b.Len() }

// sourceLine is one logical line of the source.
type sourceLine struct {
	text string
	// truncated reports that the line was longer than the retained limit. The
	// rest of the line was consumed and dropped.
	truncated bool
}

// lineReader yields the logical lines of a stream. It splits on "\n" exactly as
// strings.Split does, so a source that ends with a newline yields a final empty
// line and an empty source yields one empty line. It retains at most limit
// bytes of any single line, so one very long line cannot exhaust memory.
type lineReader struct {
	br    *bufio.Reader
	limit int
	done  bool
}

func newLineReader(r io.Reader, limit int) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, openFileReadChunkBytes), limit: limit}
}

// next returns the next line. ok=false means the stream is finished.
func (lr *lineReader) next() (sourceLine, bool, error) {
	if lr.done {
		return sourceLine{}, false, nil
	}
	out := newBoundedBuilder(lr.limit)
	dropped := false
	for {
		chunk, err := lr.br.ReadSlice('\n')
		switch {
		case err == nil:
			appendChunk(out, chunk[:len(chunk)-1], &dropped)
			return sourceLine{text: out.String(), truncated: dropped}, true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			appendChunk(out, chunk, &dropped)
		case errors.Is(err, io.EOF):
			lr.done = true
			appendChunk(out, chunk, &dropped)
			return sourceLine{text: out.String(), truncated: dropped}, true, nil
		default:
			return sourceLine{}, false, err
		}
	}
}

// appendChunk adds a chunk to a bounded builder and records whether the builder
// had to drop bytes.
func appendChunk(out *boundedBuilder, chunk []byte, dropped *bool) {
	before := out.len()
	out.writeString(string(chunk))
	if out.len()-before < len(chunk) {
		*dropped = true
	}
}

// selectSpan streams the source and returns the requested window. It reproduces
// the span semantics of the buffered path: an empty kind with no line numbers
// returns the head of the document, "lines" returns a line range, "page"
// returns a form-feed delimited page, and "time" returns the timestamped lines
// inside a time window.
func selectSpan(r io.Reader, kind string, span model.Span, budget int) (spanSelection, error) {
	switch kind {
	case "", "lines":
		if kind == "lines" || span.StartLine > 0 || span.EndLine > 0 {
			return selectLineRange(r, span.StartLine, span.EndLine, budget)
		}
		return selectPrefix(r, budget)
	case "page":
		return selectPage(r, span.Page, budget)
	case "time":
		return selectTimeRange(r, span, budget)
	default:
		return spanSelection{}, model.ErrDocTypeUnsupported
	}
}

// selectPrefix returns the head of the document. It reads one budget of bytes
// and stops, so a 5 GB text file costs the same as a 5 KB one.
func selectPrefix(r io.Reader, budget int) (spanSelection, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(budget)))
	if err != nil {
		return spanSelection{}, err
	}
	return spanSelection{text: string(data), truncated: len(data) >= budget}, nil
}

// selectLineRange returns lines start..end. It streams to the first requested
// line and keeps none of the lines before it, so "line 9000 of a large file" is
// a seek problem, not a buffering problem: the prefix passes through the fixed
// working buffer and is dropped. Only the requested lines are retained, and
// only up to the budget.
func selectLineRange(r io.Reader, start, end, budget int) (spanSelection, error) {
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end < start {
		end = start
	}
	lr := newLineReader(r, budget)
	out := newBoundedBuilder(budget)
	truncated := false
	for idx := 1; idx <= end; idx++ {
		line, ok, err := lr.next()
		if err != nil {
			return spanSelection{}, err
		}
		if !ok {
			break
		}
		if idx < start {
			continue
		}
		if line.truncated {
			truncated = true
		}
		out.writeLine(line.text)
		if out.full {
			truncated = true
			break
		}
	}
	return spanSelection{text: out.String(), truncated: truncated}, nil
}

// selectPage returns one form-feed delimited page. It discards every earlier
// page as it passes, and it retains at most one budget of the requested page.
//
// The page count follows the buffered rule (#427): a form feed at the very end
// of the document terminates the last page, it does not open a phantom empty
// page. A document with more than one page also has its page text trimmed of
// leading and trailing newlines, so the reader must know whether a second page
// exists before it can answer for page 1. One peeked byte answers that.
func selectPage(r io.Reader, page, budget int) (spanSelection, error) {
	if page <= 0 {
		page = 1
	}
	br := bufio.NewReaderSize(r, openFileReadChunkBytes)
	for idx := 1; idx < page; idx++ {
		_, _, delimited, err := readFormFeedSegment(br, 0)
		if err != nil {
			return spanSelection{}, err
		}
		if !delimited {
			// The document ended before this page began.
			return spanSelection{}, model.ErrDocTypeUnsupported
		}
	}

	text, truncated, delimited, err := readFormFeedSegment(br, budget)
	if err != nil {
		return spanSelection{}, err
	}
	multiPage := page > 1
	if delimited {
		more, peekErr := hasMoreBytes(br)
		if peekErr != nil {
			return spanSelection{}, peekErr
		}
		multiPage = multiPage || more
	} else if page > 1 && text == "" && !truncated {
		// A trailing form feed produced this empty segment. It is not a page.
		return spanSelection{}, model.ErrDocTypeUnsupported
	}
	if multiPage {
		text = strings.Trim(text, "\n")
	}
	return spanSelection{text: text, truncated: truncated}, nil
}

// readFormFeedSegment consumes one form-feed delimited segment. It retains at
// most limit bytes of it and drops the rest, and it always consumes the segment
// to its end so the caller can look at what follows. delimited reports that a
// form feed, not the end of the source, closed the segment.
func readFormFeedSegment(br *bufio.Reader, limit int) (text string, truncated, delimited bool, err error) {
	out := newBoundedBuilder(limit)
	dropped := false
	for {
		chunk, readErr := br.ReadSlice('\f')
		switch {
		case readErr == nil:
			appendChunk(out, chunk[:len(chunk)-1], &dropped)
			return out.String(), dropped, true, nil
		case errors.Is(readErr, bufio.ErrBufferFull):
			appendChunk(out, chunk, &dropped)
		case errors.Is(readErr, io.EOF):
			appendChunk(out, chunk, &dropped)
			return out.String(), dropped, false, nil
		default:
			return "", false, false, readErr
		}
	}
}

// hasMoreBytes reports whether the stream holds at least one more byte.
func hasMoreBytes(br *bufio.Reader) (bool, error) {
	if _, err := br.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// selectTimeRange returns the timestamped lines inside the requested window. It
// keeps only the matching lines, up to the budget, and it stops reading once
// the budget is full. A source with no timestamp at all is not a transcript, so
// it reports model.ErrDocTypeUnsupported, as the buffered path did.
func selectTimeRange(r io.Reader, span model.Span, budget int) (spanSelection, error) {
	startMS, endMS := normalizeTimeBounds(span)
	lr := newLineReader(r, budget)
	out := newBoundedBuilder(budget)
	truncated := false
	found := false
	for {
		line, ok, err := lr.next()
		if err != nil {
			return spanSelection{}, err
		}
		if !ok {
			break
		}
		m := timePrefixRe.FindStringSubmatch(line.text)
		if len(m) == 0 {
			continue
		}
		found = true
		ts := parseTimestampMS(m[1], m[2], m[3])
		if ts < startMS || (endMS > 0 && ts > endMS) {
			continue
		}
		if line.truncated {
			truncated = true
		}
		out.writeLine(line.text)
		if out.full {
			truncated = true
			break
		}
	}
	if !found {
		return spanSelection{}, model.ErrDocTypeUnsupported
	}
	return spanSelection{text: out.String(), truncated: truncated}, nil
}

// normalizeTimeBounds clamps the requested time window the way the buffered
// path did: negative values become 0, and an end before the start becomes the
// start.
func normalizeTimeBounds(span model.Span) (startMS, endMS int) {
	startMS = span.StartMS
	endMS = span.EndMS
	if startMS < 0 {
		startMS = 0
	}
	if endMS < 0 {
		endMS = 0
	}
	if endMS > 0 && endMS < startMS {
		endMS = startMS
	}
	return startMS, endMS
}
