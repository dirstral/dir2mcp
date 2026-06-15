package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// TestS3FS_MediaURL_PresignsKeyedByPrefix verifies the MediaURLProvider
// capability (issue #243): S3FS.MediaURL presigns a URL for the full object key
// (prefix + relPath) and reports ok=true. The presigner is injected so the test
// stays network-free and asserts the exact (bucket, key) handed to it.
func TestS3FS_MediaURL_PresignsKeyedByPrefix(t *testing.T) {
	objects := map[string][]byte{"media/clip.mp4": []byte("DATA")}
	client := &fakeS3{objects: objects}

	var gotBucket, gotKey string
	presign := func(_ context.Context, bucket, key string) (string, error) {
		gotBucket, gotKey = bucket, key
		return "https://signed.example/" + key + "?sig=abc", nil
	}
	fsys, err := corpusfs.NewS3FSWithPresignForTest(client, corpusfs.S3Config{
		Bucket: "my-bucket", Prefix: "media",
	}, presign)
	if err != nil {
		t.Fatalf("NewS3FSWithPresignForTest: %v", err)
	}

	provider, ok := interface{}(fsys).(corpusfs.MediaURLProvider)
	if !ok {
		t.Fatal("*S3FS must implement corpusfs.MediaURLProvider")
	}
	url, ok, err := provider.MediaURL(context.Background(), "clip.mp4")
	if err != nil {
		t.Fatalf("MediaURL: %v", err)
	}
	if !ok {
		t.Fatal("MediaURL ok=false, want true (presigner configured)")
	}
	if gotBucket != "my-bucket" {
		t.Errorf("presigned bucket = %q, want my-bucket", gotBucket)
	}
	if gotKey != "media/clip.mp4" {
		t.Errorf("presigned key = %q, want media/clip.mp4", gotKey)
	}
	if !strings.HasPrefix(url, "https://signed.example/media/clip.mp4") {
		t.Errorf("url = %q, want the presigned URL", url)
	}
}

// TestS3FS_MediaURL_NoPresignerReportsNotOK confirms that an S3FS built without a
// presigner (e.g. a stub client that cannot mint a *s3.PresignClient) reports
// ok=false with no error so the caller falls back to Localize.
func TestS3FS_MediaURL_NoPresignerReportsNotOK(t *testing.T) {
	client := &fakeS3{objects: map[string][]byte{"clip.mp4": []byte("DATA")}}
	fsys, err := corpusfs.NewS3FS(client, corpusfs.S3Config{Bucket: "b"})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	provider, ok := interface{}(fsys).(corpusfs.MediaURLProvider)
	if !ok {
		t.Fatal("*S3FS must implement corpusfs.MediaURLProvider")
	}
	url, ok, err := provider.MediaURL(context.Background(), "clip.mp4")
	if err != nil {
		t.Fatalf("MediaURL error = %v, want nil", err)
	}
	if ok {
		t.Fatal("MediaURL ok=true, want false (no presigner configured)")
	}
	if url != "" {
		t.Errorf("url = %q, want empty", url)
	}
}

// TestS3FS_MediaURL_PresignErrorPropagates confirms a presigner failure surfaces
// as a non-nil error (ok=false) so the worker can classify it.
func TestS3FS_MediaURL_PresignErrorPropagates(t *testing.T) {
	client := &fakeS3{objects: map[string][]byte{"clip.mp4": []byte("DATA")}}
	presign := func(context.Context, string, string) (string, error) {
		return "", errors.New("boom")
	}
	fsys, err := corpusfs.NewS3FSWithPresignForTest(client, corpusfs.S3Config{Bucket: "b"}, presign)
	if err != nil {
		t.Fatalf("NewS3FSWithPresignForTest: %v", err)
	}
	provider := interface{}(fsys).(corpusfs.MediaURLProvider)
	if _, ok, err := provider.MediaURL(context.Background(), "clip.mp4"); err == nil || ok {
		t.Fatalf("MediaURL = (ok=%v, err=%v), want (false, non-nil)", ok, err)
	}
}

// TestLocalFS_NotMediaURLProvider pins that LocalFS does NOT implement
// MediaURLProvider, so a local-backed worker always uses the Localize path and
// its behavior is unchanged by issue #243.
func TestLocalFS_NotMediaURLProvider(t *testing.T) {
	fsys := corpusfs.NewLocalFS(t.TempDir())
	if _, ok := interface{}(fsys).(corpusfs.MediaURLProvider); ok {
		t.Fatal("LocalFS must NOT implement MediaURLProvider (no URL for a local file)")
	}
}
