package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// TestS3FSWalk_SkipsSMILBackups pins SPEC §7.1 (0.90.0) on the S3 backend: a
// SMIL caption-patch backup is never discovered, on every corpus source, while
// the SMIL itself is.
func TestS3FSWalk_SkipsSMILBackups(t *testing.T) {
	objs := map[string][]byte{
		"v/abc.smil":                       []byte("<smil/>"),
		"v/abc.smil.bak.20251106_120000":   []byte("<smil/>"),
		"v/abc.smil.bak.20251106_120000.2": []byte("<smil/>"),
	}
	fsys, _ := newFakeS3FS(t, "", objs, "")
	got, err := fsys.Walk(context.Background(), "", corpusfs.Options{})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 || got[0].RelPath != "v/abc.smil" {
		t.Fatalf("want only v/abc.smil discovered, got %+v", got)
	}
}
