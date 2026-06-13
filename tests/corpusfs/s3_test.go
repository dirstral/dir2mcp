package tests

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// fakeS3 is a network-free stub of the S3 client surface S3FS depends on. It
// serves objects from an in-memory map keyed by full object key.
type fakeS3 struct {
	objects map[string][]byte
	// getRanges records the Range header of each GetObject call for assertions.
	getRanges []string
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	out := &s3.ListObjectsV2Output{}
	for key, body := range f.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out.Contents = append(out.Contents, s3types.Object{
			Key:          aws.String(key),
			Size:         aws.Int64(int64(len(body))),
			ETag:         aws.String("\"etag-" + key + "\""),
			LastModified: aws.Time(time.Unix(1700000000, 0)),
		})
	}
	out.IsTruncated = aws.Bool(false)
	return out, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(body)))}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	rangeHeader := aws.ToString(in.Range)
	f.getRanges = append(f.getRanges, rangeHeader)
	if start, ok := parseRangeStart(rangeHeader); ok && start <= int64(len(body)) {
		body = body[start:]
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

// parseRangeStart extracts the start byte from a "bytes=N-" header.
func parseRangeStart(h string) (int64, bool) {
	const p = "bytes="
	if !strings.HasPrefix(h, p) {
		return 0, false
	}
	rest := strings.TrimPrefix(h, p)
	dash := strings.IndexByte(rest, '-')
	if dash < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(rest[:dash], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func newFakeS3FS(t *testing.T, prefix string, objs map[string][]byte, cacheDir string) (*corpusfs.S3FS, *fakeS3) {
	t.Helper()
	stub := &fakeS3{objects: objs}
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{Bucket: "bkt", Prefix: prefix, CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	return fsys, stub
}

// TestS3FSWalk_KeyMappingExcludeAndSizeCap verifies that Walk maps keys to
// corpus-relative paths under the prefix, skips directory placeholder keys and
// excluded dirs, and enforces the size cap.
func TestS3FSWalk_KeyMappingExcludeAndSizeCap(t *testing.T) {
	objs := map[string][]byte{
		"corpus/keep.txt":            []byte("hello"),
		"corpus/sub/doc.md":          []byte("# doc"),
		"corpus/":                    {},               // directory placeholder
		"corpus/sub/":                {},               // directory placeholder
		"corpus/node_modules/lib.js": []byte("x"),      // excluded dir
		"corpus/.git/config":         []byte("[core]"), // excluded dir
		"corpus/big.bin":             make([]byte, 64),
		"other-prefix/not-mine.txt":  []byte("nope"), // outside prefix
	}
	fsys, _ := newFakeS3FS(t, "corpus/", objs, "")

	got, err := fsys.Walk(context.Background(), "", corpusfs.Options{MaxSizeBytes: 32})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	rels := map[string]corpusfs.DiscoveredFile{}
	for _, f := range got {
		rels[f.RelPath] = f
	}

	for _, want := range []string{"keep.txt", "sub/doc.md"} {
		if _, ok := rels[want]; !ok {
			t.Fatalf("expected %q in Walk output, got %v", want, keys(rels))
		}
	}
	for _, bad := range []string{"", "sub/", "node_modules/lib.js", ".git/config", "big.bin", "../other-prefix/not-mine.txt"} {
		if _, ok := rels[bad]; ok {
			t.Fatalf("did not expect %q in Walk output", bad)
		}
	}

	// sorted by RelPath
	if got[0].RelPath != "keep.txt" || got[1].RelPath != "sub/doc.md" {
		t.Fatalf("expected sorted output, got %q,%q", got[0].RelPath, got[1].RelPath)
	}

	// metadata: AbsPath empty, ETag unquoted, size + mtime populated.
	keep := rels["keep.txt"]
	if keep.AbsPath != "" {
		t.Fatalf("S3 AbsPath should be empty, got %q", keep.AbsPath)
	}
	if keep.ETag != "etag-corpus/keep.txt" {
		t.Fatalf("ETag mismatch: got %q", keep.ETag)
	}
	if keep.SizeBytes != 5 {
		t.Fatalf("size mismatch: got %d", keep.SizeBytes)
	}
	if keep.MTimeUnix != 1700000000 {
		t.Fatalf("mtime mismatch: got %d", keep.MTimeUnix)
	}
}

// TestS3FSWalk_ExcludesDirsNotFilesNamedLikeDirs pins LocalFS↔S3 parity for the
// excluded-dirs policy: objects *under* an excluded directory are skipped, but a
// regular object whose own basename equals an excluded-dir name (e.g. a file
// literally named "vendor") is still discovered — LocalFS excludes directories,
// not files.
func TestS3FSWalk_ExcludesDirsNotFilesNamedLikeDirs(t *testing.T) {
	objs := map[string][]byte{
		"vendor":            []byte("a file, not a dir"), // top-level file named like an excluded dir
		"sub/node_modules":  []byte("also a file"),       // nested file named like an excluded dir
		"vendor/lib.go":     []byte("excluded: under dir"),
		"node_modules/x.js": []byte("excluded: under dir"),
		"sub/.git/config":   []byte("excluded: under dir"),
	}
	fsys, _ := newFakeS3FS(t, "", objs, "")
	got, err := fsys.Walk(context.Background(), "", corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	rels := map[string]bool{}
	for _, f := range got {
		rels[f.RelPath] = true
	}
	for _, want := range []string{"vendor", "sub/node_modules"} {
		if !rels[want] {
			t.Fatalf("expected file %q to be discovered (it is a file, not a dir)", want)
		}
	}
	for _, bad := range []string{"vendor/lib.go", "node_modules/x.js", "sub/.git/config"} {
		if rels[bad] {
			t.Fatalf("did not expect %q (lives under an excluded dir)", bad)
		}
	}
}

// TestS3FSWalk_EmptyPrefix verifies key mapping with no prefix configured.
func TestS3FSWalk_EmptyPrefix(t *testing.T) {
	objs := map[string][]byte{
		"a.txt":     []byte("a"),
		"dir/b.txt": []byte("b"),
	}
	fsys, _ := newFakeS3FS(t, "", objs, "")
	got, err := fsys.Walk(context.Background(), "", corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 2 || got[0].RelPath != "a.txt" || got[1].RelPath != "dir/b.txt" {
		t.Fatalf("unexpected output: %+v", got)
	}
}

// TestS3FSOpen_RangeReadRoundTrip verifies Open round-trips the bytes and uses a
// ranged GET after a seek.
func TestS3FSOpen_RangeReadRoundTrip(t *testing.T) {
	body := []byte("the quick brown fox")
	objs := map[string][]byte{"corpus/a/b.txt": body}
	fsys, stub := newFakeS3FS(t, "corpus", objs, "")

	rc, err := fsys.Open(context.Background(), "a/b.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(4, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf) != "quick" {
		t.Fatalf("range read mismatch: got %q", buf)
	}
	if len(stub.getRanges) == 0 || stub.getRanges[0] != "bytes=4-" {
		t.Fatalf("expected ranged GET bytes=4-, got %v", stub.getRanges)
	}
}

// TestS3FSLocalize_DownloadsToTempPreservingExt verifies Localize downloads to a
// temp file (with the source extension) under the configured cache dir and the
// cleanup removes it.
func TestS3FSLocalize_DownloadsToTempPreservingExt(t *testing.T) {
	body := []byte("media-bytes")
	objs := map[string][]byte{"corpus/clip.mp3": body}
	cacheDir := t.TempDir()
	fsys, _ := newFakeS3FS(t, "corpus", objs, cacheDir)

	localPath, cleanup, err := fsys.Localize(context.Background(), "clip.mp3")
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}

	if filepath.Ext(localPath) != ".mp3" {
		t.Fatalf("expected .mp3 extension preserved, got %q", localPath)
	}
	if filepath.Dir(localPath) != cacheDir {
		t.Fatalf("expected temp file under cache dir %q, got %q", cacheDir, localPath)
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("read localized: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("localized body mismatch: got %q", got)
	}

	cleanup()
	if _, err := os.Stat(localPath); !os.IsNotExist(err) {
		t.Fatalf("expected temp file removed after cleanup, stat err=%v", err)
	}
}

func keys(m map[string]corpusfs.DiscoveredFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
