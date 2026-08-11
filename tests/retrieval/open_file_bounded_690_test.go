package tests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Issue #690: open_file publishes a bounded answer (max_chars, clamped to
// 50000 runes) but used to read the COMPLETE local file or S3 object into
// memory, convert it to a string, scan it, slice it, and only then truncate. A
// source that grew after indexing, or a large remote text file, therefore cost
// unbounded memory and bandwidth for a strictly bounded answer.
//
// The tests below fail against the buffered implementation: the capped readers
// return an error as soon as the read runs past the budget, and the allocation
// probe sees the whole-source copies.

// errReadPastCap reports that a reader was asked for more bytes than the test
// allows. It stands in for the memory and bandwidth an unbounded read spends.
var errReadPastCap = errors.New("read ran past the cap the test allows")

// cappedReader serves a document and fails once a caller has read more than cap
// bytes from it.
type cappedReader struct {
	data []byte
	off  int
	cap  int
	read *int
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if *c.read >= c.cap {
		return 0, fmt.Errorf("%w: %d bytes", errReadPastCap, *c.read)
	}
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.off:])
	c.off += n
	*c.read += n
	return n, nil
}

func (c *cappedReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		c.off = int(offset)
	case io.SeekCurrent:
		c.off += int(offset)
	case io.SeekEnd:
		c.off = len(c.data) + int(offset)
	}
	return int64(c.off), nil
}

func (c *cappedReader) Close() error { return nil }

// cappedCorpusFS is an object-store stand-in whose objects are served through a
// capped reader. It has no local file, so it exercises the same remote read path
// that S3 corpora use (#432).
type cappedCorpusFS struct {
	objects   map[string][]byte
	cap       int
	bytesRead int
}

func (f *cappedCorpusFS) Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := f.objects[relPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &cappedReader{data: data, cap: f.cap, read: &f.bytesRead}, nil
}

func (f *cappedCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("cappedCorpusFS: Walk not implemented")
}

func (f *cappedCorpusFS) Localize(context.Context, string) (string, func(), error) {
	return "", func() {}, errors.New("cappedCorpusFS: Localize not implemented")
}

// cancellingCorpusFS cancels the request context after the first read, then
// keeps serving bytes. A read that honours the context stops at once; a read
// that ignores it returns the whole object.
type cancellingCorpusFS struct {
	data   []byte
	cancel context.CancelFunc
}

type cancellingReader struct {
	data   []byte
	off    int
	cancel context.CancelFunc
}

func (c *cancellingReader) Read(p []byte) (int, error) {
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.off:])
	c.off += n
	c.cancel()
	return n, nil
}

func (c *cancellingReader) Seek(int64, int) (int64, error) { return int64(c.off), nil }
func (c *cancellingReader) Close() error                   { return nil }

func (f *cancellingCorpusFS) Open(ctx context.Context, _ string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &cancellingReader{data: f.data, cancel: f.cancel}, nil
}

func (f *cancellingCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("cancellingCorpusFS: Walk not implemented")
}

func (f *cancellingCorpusFS) Localize(context.Context, string) (string, func(), error) {
	return "", func() {}, errors.New("cancellingCorpusFS: Localize not implemented")
}

// newBoundedService returns a retrieval service rooted at root with no secret
// pattern configured.
func newBoundedService(root string) *retrieval.Service {
	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(filepath.Join(root, ".dir2mcp"))
	return svc
}

// repeatToSize returns unit repeated until it holds at least size bytes.
func repeatToSize(unit string, size int) string {
	return strings.Repeat(unit, size/len(unit)+1)
}

// allocatedBytes reports how many bytes fn allocated in total. Cumulative
// allocation, not live heap, is the honest probe here: a streaming read touches
// every byte of the source but keeps only a window, so its total stays flat
// while a buffered read allocates the source several times over.
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestOpenFile_LocalSourceGrewAfterIndexing_ReadStaysBounded covers the exact
// condition #690 describes: the source is far larger than the answer, because it
// grew after it was indexed. open_file must answer from a bounded read.
//
// The probe is allocation. The buffered path allocated the file as bytes, again
// as a string, and again as a rune slice inside the truncation, so a 24 MiB file
// cost about 150 MiB. The streaming path reads one read budget: 200004 bytes at
// the 50000-rune cap.
func TestOpenFile_LocalSourceGrewAfterIndexing_ReadStaysBounded(t *testing.T) {
	root := t.TempDir()
	relPath := "notes/grown.txt"
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The document as indexed: small.
	if err := os.WriteFile(abs, []byte("short at index time\n"), 0o644); err != nil {
		t.Fatalf("write small file: %v", err)
	}
	// The document as it is now: 24 MiB, with the head still readable.
	const grownBytes = 24 << 20
	head := "alpha bravo charlie delta echo foxtrot golf hotel india juliet\n"
	if err := os.WriteFile(abs, []byte(repeatToSize(head, grownBytes)), 0o644); err != nil {
		t.Fatalf("grow file: %v", err)
	}

	svc := newBoundedService(root)

	var out string
	var truncated bool
	var err error
	allocated := allocatedBytes(func() {
		out, truncated, err = svc.OpenFileWithMeta(context.Background(), relPath, model.Span{}, 50000)
	})
	if err != nil {
		t.Fatalf("OpenFileWithMeta on a grown source returned err: %v", err)
	}
	if got := utf8.RuneCountInString(out); got != 50000 {
		t.Fatalf("answer holds %d runes, want the published cap of 50000", got)
	}
	if !truncated {
		t.Fatalf("expected truncated=true for a 24 MiB source answered with 50000 runes")
	}
	if !strings.HasPrefix(out, "alpha bravo charlie") {
		t.Fatalf("answer does not start with the document head: %q", out[:40])
	}
	// One read budget is 200004 bytes. 8 MiB leaves generous room for the
	// runtime's own allocations while still failing the buffered path, which
	// allocated about 150 MiB for this file.
	const allocCap = 8 << 20
	if allocated > allocCap {
		t.Fatalf("open_file allocated %d bytes for a %d-byte source; want at most %d", allocated, grownBytes, allocCap)
	}
}

// TestOpenFile_CorpusFSLargeObject_StopsAfterTheWindow verifies that a large
// remote text object is not downloaded in full for a bounded answer. The capped
// reader fails the request once the read runs past 1 MiB.
func TestOpenFile_CorpusFSLargeObject_StopsAfterTheWindow(t *testing.T) {
	root := t.TempDir()
	const objectBytes = 16 << 20
	body := repeatToSize("remote object line with enough text to be interesting\n", objectBytes)
	fs := &cappedCorpusFS{objects: map[string][]byte{"docs/big.md": []byte(body)}, cap: 1 << 20}

	svc := newBoundedService(root)
	svc.SetCorpusFS(fs)

	out, truncated, err := svc.OpenFileWithMeta(context.Background(), "docs/big.md", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFileWithMeta over CorpusFS returned err: %v", err)
	}
	if got := utf8.RuneCountInString(out); got != 20000 {
		t.Fatalf("answer holds %d runes, want 20000", got)
	}
	if !truncated {
		t.Fatalf("expected truncated=true for a 16 MiB object answered with 20000 runes")
	}
	if fs.bytesRead > 1<<20 {
		t.Fatalf("read %d bytes of a %d-byte object for a 20000-rune answer", fs.bytesRead, objectBytes)
	}
}

// TestOpenFile_LateLineRange_DoesNotBufferTheTail covers the line addressing
// case. Line 9000 is not the first 50000 characters: the selector must stream
// past the lines before it and keep none of them, and it must stop once the
// requested range is complete instead of buffering the rest of the document.
func TestOpenFile_LateLineRange_DoesNotBufferTheTail(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 9002; i++ {
		fmt.Fprintf(&b, "line %d: the quick brown fox jumps over the lazy dog\n", i)
	}
	prefixBytes := b.Len()
	// A large tail after the requested range. A bounded read never touches it.
	b.WriteString(repeatToSize("tail padding that must never be buffered\n", 16<<20))
	fs := &cappedCorpusFS{objects: map[string][]byte{"docs/lines.txt": []byte(b.String())}, cap: prefixBytes + (1 << 20)}

	svc := newBoundedService(root)
	svc.SetCorpusFS(fs)

	span := model.Span{Kind: "lines", StartLine: 9000, EndLine: 9002}
	out, _, err := svc.OpenFileWithMeta(context.Background(), "docs/lines.txt", span, 20000)
	if err != nil {
		t.Fatalf("OpenFileWithMeta for a late line range returned err: %v", err)
	}
	want := "line 9000: the quick brown fox jumps over the lazy dog\n" +
		"line 9001: the quick brown fox jumps over the lazy dog\n" +
		"line 9002: the quick brown fox jumps over the lazy dog"
	if out != want {
		t.Fatalf("late line range returned %q, want %q", out, want)
	}
}

// TestOpenFile_MultibyteRunes_AnswerIsWholeRunes checks the byte budget against
// the character contract. max_chars counts runes, and a rune takes up to four
// bytes, so the read budget is four bytes per requested character plus slack. A
// budget that reused the max_chars number would return a short answer, and a
// budget that cut a rune would return a replacement character.
func TestOpenFile_MultibyteRunes_AnswerIsWholeRunes(t *testing.T) {
	root := t.TempDir()
	relPath := "docs/multibyte.txt"
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Four-byte runes are the worst case for the byte budget.
	body := strings.Repeat("𝔘𝔫𝔦𝔠𝔬𝔡𝔢", 20000)
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	svc := newBoundedService(root)
	out, truncated, err := svc.OpenFileWithMeta(context.Background(), relPath, model.Span{}, 1000)
	if err != nil {
		t.Fatalf("OpenFileWithMeta on multibyte text returned err: %v", err)
	}
	if got := utf8.RuneCountInString(out); got != 1000 {
		t.Fatalf("answer holds %d runes, want 1000", got)
	}
	if !truncated {
		t.Fatalf("expected truncated=true")
	}
	if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
		t.Fatalf("answer holds a cut rune: %q", out[:20])
	}
	if out != string([]rune(body)[:1000]) {
		t.Fatalf("answer is not the first 1000 runes of the document")
	}
}

// writeSecretDoc writes a document at relPath under root and returns a service
// with one secret pattern configured.
func writeSecretDoc(t *testing.T, root, relPath, body string) *retrieval.Service {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	svc := newBoundedService(root)
	if err := svc.SetSecretPatterns([]string{`AKIA[0-9A-Z]{16}`}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}
	return svc
}

// TestOpenFile_SecretJustPastTheWindow_StillRefuses pins the margin the scanner
// reads past the answer. A secret that starts inside the window, or just after
// it, must refuse the request even though the answer itself looks harmless.
func TestOpenFile_SecretJustPastTheWindow_StillRefuses(t *testing.T) {
	root := t.TempDir()
	relPath := "docs/margin.txt"
	// 20000 runes of answer, then the secret a few bytes later.
	body := strings.Repeat("a", 20050) + "\nAKIAIOSFODNN7EXAMPLE\n" + strings.Repeat("b", 1<<20)
	svc := writeSecretDoc(t, root, relPath, body)

	if _, err := svc.OpenFile(context.Background(), relPath, model.Span{}, 20000); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for a secret just past the window, got %v", err)
	}
}

// TestOpenFile_SecretBeforeTheWindow_AlwaysRefuses pins the property that makes
// the bounded scan sound: the read always starts at the first byte, so a caller
// cannot ask for a late window to step over a secret and reach the text behind
// it. It also keeps the tool from serving a document that ingest withholds,
// because ingest decides on the head of the file.
func TestOpenFile_SecretBeforeTheWindow_AlwaysRefuses(t *testing.T) {
	root := t.TempDir()
	relPath := "docs/early-secret.txt"
	var b strings.Builder
	b.WriteString("AKIAIOSFODNN7EXAMPLE\n")
	for i := 2; i <= 9002; i++ {
		fmt.Fprintf(&b, "line %d: ordinary text\n", i)
	}
	svc := writeSecretDoc(t, root, relPath, b.String())

	span := model.Span{Kind: "lines", StartLine: 9000, EndLine: 9002}
	if _, err := svc.OpenFile(context.Background(), relPath, span, 20000); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected ErrForbidden for a late window behind a secret, got %v", err)
	}
}

// TestOpenFile_SecretFarPastTheWindow_IsNotRead documents the one semantic the
// bounded read changes on purpose. The buffered path read the whole source, so
// a secret 8 MiB down refused the head of the document as well. The bounded
// read stops after the answer and its margin, so the head is served. The bytes
// it returns were all scanned, and ingest itself decides on a 64 KiB sample, so
// this document was indexed and searchable already.
func TestOpenFile_SecretFarPastTheWindow_IsNotRead(t *testing.T) {
	root := t.TempDir()
	relPath := "docs/late-secret.txt"
	head := repeatToSize("harmless prose that says nothing at all\n", 8<<20)
	svc := writeSecretDoc(t, root, relPath, head+"\nAKIAIOSFODNN7EXAMPLE\n")

	var out string
	var err error
	allocated := allocatedBytes(func() {
		out, err = svc.OpenFile(context.Background(), relPath, model.Span{}, 20000)
	})
	if err != nil {
		t.Fatalf("OpenFile returned err: %v", err)
	}
	if !strings.HasPrefix(out, "harmless prose") {
		t.Fatalf("expected the scanned head of the document, got %q", out[:40])
	}
	if strings.Contains(out, "AKIA") {
		t.Fatalf("the answer holds secret material")
	}
	const allocCap = 8 << 20
	if allocated > allocCap {
		t.Fatalf("open_file allocated %d bytes for an 8 MiB source; want at most %d", allocated, allocCap)
	}
}

// TestOpenFile_SecretAcrossAChunkBoundary_StillRefuses checks the overlap the
// streaming scanner carries between chunks. A secret that straddles two reads
// must still match.
func TestOpenFile_SecretAcrossAChunkBoundary_StillRefuses(t *testing.T) {
	root := t.TempDir()
	relPath := "docs/boundary.txt"
	const secret = "AKIAIOSFODNN7EXAMPLE"
	// Place the secret so that it spans several plausible chunk edges. Every
	// offset stays inside the answer plus its scan margin.
	for _, offset := range []int{(64 << 10) - 7, (64 << 10) - 1, 100 << 10} {
		body := strings.Repeat("x", offset) + secret + strings.Repeat("y", 4096)
		svc := writeSecretDoc(t, root, relPath, body)
		if _, err := svc.OpenFile(context.Background(), relPath, model.Span{}, 20000); !errors.Is(err, model.ErrForbidden) {
			t.Fatalf("secret at offset %d: expected ErrForbidden, got %v", offset, err)
		}
	}
}

// TestOpenFile_ContextCancelled_StopsTheRead verifies that a cancelled request
// interrupts a long read instead of running it to the end of the source.
func TestOpenFile_ContextCancelled_StopsTheRead(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fs := &cancellingCorpusFS{data: []byte(repeatToSize("cancel me\n", 8<<20)), cancel: cancel}
	svc := newBoundedService(root)
	svc.SetCorpusFS(fs)
	if err := svc.SetSecretPatterns([]string{`AKIA[0-9A-Z]{16}`}); err != nil {
		t.Fatalf("SetSecretPatterns: %v", err)
	}

	_, err := svc.OpenFile(ctx, "docs/slow.md", model.Span{}, 50000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled once the request is cancelled, got %v", err)
	}
}
