package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// #728: the disk backend persisted the segment and its embed-identity sidecar as
// two separate commits. Save renamed the compacted segment, marked the index
// clean, and only then wrote the sidecar; a sidecar failure therefore returned
// an error with the index ALREADY clean, so the `version == savedVersion` gate
// made every later Save a no-op and the sidecar was never retried. On the next
// startup Load mapped the missing sidecar to "", EnsureIdentity read that as a
// mismatch, and Reset discarded the whole segment: an ordinary filesystem error
// at a narrow boundary cost a full re-embed.

// diskIndexAt builds an index over a caller-chosen path, so a test can reach
// the segment and its sidecar on disk. (newDiskIndex in disk_index_test.go
// hides the path inside a fresh TempDir.)
func diskIndexAt(t *testing.T, dir string) (*diskindex.DiskIndex, string) {
	t.Helper()
	path := filepath.Join(dir, diskindex.SegmentFileName("text"))
	idx := diskindex.New(path)
	t.Cleanup(func() { _ = idx.Close() })
	return idx, path
}

func upsert728(t *testing.T, idx *diskindex.DiskIndex, chunkID uint64, v []float32) {
	t.Helper()
	if err := idx.Upsert(context.Background(), v, model.IndexPayload{
		ChunkID: chunkID, RelPath: "a.md", DocType: "md",
	}); err != nil {
		t.Fatalf("Upsert(%d): %v", chunkID, err)
	}
}

// TestSaveDoesNotMarkTheIndexCleanUntilTheIdentityIsDurable is the reported
// defect. With the sidecar path unwritable, Save must fail AND leave the index
// dirty, so that a later Save retries rather than returning early at the
// version gate having never persisted the identity.
func TestSaveDoesNotMarkTheIndexCleanUntilTheIdentityIsDurable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx, path := diskIndexAt(t, dir)
	upsert728(t, idx, 1, []float32{1, 0})

	// Make the sidecar unwritable by occupying its name with a directory: the
	// staged write cannot be created and the rename cannot land.
	sidecar := path + ".identity.json"
	if err := os.MkdirAll(sidecar+".tmp", 0o700); err != nil {
		t.Fatalf("seed obstruction: %v", err)
	}

	if err := idx.Save(ctx, path); err == nil {
		t.Fatal("Save succeeded despite an unwritable identity sidecar")
	}

	// The retry is the point: before the fix this returned nil immediately,
	// because savedVersion had already been advanced by the failed Save.
	if err := os.RemoveAll(sidecar + ".tmp"); err != nil {
		t.Fatalf("clear obstruction: %v", err)
	}
	if err := idx.Save(ctx, path); err != nil {
		t.Fatalf("second Save did not retry cleanly: %v", err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("identity sidecar still absent after a successful Save: %v", err)
	}
}

// TestAnIndexSurvivesRestartAfterAFailedSave closes the loop the issue
// describes. It is not enough that Save reports an error: the vectors have to
// still be there after the restart, and the thing that destroyed them was
// EnsureIdentity, which reads a missing identity as a mismatch and calls Reset.
// So the test goes through EnsureIdentity rather than Load alone.
func TestAnIndexSurvivesRestartAfterAFailedSave(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx, path := diskIndexAt(t, dir)
	const identity = "openai/text-embedding-3-small@1536"
	if err := idx.Reset(ctx, identity); err != nil {
		t.Fatalf("Reset(initial identity): %v", err)
	}
	upsert728(t, idx, 1, []float32{1, 0})
	upsert728(t, idx, 2, []float32{0, 1})

	// A save whose identity commit cannot land.
	sidecar := path + ".identity.json"
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("clear the initial sidecar: %v", err)
	}
	if err := os.MkdirAll(sidecar+".tmp", 0o700); err != nil {
		t.Fatalf("seed obstruction: %v", err)
	}
	if err := idx.Save(ctx, path); err == nil {
		t.Fatal("Save succeeded despite an unwritable identity sidecar")
	}
	if err := os.RemoveAll(sidecar + ".tmp"); err != nil {
		t.Fatalf("clear obstruction: %v", err)
	}
	// The retry that the version gate used to prevent.
	if err := idx.Save(ctx, path); err != nil {
		t.Fatalf("retry Save: %v", err)
	}

	reopened := diskindex.New(path)
	defer func() { _ = reopened.Close() }()
	if err := reopened.Load(ctx, path); err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	// This is the step that used to discard everything.
	if err := index.EnsureIdentity(ctx, reopened, identity); err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	hits, err := reopened.Search(ctx, []float32{1, 0}, 2, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("restart kept %d vectors, want 2: EnsureIdentity reset the segment", len(hits))
	}
}

// TestADamagedIdentitySidecarIsReportedNotSilentlyReset pins the Load half.
// readIdentitySidecar used to map a corrupt sidecar to "" exactly like a
// missing one, so EnsureIdentity saw a mismatch and Reset threw the segment
// away. Losing an index because a small JSON file went bad is not a recovery.
func TestADamagedIdentitySidecarIsReportedNotSilentlyReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx, path := diskIndexAt(t, dir)
	upsert728(t, idx, 1, []float32{1, 0})
	if err := idx.Save(ctx, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	sidecar := path + ".identity.json"
	if err := os.WriteFile(sidecar, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupt the sidecar: %v", err)
	}

	reopened := diskindex.New(path)
	defer func() { _ = reopened.Close() }()
	err := reopened.Load(ctx, path)
	if err == nil {
		t.Fatal("Load accepted a damaged identity sidecar; EnsureIdentity would then reset the segment")
	}
	// The message has to tell an operator what to do about it.
	if !strings.Contains(err.Error(), "reindex") {
		t.Fatalf("error does not name the recovery: %v", err)
	}
}

// TestAMissingSidecarOverAPopulatedSegmentIsNotDamage guards the correction I
// had to make while writing this. Appends are durable immediately while the
// sidecar is only written by Save and Reset, so EVERY index between its first
// append and its first save legitimately has vectors and no sidecar. Failing
// that state would break ordinary operation, not protect it.
func TestAMissingSidecarOverAPopulatedSegmentIsNotDamage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx, path := diskIndexAt(t, dir)
	upsert728(t, idx, 1, []float32{1, 0})

	if _, err := os.Stat(path + ".identity.json"); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar before the first Save, stat err = %v", err)
	}
	reopened := diskindex.New(path)
	defer func() { _ = reopened.Close() }()
	if err := reopened.Load(ctx, path); err != nil {
		t.Fatalf("Load of an appended-but-unsaved segment must succeed: %v", err)
	}
	hits, err := reopened.Search(ctx, []float32{1, 0}, 1, model.Filter{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search after load = %v, %v; the durable append was lost", hits, err)
	}
}

// TestResetDoesNotDestroyTheSegmentWhenTheIdentityCannotBeCommitted: Reset used
// to truncate FIRST and write the identity second, so a failure in between left
// an emptied segment whose recorded identity still described the vectors that
// were just deleted. Staging the identity before the destructive step means a
// failure leaves the previous generation intact.
func TestResetDoesNotDestroyTheSegmentWhenTheIdentityCannotBeCommitted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx, path := diskIndexAt(t, dir)
	const identity = "openai/text-embedding-3-small@1536"
	if err := idx.Reset(ctx, identity); err != nil {
		t.Fatalf("Reset(initial): %v", err)
	}
	upsert728(t, idx, 1, []float32{1, 0})
	upsert728(t, idx, 2, []float32{0, 1})
	if err := idx.Save(ctx, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Make the identity commit impossible, then ask for a reset.
	sidecar := path + ".identity.json"
	if err := os.MkdirAll(sidecar+".tmp", 0o700); err != nil {
		t.Fatalf("seed obstruction: %v", err)
	}
	if err := idx.Reset(ctx, "some-other-identity"); err == nil {
		t.Fatal("Reset succeeded despite an uncommittable identity")
	}
	if err := os.RemoveAll(sidecar + ".tmp"); err != nil {
		t.Fatalf("clear obstruction: %v", err)
	}

	// The segment on disk must still be the committed generation, not an empty
	// file left behind by a half-done reset.
	reopened := diskindex.New(path)
	defer func() { _ = reopened.Close() }()
	if err := reopened.Load(ctx, path); err != nil {
		t.Fatalf("Load after the failed Reset: %v", err)
	}
	hits, err := reopened.Search(ctx, []float32{1, 0}, 2, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("a failed Reset destroyed the committed segment: %d vectors survive, want 2", len(hits))
	}
	recorded, err := reopened.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if recorded != identity {
		t.Fatalf("recorded identity = %q after a failed Reset, want the previous %q", recorded, identity)
	}
}
