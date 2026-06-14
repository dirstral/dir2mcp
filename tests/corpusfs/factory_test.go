package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// TestNew_LocalKindReturnsLocalFS asserts the default and explicit local/nfs
// kinds yield a *LocalFS rooted at RootDir — the byte-identical historical path.
func TestNew_LocalKindReturnsLocalFS(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{"", "local", "nfs", "LOCAL", "  nfs  "} {
		fsys, err := corpusfs.New(context.Background(), corpusfs.Config{
			Kind:    kind,
			RootDir: root,
		})
		if err != nil {
			t.Fatalf("New(kind=%q) error: %v", kind, err)
		}
		if _, ok := fsys.(*corpusfs.LocalFS); !ok {
			t.Fatalf("New(kind=%q) = %T, want *corpusfs.LocalFS", kind, fsys)
		}
	}
}

// TestNew_LocalFSWalksRoot confirms the default backend actually discovers files
// under RootDir, so the local path is wired end to end.
func TestNew_LocalFSWalksRoot(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), []byte("world"))

	fsys, err := corpusfs.New(context.Background(), corpusfs.Config{RootDir: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	files, err := fsys.Walk(context.Background(), root, corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Walk returned %d files, want 2", len(files))
	}
}

// TestNew_S3KindReturnsS3FS exercises the s3 path with a stubbed client builder
// so no network or real credentials are required. It asserts the factory returns
// an *S3FS that lists the stub bucket.
func TestNew_S3KindReturnsS3FS(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{
		"corpus/a.txt":     []byte("a"),
		"corpus/sub/b.txt": []byte("bb"),
		"other/c.txt":      []byte("c"),
	}}

	var gotCfg corpusfs.Config
	restore := corpusfs.SetS3ClientBuilderForTest(func(_ context.Context, cfg corpusfs.Config) (corpusfs.S3API, error) {
		gotCfg = cfg
		return fake, nil
	})
	defer restore()

	fsys, err := corpusfs.New(context.Background(), corpusfs.Config{
		Kind:              "s3",
		StateDir:          t.TempDir(),
		S3Bucket:          "my-bucket",
		S3Prefix:          "corpus",
		S3Region:          "us-east-1",
		S3AccessKeyID:     "AKIAEXAMPLE",
		S3SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("New(s3): %v", err)
	}
	if _, ok := fsys.(*corpusfs.S3FS); !ok {
		t.Fatalf("New(s3) = %T, want *corpusfs.S3FS", fsys)
	}
	if gotCfg.S3Bucket != "my-bucket" || gotCfg.S3Region != "us-east-1" {
		t.Fatalf("builder received cfg %+v, want bucket/region propagated", gotCfg)
	}

	files, err := fsys.Walk(context.Background(), "", corpusfs.DefaultOptions())
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// Only the two objects under the "corpus/" prefix are in scope.
	if len(files) != 2 {
		t.Fatalf("Walk returned %d files, want 2 (prefix-scoped)", len(files))
	}
}

// TestNew_S3MissingBucket asserts the factory rejects an s3 source without a
// bucket before any client is built.
func TestNew_S3MissingBucket(t *testing.T) {
	restore := corpusfs.SetS3ClientBuilderForTest(func(_ context.Context, _ corpusfs.Config) (corpusfs.S3API, error) {
		t.Fatal("client builder must not be called when bucket is missing")
		return nil, nil
	})
	defer restore()

	_, err := corpusfs.New(context.Background(), corpusfs.Config{Kind: "s3"})
	if err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("New(s3, no bucket) err = %v, want a bucket-required error", err)
	}
}

// TestNew_UnknownKind asserts an unrecognized kind is a clear error.
func TestNew_UnknownKind(t *testing.T) {
	_, err := corpusfs.New(context.Background(), corpusfs.Config{Kind: "ftp"})
	if err == nil || !strings.Contains(err.Error(), "ftp") {
		t.Fatalf("New(kind=ftp) err = %v, want unknown-kind error", err)
	}
}
