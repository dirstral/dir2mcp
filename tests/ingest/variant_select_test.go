package tests

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// df builds a DiscoveredFile with the given relative path and size for the pure
// SelectMediaVariants unit tests (no filesystem involved).
func df(relPath string, size int64) ingest.DiscoveredFile {
	return ingest.DiscoveredFile{RelPath: relPath, SizeBytes: size}
}

func relPaths(files []ingest.DiscoveredFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.RelPath)
	}
	return out
}

func groupBest() ingest.MediaVariantOptions {
	return ingest.MediaVariantOptions{Group: true, Select: ingest.MediaVariantSelectBest}
}

func TestSelectMediaVariants_Disabled_ReturnsAll(t *testing.T) {
	in := []ingest.DiscoveredFile{
		df("clip.1080p.mp4", 30),
		df("clip.720p.mp4", 20),
		df("clip.480p.mp4", 10),
	}
	got := ingest.SelectMediaVariants(in, ingest.MediaVariantOptions{Group: false})
	if !slices.Equal(relPaths(got), relPaths(in)) {
		t.Fatalf("disabled grouping must return all files unchanged: %v", relPaths(got))
	}
}

func TestSelectMediaVariants_PicksHighestResolution(t *testing.T) {
	// Deliberately list largest-size on a lower resolution to prove resolution
	// wins over size in the "best" policy.
	in := []ingest.DiscoveredFile{
		df("clip.480p.mp4", 9999),
		df("clip.720p.mp4", 20),
		df("clip.1080p.mp4", 30),
	}
	got := ingest.SelectMediaVariants(in, groupBest())
	if !slices.Equal(relPaths(got), []string{"clip.1080p.mp4"}) {
		t.Fatalf("expected only the 1080p rendition, got %v", relPaths(got))
	}
}

func TestSelectMediaVariants_UnrelatedFilesUntouched(t *testing.T) {
	in := []ingest.DiscoveredFile{
		df("notes.txt", 5),
		df("clip.1080p.mp4", 30),
		df("clip.720p.mp4", 20),
		df("image.png", 7),
		df("report.pdf", 100),
		df("other.mp4", 50), // distinct media (no marker, different stem)
	}
	got := ingest.SelectMediaVariants(in, groupBest())
	want := []string{"notes.txt", "clip.1080p.mp4", "image.png", "report.pdf", "other.mp4"}
	if !slices.Equal(relPaths(got), want) {
		t.Fatalf("unrelated/non-media files must be preserved in order:\nwant=%v\ngot=%v", want, relPaths(got))
	}
}

func TestSelectMediaVariants_NoMarkerFilesAreDistinct(t *testing.T) {
	// Two media files without rendition markers normalize to their own paths and
	// must NOT be merged.
	in := []ingest.DiscoveredFile{
		df("intro.mp4", 10),
		df("outro.mp4", 10),
	}
	got := ingest.SelectMediaVariants(in, groupBest())
	if !slices.Equal(relPaths(got), []string{"intro.mp4", "outro.mp4"}) {
		t.Fatalf("no-marker media files must stay distinct, got %v", relPaths(got))
	}
}

func TestSelectMediaVariants_SizeTiebreak(t *testing.T) {
	// Same resolution => largest size wins.
	in := []ingest.DiscoveredFile{
		df("clip.1080p.h264.mp4", 50),
		df("clip.1080p.hevc.mp4", 80),
	}
	got := ingest.SelectMediaVariants(in, groupBest())
	if !slices.Equal(relPaths(got), []string{"clip.1080p.hevc.mp4"}) {
		t.Fatalf("expected largest same-resolution rendition, got %v", relPaths(got))
	}
}

func TestSelectMediaVariants_LexicalTiebreak_Stable(t *testing.T) {
	// Same resolution and same size => lexically-lowest path wins, regardless of
	// input order. The two files share a normalized name (codec markers differ
	// but are stripped) and have equal size, so only the lexical tiebreak decides.
	aac := df("clip.1080p.aac.mp4", 100)
	ac3 := df("clip.1080p.ac3.mp4", 100)

	got1 := ingest.SelectMediaVariants([]ingest.DiscoveredFile{aac, ac3}, groupBest())
	got2 := ingest.SelectMediaVariants([]ingest.DiscoveredFile{ac3, aac}, groupBest())
	if !slices.Equal(relPaths(got1), []string{"clip.1080p.aac.mp4"}) ||
		!slices.Equal(relPaths(got2), []string{"clip.1080p.aac.mp4"}) {
		t.Fatalf("lexical tiebreak must be stable across input order: %v / %v", relPaths(got1), relPaths(got2))
	}
}

func TestSelectMediaVariants_FirstPolicy(t *testing.T) {
	in := []ingest.DiscoveredFile{
		df("clip.1080p.mp4", 9999),
		df("clip.480p.mp4", 1),
		df("clip.720p.mp4", 5),
	}
	got := ingest.SelectMediaVariants(in, ingest.MediaVariantOptions{Group: true, Select: ingest.MediaVariantSelectFirst})
	// "first" ignores resolution/size and takes lexically-lowest path.
	if !slices.Equal(relPaths(got), []string{"clip.1080p.mp4"}) {
		t.Fatalf("first policy must pick lexically-lowest path, got %v", relPaths(got))
	}
}

func TestSelectMediaVariants_Deterministic_AcrossOrderings(t *testing.T) {
	orderA := []ingest.DiscoveredFile{
		df("a/clip.480p.mp4", 10),
		df("a/clip.1080p.mp4", 30),
		df("a/clip.720p.mp4", 20),
		df("b/song.128k.mp3", 5),
		df("b/song.320k.mp3", 12),
	}
	orderB := []ingest.DiscoveredFile{
		df("b/song.320k.mp3", 12),
		df("a/clip.720p.mp4", 20),
		df("b/song.128k.mp3", 5),
		df("a/clip.1080p.mp4", 30),
		df("a/clip.480p.mp4", 10),
	}
	gotA := relPaths(ingest.SelectMediaVariants(orderA, groupBest()))
	gotB := relPaths(ingest.SelectMediaVariants(orderB, groupBest()))
	slices.Sort(gotA)
	slices.Sort(gotB)
	want := []string{"a/clip.1080p.mp4", "b/song.320k.mp3"}
	if !slices.Equal(gotA, want) || !slices.Equal(gotB, want) {
		t.Fatalf("selection must be deterministic regardless of order:\nwant=%v\ngotA=%v\ngotB=%v", want, gotA, gotB)
	}
}

func TestSelectMediaVariants_DirScoped(t *testing.T) {
	// Same filename in different directories are distinct logical media.
	in := []ingest.DiscoveredFile{
		df("ep1/clip.1080p.mp4", 30),
		df("ep1/clip.720p.mp4", 20),
		df("ep2/clip.1080p.mp4", 31),
		df("ep2/clip.720p.mp4", 21),
	}
	got := relPaths(ingest.SelectMediaVariants(in, groupBest()))
	slices.Sort(got)
	want := []string{"ep1/clip.1080p.mp4", "ep2/clip.1080p.mp4"}
	if !slices.Equal(got, want) {
		t.Fatalf("variant grouping must be directory-scoped:\nwant=%v\ngot=%v", want, got)
	}
}

// TestDiscoverFilesWithOptions_VariantDedup exercises the full discovery path
// (walk + dedup) against a real temp directory, including the size tiebreak.
func TestDiscoverFilesWithOptions_VariantDedup(t *testing.T) {
	root := t.TempDir()
	// Three renditions of one logical clip; sizes are irrelevant because
	// resolution decides, but make the highest-res file the smallest to prove it.
	mustWriteFile(t, filepath.Join(root, "clip.1080p.mp4"), []byte("hi"))
	mustWriteFile(t, filepath.Join(root, "clip.720p.mp4"), []byte("medium-size"))
	mustWriteFile(t, filepath.Join(root, "clip.480p.mp4"), []byte("the-largest-bytes-here"))
	// Unrelated assets.
	mustWriteFile(t, filepath.Join(root, "readme.txt"), []byte("hello"))
	mustWriteFile(t, filepath.Join(root, "other.mp4"), []byte("distinct"))

	opts := ingest.DiscoverOptions{
		MaxSizeBytes:  1024,
		MediaVariants: groupBest(),
	}
	files, err := ingest.DiscoverFilesWithOptions(context.Background(), root, opts)
	if err != nil {
		t.Fatalf("DiscoverFilesWithOptions failed: %v", err)
	}
	got := relPaths(files)
	slices.Sort(got)
	want := []string{"clip.1080p.mp4", "other.mp4", "readme.txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("end-to-end variant dedup mismatch:\nwant=%v\ngot=%v", want, got)
	}

	// Disabled => all renditions returned.
	optsOff := ingest.DiscoverOptions{MaxSizeBytes: 1024}
	allFiles, err := ingest.DiscoverFilesWithOptions(context.Background(), root, optsOff)
	if err != nil {
		t.Fatalf("DiscoverFilesWithOptions (disabled) failed: %v", err)
	}
	if len(allFiles) != 5 {
		t.Fatalf("disabled grouping must return all 5 files, got %d: %v", len(allFiles), relPaths(allFiles))
	}
}

func TestMediaVariantOptionsFromConfig(t *testing.T) {
	cfg := config.Default()
	// Default config: grouping disabled, select defaults to best.
	if got := ingest.MediaVariantOptionsFromConfig(cfg); got.Group || got.Select != ingest.MediaVariantSelectBest {
		t.Fatalf("default config must disable grouping and default to best: %+v", got)
	}

	cfg.MediaVariantsGroup = true
	cfg.MediaVariantsSelect = "first"
	if got := ingest.MediaVariantOptionsFromConfig(cfg); !got.Group || got.Select != ingest.MediaVariantSelectFirst {
		t.Fatalf("config first/group must map through: %+v", got)
	}

	// Unknown/empty select falls back to best.
	cfg.MediaVariantsSelect = "bogus"
	if got := ingest.MediaVariantOptionsFromConfig(cfg); got.Select != ingest.MediaVariantSelectBest {
		t.Fatalf("unknown select must fall back to best: %+v", got)
	}
}
