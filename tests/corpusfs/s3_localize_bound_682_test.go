package tests

import (
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// Bounded object-store reads and downloads (dir2mcp #682).
//
// The size cap was time-of-check only. Discovery sized an object from
// ListObjectsV2 and admitted it, and the bytes then arrived from a separate
// GetObject that nothing limited. An object that reports one size and serves
// another therefore had no bound at all: Localize streamed the whole body onto the
// local cache disk, and a whole-object read streamed it into memory.
//
// Every test here uses a bucket that LIES. List reports a few hundred bytes, and
// HEAD or the body then delivers far more. That is the case a repeated size check
// can never catch, because every number a check can read is a number the object
// chose to report.

// capBytes682 is the default per-object cap the backend applies when none is
// configured (10 MiB). The tests serve more than this so the bound has to act.
const capBytes682 = 10 * 1024 * 1024

// bodyBytes682 is how many bytes the lying bucket actually serves.
const bodyBytes682 = capBytes682 + 2*1024*1024

// listedBytes682 is the size the lying bucket reports to DISCOVERY, which is what
// admits the object under the cap in the first place.
const listedBytes682 = 512

// generatedBody682 is an io.ReadCloser that serves `remaining` bytes without ever
// holding them in memory, and records how many were pulled. Generating the body is
// what makes the test honest about the defect: on `main` the read is unbounded, so
// a body held in a []byte would have to be allocated in full before the test could
// prove anything about the bound.
type generatedBody682 struct {
	remaining int64
	counter   *atomic.Int64
}

func (g *generatedBody682) Read(p []byte) (int, error) {
	if g.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.remaining {
		p = p[:g.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	g.remaining -= int64(len(p))
	g.counter.Add(int64(len(p)))
	return len(p), nil
}

func (g *generatedBody682) Close() error { return nil }

// lyingS3For682 is a network-free S3 stub whose reported object size disagrees
// with the body it serves.
//
// The three sizes are independent on purpose, because in production they come
// from three separate operations: ListObjectsV2 (what discovery measured),
// HeadObject (what Open measured), and the GetObject body (what actually
// arrived). headSize == nil drops HEAD's Content-Length entirely, the shape #487
// documented on some MinIO/R2 gateways and the one case where no reported size
// exists to check at all.
type lyingS3For682 struct {
	key         string
	listSize    int64
	headSize    *int64
	bodySize    int64
	bytesServed atomic.Int64
}

func (f *lyingS3For682) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if !strings.HasPrefix(f.key, aws.ToString(in.Prefix)) {
		return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}, nil
	}
	return &s3.ListObjectsV2Output{
		Contents: []s3types.Object{{
			Key:          aws.String(f.key),
			Size:         aws.Int64(f.listSize),
			ETag:         aws.String("\"etag-682\""),
			LastModified: aws.Time(time.Unix(1700000000, 0)),
		}},
		IsTruncated: aws.Bool(false),
	}, nil
}

func (f *lyingS3For682) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if aws.ToString(in.Key) != f.key {
		return nil, &s3types.NoSuchKey{}
	}
	if f.headSize == nil {
		return &s3.HeadObjectOutput{}, nil
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(*f.headSize)}, nil
}

func (f *lyingS3For682) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if aws.ToString(in.Key) != f.key {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: &generatedBody682{remaining: f.bodySize, counter: &f.bytesServed}}, nil
}

// newLyingS3FS682 builds the stub plus a backend over it, with the default cap.
func newLyingS3FS682(t *testing.T, stub *lyingS3For682, cacheDir string) *corpusfs.S3FS {
	t.Helper()
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{Bucket: "bkt", Prefix: "corpus/", CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	return fsys
}

// cacheDirBytes682 sums the sizes of the files left in the Localize cache dir, so
// a test can assert that a refused download left nothing behind.
func cacheDirBytes682(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read cache dir: %v", err)
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat cache entry %s: %v", e.Name(), err)
		}
		total += info.Size()
	}
	return total
}

// TestS3FSLocalize_BoundsTheDownloadWhenTheObjectLies is the disk half of #682.
// Discovery listed the object at 512 bytes, so the cap admitted it. The body then
// serves 12 MiB. Localize must stop at the cap, refuse the object, and leave no
// partial file in the cache dir.
//
// On `main` it downloads all 12 MiB, reports success, and hands the caller a path
// to a file larger than the operator allowed.
func TestS3FSLocalize_BoundsTheDownloadWhenTheObjectLies(t *testing.T) {
	stub := &lyingS3For682{key: "corpus/big.mp4", listSize: listedBytes682, bodySize: bodyBytes682}
	cacheDir := t.TempDir()
	fsys := newLyingS3FS682(t, stub, cacheDir)

	localPath, cleanup, err := fsys.Localize(context.Background(), "big.mp4")
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Errorf("Localize succeeded for an object that served %d bytes under a %d-byte cap; want a refusal (localized to %q)", bodyBytes682, capBytes682, localPath)
	}
	if served := stub.bytesServed.Load(); served != capBytes682+1 {
		t.Errorf("Localize pulled %d bytes from the body; want exactly %d (cap+1): the bound must stop there, and must not stop short either", served, capBytes682+1)
	}
	if left := cacheDirBytes682(t, cacheDir); left != 0 {
		t.Errorf("cache dir still holds %d bytes after the refused download; a partial file must be removed", left)
	}
}

// TestS3FSOpen_BoundsAWholeObjectReadWhenHeadDisagreesWithDiscovery is the memory
// half of #682 for an object that DOES report a length: discovery listed it at 512
// bytes and HEAD then reports 12 MiB. The ranged reader trusted HEAD, so a
// whole-object read delivered every one of those bytes.
//
// On `main` io.ReadAll returns 12 MiB with no error.
func TestS3FSOpen_BoundsAWholeObjectReadWhenHeadDisagreesWithDiscovery(t *testing.T) {
	head := int64(bodyBytes682)
	stub := &lyingS3For682{key: "corpus/report.txt", listSize: listedBytes682, headSize: &head, bodySize: bodyBytes682}
	fsys := newLyingS3FS682(t, stub, t.TempDir())

	rc, err := fsys.Open(context.Background(), "report.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, readErr := io.ReadAll(rc)
	if readErr == nil {
		t.Errorf("read of a %d-byte object completed with no error under a %d-byte cap; want a refusal (got %d bytes)", bodyBytes682, capBytes682, len(got))
	}
	// limit+1: one byte past the cap is what proves the object is over it rather
	// than exactly at it. That byte is the whole allowance.
	if int64(len(got)) > capBytes682+1 {
		t.Errorf("read returned %d bytes under a %d-byte cap; the bound is cap+1", len(got), capBytes682)
	}
	if served := stub.bytesServed.Load(); served != capBytes682+1 {
		t.Errorf("read pulled %d bytes from the body; want exactly %d (cap+1): the bound must stop there, and must not stop short either", served, capBytes682+1)
	}
}

// TestS3FSOpen_BoundsAWholeObjectReadWithNoReportedLength is the case where no
// size check of any kind could help: HEAD omits Content-Length, so there is no
// reported size to compare against. The backend falls back to a whole-object
// streaming GET (#487), and only a bound on the arriving bytes can hold it.
//
// On `main` the stream reader is unbounded and io.ReadAll returns all 12 MiB.
func TestS3FSOpen_BoundsAWholeObjectReadWithNoReportedLength(t *testing.T) {
	stub := &lyingS3For682{key: "corpus/nolen.bin", listSize: listedBytes682, bodySize: bodyBytes682}
	fsys := newLyingS3FS682(t, stub, t.TempDir())

	rc, err := fsys.Open(context.Background(), "nolen.bin")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, readErr := io.ReadAll(rc)
	if readErr == nil {
		t.Errorf("unknown-length object read to completion with no error; a %d-byte read under a %d-byte cap must be refused (got %d bytes)", bodyBytes682, capBytes682, len(got))
	}
	// limit+1: one byte past the cap is what proves the object is over it rather
	// than exactly at it. That byte is the whole allowance.
	if int64(len(got)) > capBytes682+1 {
		t.Errorf("read returned %d bytes under a %d-byte cap; the bound is cap+1", len(got), capBytes682)
	}
	if served := stub.bytesServed.Load(); served != capBytes682+1 {
		t.Errorf("read pulled %d bytes from the body; want exactly %d (cap+1): the bound must stop there, and must not stop short either", served, capBytes682+1)
	}
}

// TestS3FSOpen_ReadsAnHonestObjectInFull is the false-positive guard. An object
// whose reported size and body agree, and which fits the cap, must still read
// completely: the bound must refuse only what passes it.
func TestS3FSOpen_ReadsAnHonestObjectInFull(t *testing.T) {
	const size = 3 * 1024 * 1024
	head := int64(size)
	stub := &lyingS3For682{key: "corpus/honest.txt", listSize: size, headSize: &head, bodySize: size}
	fsys := newLyingS3FS682(t, stub, t.TempDir())

	rc, err := fsys.Open(context.Background(), "honest.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read of an in-cap object failed: %v", err)
	}
	if len(got) != size {
		t.Errorf("read returned %d bytes, want the whole %d-byte object", len(got), size)
	}
}
