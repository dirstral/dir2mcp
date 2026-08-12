package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// Range-response integrity and reader lifecycle (dir2mcp #673).
//
// The known-size reader satisfies a read with a ranged GET and then trusted
// whatever came back. Two ordinary server behaviours therefore returned the WRONG
// BYTES with no error at all:
//
//   - a proxy or gateway that DROPS the Range header answers with the whole
//     object, so a reader positioned at byte N receives the bytes at byte 0;
//   - a body that ends early is a PREFIX of the object, and it arrived as io.EOF,
//     which every caller reads as "the file ended here".
//
// Both corrupt the corpus quietly: the wrong bytes are hashed, chunked, embedded
// and cited, so `search` answers from content the document does not contain at
// that position and a citation does not match the source. There is no error to
// find in a log afterwards.
//
// A closed reader that reopens is the same defect with a lifecycle trigger: the
// reopened GET is a fresh request, and if the store ignores its Range the caller
// silently continues at byte 0.

// rangeBehavior673 selects how the stub answers a ranged GET.
type rangeBehavior673 int

const (
	// honorRange673 is a conforming store: it serves the requested tail and states
	// the range it served, as a 206 must.
	honorRange673 rangeBehavior673 = iota
	// ignoreRange673 is the proxy that drops the header: the whole object, a 200,
	// and no Content-Range at all.
	ignoreRange673
	// wrongOffset673 states a range it did not serve from.
	wrongOffset673
	// lengthOnly673 serves the correct tail but states it only through
	// Content-Length, with no Content-Range.
	lengthOnly673
)

// rangeStubS3For673 is a network-free S3 stub whose range behaviour and body
// length are set per test. No credentials and no endpoint are involved.
type rangeStubS3For673 struct {
	key      string
	object   []byte
	headSize *int64 // nil: HEAD reports len(object)
	behavior rangeBehavior673
	// serveBytes truncates the served body to this many bytes when it is > 0. It
	// models a connection that ends early or a store that returns a short object.
	serveBytes int64

	gets atomic.Int64
}

func (f *rangeStubS3For673) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}, nil
}

func (f *rangeStubS3For673) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if aws.ToString(in.Key) != f.key {
		return nil, &s3types.NoSuchKey{}
	}
	size := int64(len(f.object))
	if f.headSize != nil {
		size = *f.headSize
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(size)}, nil
}

func (f *rangeStubS3For673) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if aws.ToString(in.Key) != f.key {
		return nil, &s3types.NoSuchKey{}
	}
	f.gets.Add(1)
	start, _ := parseRangeStart(aws.ToString(in.Range))
	total := int64(len(f.object))

	served := f.object
	if f.behavior != ignoreRange673 && start > 0 && start <= total {
		served = f.object[start:]
	}
	if f.serveBytes > 0 && int64(len(served)) > f.serveBytes {
		served = served[:f.serveBytes]
	}
	out := &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(served)),
		ContentLength: aws.Int64(int64(len(served))),
	}
	switch f.behavior {
	case ignoreRange673:
		// A 200: the whole object, and no range stated.
	case wrongOffset673:
		out.ContentRange = aws.String(fmt.Sprintf("bytes 0-%d/%d", total-1, total))
	case lengthOnly673:
		// Content-Length only, which is still proof of the tail.
	case honorRange673:
		out.ContentRange = aws.String(fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
	}
	return out, nil
}

// object673 is a body whose every byte names its own offset, so a test can prove
// WHICH bytes it received, not only how many.
func object673(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func newRangeFS673(t *testing.T, stub *rangeStubS3For673) *corpusfs.S3FS {
	t.Helper()
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{Bucket: "bkt", Prefix: "corpus/", CacheDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	return fsys
}

// TestS3RangeReader_RefusesAServerThatIgnoresTheRange is the core case. The store
// answers a `bytes=32-` request with the whole object, so the reader is at byte 32
// and the bytes are the ones at byte 0.
//
// On `main` the read succeeds and returns the head of the file as the tail of the
// file. Nothing downstream can detect that, so the document is indexed with the
// wrong content at the wrong offset.
func TestS3RangeReader_RefusesAServerThatIgnoresTheRange(t *testing.T) {
	obj := object673(64)
	stub := &rangeStubS3For673{key: "corpus/doc.txt", object: obj, behavior: ignoreRange673}
	fsys := newRangeFS673(t, stub)

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(32, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, readErr := io.ReadAll(rc)
	if !errors.Is(readErr, corpusfs.ErrRangeNotHonored) {
		t.Fatalf("read error = %v, want ErrRangeNotHonored (got %d bytes)", readErr, len(got))
	}
	if bytes.HasPrefix(obj, got) && len(got) > 0 {
		t.Errorf("read returned the bytes at offset 0 for a request that asked for offset 32: %q", got)
	}
}

// TestS3RangeReader_RefusesARangeServedFromTheWrongOffset covers the store that
// DOES state a range, but not the one it was asked for. The stated start is the
// evidence, so it must be compared rather than assumed to match.
func TestS3RangeReader_RefusesARangeServedFromTheWrongOffset(t *testing.T) {
	stub := &rangeStubS3For673{key: "corpus/doc.txt", object: object673(64), behavior: wrongOffset673}
	fsys := newRangeFS673(t, stub)

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(16, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if _, err := io.ReadAll(rc); !errors.Is(err, corpusfs.ErrRangeNotHonored) {
		t.Fatalf("read error = %v, want ErrRangeNotHonored", err)
	}
}

// TestS3RangeReader_ShortBodyIsNotEOF is the truncation half. HEAD reports 4096
// bytes and the body delivers 100, so the reader holds a PREFIX of the document.
//
// The precedent #682 set is followed here: a short read gets its own error and
// never io.EOF, because io.EOF tells every caller the file ended there. That is
// the #487 truncated-document failure, and it ends as a stored document whose
// content and content hash belong to bytes the corpus does not have.
//
// On `main` io.ReadAll returns the 100 bytes with a nil error, at offset 0 and
// after a seek alike.
func TestS3RangeReader_ShortBodyIsNotEOF(t *testing.T) {
	const reported = 4096
	const delivered = 100

	for _, tc := range []struct {
		name string
		seek int64
	}{
		{name: "from the start", seek: 0},
		{name: "after a seek", seek: 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := int64(reported)
			stub := &rangeStubS3For673{
				key:        "corpus/report.txt",
				object:     object673(reported),
				headSize:   &head,
				behavior:   honorRange673,
				serveBytes: delivered,
			}
			fsys := newRangeFS673(t, stub)

			rc, err := fsys.Open(context.Background(), "report.txt")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = rc.Close() }()

			if tc.seek > 0 {
				if _, err := rc.Seek(tc.seek, io.SeekStart); err != nil {
					t.Fatalf("Seek: %v", err)
				}
			}
			got, readErr := io.ReadAll(rc)
			if !errors.Is(readErr, corpusfs.ErrObjectTruncated) {
				t.Fatalf("read error = %v, want ErrObjectTruncated after %d of %d bytes", readErr, len(got), reported)
			}
			if len(got) != delivered {
				t.Errorf("read delivered %d bytes, want the %d the body served", len(got), delivered)
			}
		})
	}
}

// TestS3RangeReader_StaysClosed pins the lifecycle. A closed reader is finished:
// Read and Seek report fs.ErrClosed, and no new GET is issued.
//
// On `main` Close only drops the body, so the next Read opens a SECOND ranged GET
// on a reader the caller already released. That request can be answered by a store
// that ignores the Range, which returns byte 0 while the caller believes it reads
// from its saved offset.
func TestS3RangeReader_StaysClosed(t *testing.T) {
	stub := &rangeStubS3For673{key: "corpus/doc.txt", object: object673(64), behavior: honorRange673}
	fsys := newRangeFS673(t, stub)

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 8)); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	getsAtClose := stub.gets.Load()

	if _, err := rc.Read(make([]byte, 8)); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Read after Close = %v, want fs.ErrClosed", err)
	}
	if _, err := rc.Seek(0, io.SeekStart); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Seek after Close = %v, want fs.ErrClosed", err)
	}
	if err := rc.Close(); err != nil {
		t.Errorf("second Close = %v, want nil (double-Close is a no-op)", err)
	}
	if gets := stub.gets.Load(); gets != getsAtClose {
		t.Errorf("%d GetObject calls after Close, want none: a closed reader must not reopen", gets-getsAtClose)
	}
}

// TestS3StreamReader_StaysClosed pins the same lifecycle for the unknown-length
// reader, which is the variant Open returns when HEAD omits Content-Length. It
// already behaved this way; the assertion is here so BOTH variants carry the rule
// and a later change to one of them cannot quietly drop it.
func TestS3StreamReader_StaysClosed(t *testing.T) {
	stub := &rangeStubS3For673{key: "corpus/nolen.bin", object: object673(64), behavior: ignoreRange673}
	stub.headSize = nil
	fsys, err := corpusfs.NewS3FS(&noLengthHeadS3For673{rangeStubS3For673: stub}, corpusfs.S3Config{
		Bucket: "bkt", Prefix: "corpus/", CacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	rc, err := fsys.Open(context.Background(), "nolen.bin")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 8)); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	getsAtClose := stub.gets.Load()

	if _, err := rc.Read(make([]byte, 8)); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Read after Close = %v, want fs.ErrClosed", err)
	}
	if _, err := rc.Seek(0, io.SeekCurrent); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Seek after Close = %v, want fs.ErrClosed", err)
	}
	if gets := stub.gets.Load(); gets != getsAtClose {
		t.Errorf("%d GetObject calls after Close, want none", gets-getsAtClose)
	}
}

// noLengthHeadS3For673 is the stub with HEAD's Content-Length dropped, the shape
// #487 documented on some MinIO/R2 gateways. Open then returns the streaming
// reader instead of the ranged one.
type noLengthHeadS3For673 struct {
	*rangeStubS3For673
}

func (f *noLengthHeadS3For673) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if aws.ToString(in.Key) != f.key {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.HeadObjectOutput{}, nil
}

// TestS3RangeReader_HonestServerRoundTripsARangedRead is the false-positive guard.
// A conforming store must keep working exactly as before: the seek issues one
// ranged GET, the bytes are the tail from that offset, and the read ends at
// io.EOF. The check refuses a wrong answer, not a slow or unusual one.
func TestS3RangeReader_HonestServerRoundTripsARangedRead(t *testing.T) {
	obj := object673(64)
	stub := &rangeStubS3For673{key: "corpus/doc.txt", object: obj, behavior: honorRange673}
	fsys := newRangeFS673(t, stub)

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(40, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read of an honest ranged response failed: %v", err)
	}
	if !bytes.Equal(got, obj[40:]) {
		t.Errorf("read returned %q, want the tail %q", got, obj[40:])
	}
}

// TestS3RangeReader_AcceptsARangeStatedOnlyByItsLength covers the store that
// serves the right tail but states it only through Content-Length. The reported
// length equals the tail from the requested offset, which an ignored range can
// never produce (it returns the whole object), so the response is still proof and
// is accepted. Without this allowance the check would refuse correct data.
func TestS3RangeReader_AcceptsARangeStatedOnlyByItsLength(t *testing.T) {
	obj := object673(64)
	stub := &rangeStubS3For673{key: "corpus/doc.txt", object: obj, behavior: lengthOnly673}
	fsys := newRangeFS673(t, stub)

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(24, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, obj[24:]) {
		t.Errorf("read returned %q, want the tail %q", got, obj[24:])
	}
}

// TestS3RangeReader_RefusalNamesTheObjectNotItsBytes keeps the error message safe
// to log: it names the bucket, the key and the offsets, and never a byte of the
// object.
func TestS3RangeReader_RefusalNamesTheObjectNotItsBytes(t *testing.T) {
	obj := []byte(strings.Repeat("SECRET", 16))
	stub := &rangeStubS3For673{key: "corpus/doc.txt", object: obj, behavior: ignoreRange673}
	fsys := newRangeFS673(t, stub)

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(32, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	_, readErr := io.ReadAll(rc)
	if readErr == nil {
		t.Fatal("read succeeded, want a refusal")
	}
	if strings.Contains(readErr.Error(), "SECRET") {
		t.Errorf("error message carries object content: %q", readErr)
	}
	if !strings.Contains(readErr.Error(), "corpus/doc.txt") {
		t.Errorf("error message does not name the object: %q", readErr)
	}
}
