package tests

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// Cap wiring and the sentinel (dir2mcp #682).
//
// The tests in s3_localize_bound_682_test.go prove the bound exists using only the
// API `main` already has, so they run against `main` and fail there. These name the
// new surface (S3Config.MaxBytes, corpusfs.ErrObjectTooLarge), so they cannot
// compile against `main`; they pin the parts a caller depends on: the operator's
// configured cap is the one enforced, and the refusal is a sentinel a caller can
// classify instead of a string it has to match.

// TestS3FSLocalize_HonorsTheConfiguredCap pins that the enforced bound is the
// operator's `ingest.max_file_mb`, not a hard-coded default. The cap is set well
// below the default so a passing test cannot be explained by the default.
func TestS3FSLocalize_HonorsTheConfiguredCap(t *testing.T) {
	const cap682 = 64 * 1024
	stub := &lyingS3For682{key: "corpus/clip.mp4", listSize: 512, bodySize: 4 * cap682}
	cacheDir := t.TempDir()
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{
		Bucket: "bkt", Prefix: "corpus/", CacheDir: cacheDir, MaxBytes: cap682,
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	_, cleanup, err := fsys.Localize(context.Background(), "clip.mp4")
	if cleanup != nil {
		cleanup()
	}
	if !errors.Is(err, corpusfs.ErrObjectTooLarge) {
		t.Fatalf("Localize error = %v, want ErrObjectTooLarge", err)
	}
	if served := stub.bytesServed.Load(); served > cap682+1 {
		t.Errorf("Localize pulled %d bytes; the bound is %d (cap+1)", served, cap682+1)
	}
	if left := cacheDirBytes682(t, cacheDir); left != 0 {
		t.Errorf("cache dir still holds %d bytes after the refused download", left)
	}
}

// TestS3FSOpen_RefusalCarriesTheSentinel pins that an over-cap read fails with
// ErrObjectTooLarge rather than with io.EOF. A silent short read is the worse
// outcome of the two: every caller would treat the prefix as a complete file, which
// is the truncated-document failure #487 already fixed once.
func TestS3FSOpen_RefusalCarriesTheSentinel(t *testing.T) {
	const cap682 = 64 * 1024
	head := int64(4 * cap682)
	stub := &lyingS3For682{key: "corpus/notes.txt", listSize: 512, headSize: &head, bodySize: 4 * cap682}
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{
		Bucket: "bkt", Prefix: "corpus/", CacheDir: t.TempDir(), MaxBytes: cap682,
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	rc, err := fsys.Open(context.Background(), "notes.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	got, readErr := io.ReadAll(rc)
	if !errors.Is(readErr, corpusfs.ErrObjectTooLarge) {
		t.Fatalf("read error = %v, want ErrObjectTooLarge", readErr)
	}
	if len(got) != cap682 {
		t.Errorf("read delivered %d bytes before refusing, want exactly the %d-byte cap", len(got), cap682)
	}
}

// TestS3FSCapDefaultsWhenUnset pins that an unset (or zero) MaxBytes resolves to
// the 10 MiB default rather than to "unbounded". A caller that forgets the field
// must still get a bound; there is deliberately no unlimited setting.
func TestS3FSCapDefaultsWhenUnset(t *testing.T) {
	stub := &lyingS3For682{key: "corpus/big.bin", listSize: listedBytes682, bodySize: bodyBytes682}
	cacheDir := t.TempDir()
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{
		Bucket: "bkt", Prefix: "corpus/", CacheDir: cacheDir, MaxBytes: 0,
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	_, cleanup, err := fsys.Localize(context.Background(), "big.bin")
	if cleanup != nil {
		cleanup()
	}
	if !errors.Is(err, corpusfs.ErrObjectTooLarge) {
		t.Fatalf("Localize error = %v, want ErrObjectTooLarge under the default cap", err)
	}
	if served := stub.bytesServed.Load(); served > corpusfs.DefaultMaxFileSizeBytes()+1 {
		t.Errorf("Localize pulled %d bytes; the default bound is %d (cap+1)", served, corpusfs.DefaultMaxFileSizeBytes()+1)
	}
}
