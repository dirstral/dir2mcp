package tests

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// TestS3FSWalk_HonorsGitIgnore pins issue #487 part 2: when UseGitIgnore is set,
// the rules in discovered .gitignore objects are fetched and applied to S3 keys
// exactly as the local walker applies them, so an operator relying on gitignore
// to keep secrets/junk out of the corpus gets it honored on S3 too.
func TestS3FSWalk_HonorsGitIgnore(t *testing.T) {
	objs := map[string][]byte{
		".gitignore":     []byte("*.log\nsecret/\n"),
		"keep.txt":       []byte("keep"),
		"app.log":        []byte("ignored by *.log"),
		"secret/key.txt": []byte("ignored: under secret/ dir"),
		"sub/.gitignore": []byte("local.tmp\n"),
		"sub/keep.md":    []byte("keep"),
		"sub/local.tmp":  []byte("ignored by sub/.gitignore"),
		"sub/app.log":    []byte("ignored by root *.log (unanchored)"),
	}
	fsys, _ := newFakeS3FS(t, "", objs, "")

	// With gitignore enabled the ignored keys drop out.
	got, err := fsys.Walk(context.Background(), "", corpusfs.Options{UseGitIgnore: true})
	if err != nil {
		t.Fatalf("Walk(gitignore): %v", err)
	}
	rels := map[string]bool{}
	for _, f := range got {
		rels[f.RelPath] = true
	}
	// .gitignore files are regular files and stay discovered (LocalFS parity).
	for _, want := range []string{"keep.txt", "sub/keep.md", ".gitignore", "sub/.gitignore"} {
		if !rels[want] {
			t.Fatalf("expected %q discovered, got %v", want, rels)
		}
	}
	for _, bad := range []string{"app.log", "secret/key.txt", "sub/local.tmp", "sub/app.log"} {
		if rels[bad] {
			t.Fatalf("expected %q excluded by gitignore, but it was discovered", bad)
		}
	}

	// Parity check: with gitignore disabled the same keys come back.
	all, err := fsys.Walk(context.Background(), "", corpusfs.Options{})
	if err != nil {
		t.Fatalf("Walk(no-gitignore): %v", err)
	}
	if len(all) != len(objs) {
		t.Fatalf("without gitignore expected all %d objects, got %d", len(objs), len(all))
	}
}

// TestS3FSWalk_OversizedGitIgnoreErrors verifies that a .gitignore larger than
// the read cap is rejected loudly rather than silently truncated: applying a
// partial rule set would drop trailing ignore rules and wrongly include files.
func TestS3FSWalk_OversizedGitIgnoreErrors(t *testing.T) {
	// > 1 MiB of valid ignore rules so the reader's cap is exceeded.
	big := strings.Repeat("*.log\n", 200000) // 1.2 MB
	objs := map[string][]byte{
		".gitignore": []byte(big),
		"keep.txt":   []byte("keep"),
		"app.log":    []byte("would be ignored if rules loaded"),
	}
	fsys, _ := newFakeS3FS(t, "", objs, "")

	_, err := fsys.Walk(context.Background(), "", corpusfs.Options{UseGitIgnore: true})
	if err == nil {
		t.Fatalf("Walk with oversized .gitignore = nil error, want a truncation error")
	}
	if !strings.Contains(err.Error(), "truncated") && !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Walk error = %v, want it to flag the oversized/truncated .gitignore", err)
	}

	// A within-cap .gitignore still loads and filters normally (control).
	objs[".gitignore"] = []byte("*.log\n")
	okFS, _ := newFakeS3FS(t, "", objs, "")
	got, err := okFS.Walk(context.Background(), "", corpusfs.Options{UseGitIgnore: true})
	if err != nil {
		t.Fatalf("Walk with small .gitignore: %v", err)
	}
	for _, f := range got {
		if f.RelPath == "app.log" {
			t.Fatalf("app.log should be excluded by the small .gitignore, got %v", got)
		}
	}
}

// TestS3FSOpen_UseAfterCloseRejected verifies that a streaming reader rejects
// Read/Seek after Close instead of silently reopening from offset 0 while its
// logical offset is non-zero (which would return corrupted data). It also
// confirms double-Close is safe.
func TestS3FSOpen_UseAfterCloseRejected(t *testing.T) {
	body := []byte("the quick brown fox jumps over the lazy dog")
	objs := map[string][]byte{"corpus/doc.txt": body}
	stub := &fakeS3{objects: objs}
	fsys, err := corpusfs.NewS3FS(&nilLenS3{fakeS3: stub}, corpusfs.S3Config{Bucket: "bkt", Prefix: "corpus"})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Consume some bytes so the logical offset is non-zero before Close.
	buf := make([]byte, 5)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double-Close must be safe.
	if err := rc.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}

	if _, err := rc.Read(make([]byte, 4)); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("Read after Close = %v, want fs.ErrClosed", err)
	}
	if _, err := rc.Seek(0, io.SeekCurrent); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("Seek after Close = %v, want fs.ErrClosed", err)
	}
}

// nilLenS3 wraps fakeS3 but reports a nil Content-Length from HeadObject, as some
// MinIO/R2/gateway setups do.
type nilLenS3 struct {
	*fakeS3
}

func (n *nilLenS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if _, ok := n.objects[aws.ToString(in.Key)]; !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.HeadObjectOutput{ContentLength: nil}, nil
}

// TestS3FSOpen_NilContentLengthStreams pins issue #487 part 3: an object whose
// HEAD omits Content-Length must stream its full bytes rather than silently
// returning an empty read (which a size-0 range reader would produce).
func TestS3FSOpen_NilContentLengthStreams(t *testing.T) {
	body := []byte("the quick brown fox jumps over the lazy dog")
	objs := map[string][]byte{"corpus/doc.txt": body}
	stub := &fakeS3{objects: objs}
	fsys, err := corpusfs.NewS3FS(&nilLenS3{fakeS3: stub}, corpusfs.S3Config{Bucket: "bkt", Prefix: "corpus"})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	rc, err := fsys.Open(context.Background(), "doc.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	// Seek(0, SeekCurrent) must report the current position without consuming.
	if pos, err := rc.Seek(0, io.SeekCurrent); err != nil || pos != 0 {
		t.Fatalf("Seek(0,Current) = (%d,%v), want (0,nil)", pos, err)
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("nil-Content-Length read mismatch:\n got %q\nwant %q", got, body)
	}

	// A backward seek on a forward-only stream is a clear error, not a silent no-op.
	if _, err := rc.Seek(0, io.SeekStart); err == nil {
		t.Fatalf("expected backward Seek to error on a streaming object")
	}
}

// TestNew_S3RegionEndpointValidation pins issue #487 part 4: an s3 source with no
// region and no custom endpoint is rejected with a clear error before any client
// is built, and a custom endpoint must be a valid http(s) URL with a host.
func TestNew_S3RegionEndpointValidation(t *testing.T) {
	restore := corpusfs.SetS3ClientBuilderForTest(func(_ context.Context, _ corpusfs.Config) (corpusfs.S3API, error) {
		t.Fatal("client builder must not be called when config is invalid")
		return nil, nil
	})
	defer restore()

	cases := []struct {
		name    string
		cfg     corpusfs.Config
		wantSub string
	}{
		{
			name:    "no region and no endpoint",
			cfg:     corpusfs.Config{Kind: "s3", S3Bucket: "b"},
			wantSub: "region",
		},
		{
			name:    "endpoint without scheme",
			cfg:     corpusfs.Config{Kind: "s3", S3Bucket: "b", S3Endpoint: "minio:9000"},
			wantSub: "http or https",
		},
		{
			name:    "endpoint with non-http scheme",
			cfg:     corpusfs.Config{Kind: "s3", S3Bucket: "b", S3Endpoint: "ftp://minio:9000"},
			wantSub: "http or https",
		},
		{
			name:    "endpoint without host",
			cfg:     corpusfs.Config{Kind: "s3", S3Bucket: "b", S3Endpoint: "http://"},
			wantSub: "host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := corpusfs.New(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("New(%+v) err = %v, want substring %q", tc.cfg, err, tc.wantSub)
			}
		})
	}
}

// TestNew_S3EndpointErrorRedactsUserinfo pins the security fix: a rejected
// endpoint that embeds credentials (http://user:pass@host) must not leak the
// userinfo into the returned error, which lands in logs/startup output.
func TestNew_S3EndpointErrorRedactsUserinfo(t *testing.T) {
	restore := corpusfs.SetS3ClientBuilderForTest(func(_ context.Context, _ corpusfs.Config) (corpusfs.S3API, error) {
		t.Fatal("client builder must not be called when config is invalid")
		return nil, nil
	})
	defer restore()

	// Non-http scheme so validation rejects it, with secret userinfo embedded.
	cfg := corpusfs.Config{Kind: "s3", S3Bucket: "b", S3Endpoint: "ftp://alice:hunter2@minio:9000"}
	_, err := corpusfs.New(context.Background(), cfg)
	if err == nil {
		t.Fatalf("New(%+v) = nil error, want validation failure", cfg)
	}
	for _, secret := range []string{"hunter2", "alice:hunter2", "alice"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("endpoint error leaked userinfo %q: %v", secret, err)
		}
	}
	// It should still be actionable: name the offending scheme.
	if !strings.Contains(err.Error(), "http or https") {
		t.Fatalf("endpoint error = %v, want it to flag the scheme requirement", err)
	}
}

// TestNew_S3CustomEndpointNoRegionOK confirms a valid custom endpoint makes a
// region optional (S3-compatible stores such as MinIO/R2 do not need one).
func TestNew_S3CustomEndpointNoRegionOK(t *testing.T) {
	restore := corpusfs.SetS3ClientBuilderForTest(func(_ context.Context, _ corpusfs.Config) (corpusfs.S3API, error) {
		return &fakeS3{objects: map[string][]byte{}}, nil
	})
	defer restore()

	fsys, err := corpusfs.New(context.Background(), corpusfs.Config{
		Kind:       "s3",
		StateDir:   t.TempDir(),
		S3Bucket:   "b",
		S3Endpoint: "http://localhost:9000",
	})
	if err != nil {
		t.Fatalf("New(s3, custom endpoint, no region): %v", err)
	}
	if _, ok := fsys.(*corpusfs.S3FS); !ok {
		t.Fatalf("New = %T, want *corpusfs.S3FS", fsys)
	}
}
