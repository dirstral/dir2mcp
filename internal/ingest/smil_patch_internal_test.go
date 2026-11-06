package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestReplaceSMIL_RefusesASMILChangedSinceRead pins the re-check before a
// patch: when another tool rewrote the SMIL after it was read, replaceSMIL
// keeps that rewrite and writes no backup (SPEC §8.6.14 "every change is
// backed up first").
func TestReplaceSMIL_RefusesASMILChangedSinceRead(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "abc.smil")
	if err := os.WriteFile(abs, []byte("<smil>new</smil>"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := smilTarget{relPath: "abc.smil", abs: abs, data: []byte("<smil>old</smil>"), mode: 0o644}
	if err := replaceSMIL(target, []byte("<smil>patched</smil>")); !errors.Is(err, errSMILChanged) {
		t.Fatalf("want errSMILChanged, got %v", err)
	}
	if got, _ := os.ReadFile(abs); string(got) != "<smil>new</smil>" {
		t.Fatalf("the rewritten SMIL must be kept, got %q", got)
	}
	if m, _ := filepath.Glob(abs + ".bak.*"); len(m) != 0 {
		t.Fatalf("no backup may be written, got %v", m)
	}

	target.data = []byte("<smil>new</smil>")
	if err := replaceSMIL(target, []byte("<smil>patched</smil>")); err != nil {
		t.Fatalf("an unchanged SMIL must be patched: %v", err)
	}
	if got, _ := os.ReadFile(abs); string(got) != "<smil>patched</smil>" {
		t.Fatalf("patched SMIL = %q", got)
	}
	if m, _ := filepath.Glob(abs + ".bak.*"); len(m) != 1 {
		t.Fatalf("want one backup, got %v", m)
	}
}
