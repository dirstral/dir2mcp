package tests

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
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
	if served := stub.bytesServed.Load(); served != cap682+1 {
		t.Errorf("Localize pulled %d bytes; want exactly %d (cap+1)", served, cap682+1)
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
	// limit+1: the reader hands over one byte past the cap and then fails. That
	// byte is what proves the object is past the cap rather than exactly at it.
	if len(got) != cap682+1 {
		t.Errorf("read delivered %d bytes before refusing, want the %d-byte cap plus the one probe byte", len(got), cap682)
	}
}

// TestS3FSOpen_ObjectOfExactlyTheCapReadsCleanly is the off-by-one guard on the
// limit+1 read, and it is the case a naive bound gets wrong. An object of exactly
// `ingest.max_file_mb` is INSIDE the policy: discovery admits it, so the reader must
// deliver all of it and end at io.EOF, not at ErrObjectTooLarge.
//
// It is asserted through both reader shapes, because they decide differently: the
// ranged reader compares against the length HEAD reported, while the streaming
// reader has no length and must read one byte past the cap to learn that the object
// ended there.
func TestS3FSOpen_ObjectOfExactlyTheCapReadsCleanly(t *testing.T) {
	const cap682 = 64 * 1024
	head := int64(cap682)

	for _, tc := range []struct {
		name     string
		headSize *int64
	}{
		{name: "reported length (ranged reader)", headSize: &head},
		{name: "no reported length (streaming reader)", headSize: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &lyingS3For682{key: "corpus/exact.bin", listSize: cap682, headSize: tc.headSize, bodySize: cap682}
			fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{
				Bucket: "bkt", Prefix: "corpus/", CacheDir: t.TempDir(), MaxBytes: cap682,
			})
			if err != nil {
				t.Fatalf("NewS3FS: %v", err)
			}
			rc, err := fsys.Open(context.Background(), "exact.bin")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = rc.Close() }()

			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read of an object exactly at the cap failed: %v", err)
			}
			if len(got) != cap682 {
				t.Errorf("read returned %d bytes, want the whole %d-byte object", len(got), cap682)
			}
		})
	}
}

// TestS3FSLocalize_ObjectOfExactlyTheCapDownloadsCleanly is the same off-by-one
// guard on the download path: an object exactly at the cap must localize.
func TestS3FSLocalize_ObjectOfExactlyTheCapDownloadsCleanly(t *testing.T) {
	const cap682 = 64 * 1024
	stub := &lyingS3For682{key: "corpus/exact.mp4", listSize: cap682, bodySize: cap682}
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{
		Bucket: "bkt", Prefix: "corpus/", CacheDir: t.TempDir(), MaxBytes: cap682,
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	localPath, cleanup, err := fsys.Localize(context.Background(), "exact.mp4")
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("Localize of an object exactly at the cap failed: %v", err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatalf("stat localized file: %v", err)
	}
	if info.Size() != cap682 {
		t.Errorf("localized file is %d bytes, want the whole %d-byte object", info.Size(), cap682)
	}
}

// TestS3FSCapClampsAnAbsurdValue pins the upper clamp on the configured cap. Every
// bound in the backend is a limit+1, so math.MaxInt64 would overflow that sum to a
// negative number, and io.LimitReader reads a negative limit as "no bytes allowed".
// The failure that produces is the dangerous kind: Localize writes an EMPTY file and
// reports success, so a caller gets a valid path to a truncated object. S3Config is
// exported, so the invariant is enforced in the backend rather than trusted to the
// caller.
func TestS3FSCapClampsAnAbsurdValue(t *testing.T) {
	const size = 4096
	head := int64(size)
	stub := &lyingS3For682{key: "corpus/small.bin", listSize: size, headSize: &head, bodySize: size}
	fsys, err := corpusfs.NewS3FS(stub, corpusfs.S3Config{
		Bucket: "bkt", Prefix: "corpus/", CacheDir: t.TempDir(), MaxBytes: math.MaxInt64,
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	localPath, cleanup, err := fsys.Localize(context.Background(), "small.bin")
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("Localize: %v", err)
	}
	info, statErr := os.Stat(localPath)
	if statErr != nil {
		t.Fatalf("stat localized file: %v", statErr)
	}
	if info.Size() != size {
		t.Errorf("localized file is %d bytes, want the whole %d-byte object", info.Size(), size)
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
	if served := stub.bytesServed.Load(); served != corpusfs.DefaultMaxFileSizeBytes()+1 {
		t.Errorf("Localize pulled %d bytes; want exactly %d (default cap+1)", served, corpusfs.DefaultMaxFileSizeBytes()+1)
	}
}
