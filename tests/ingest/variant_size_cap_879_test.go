package tests

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #879: `ingest.max_file_mb` used to run BEFORE §8.6.5 variant grouping,
// inside the walker. Every rendition over the cap was dropped as a size_cap skip
// first, so `media.variants.select: best` chose the best of the leftovers and an
// operator who asked for the best rendition silently got the worst one.
//
// Grouping now runs first, over every rendition that exists, and the cap applies
// inside the group. Two things follow, and both are pinned below:
//
//  1. selection is made out of the renditions that fit the cap, and the group
//     reports which rendition it ended on instead of deciding it by accident;
//  2. a rendition grouping discards leaves NO document row, whichever side of
//     the cap it fell. That is the bookkeeping half of the bug: the discarded
//     renditions used to appear as size_cap rows under the cap and not at all
//     without it, so the same corpus described itself two different ways.

// oc builds an over-cap grouping candidate for the pure unit tests.
func oc(relPath string, size int64) ingest.OversizeCandidate {
	return ingest.OversizeCandidate{RelPath: relPath, SizeBytes: size}
}

func oversizePaths(candidates []ingest.OversizeCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.RelPath)
	}
	return out
}

// TestVariantCap879_BestOverCapKeepsTheBestRenditionThatFits is the core case
// from the issue: 1080p/720p/480p are over the cap and 360p fits. The group must
// end on the one rendition that fits, and the three discarded renditions must
// leave no size_cap rows behind. On main the over-cap renditions never reached
// grouping at all, so they each produced a row.
func TestVariantCap879_BestOverCapKeepsTheBestRenditionThatFits(t *testing.T) {
	t.Parallel()
	fits := []ingest.DiscoveredFile{df("ep1_360p.mp4", 500)}
	over := []ingest.OversizeCandidate{
		oc("ep1_1080p.mp4", 65000),
		oc("ep1_720p.mp4", 40000),
		oc("ep1_480p.mp4", 20000),
	}

	got := ingest.SelectMediaVariantsWithCap(fits, over, groupBest())

	if !slices.Equal(relPaths(got.Files), []string{"ep1_360p.mp4"}) {
		t.Fatalf("expected the only rendition under the cap to be ingested, got %v", relPaths(got.Files))
	}
	if len(got.Oversize) != 0 {
		t.Errorf("a rendition grouping discards must not keep a size_cap row, got %v", oversizePaths(got.Oversize))
	}
	if len(got.Interactions) != 1 {
		t.Fatalf("expected one reported group, got %+v", got.Interactions)
	}
	want := ingest.VariantCapInteraction{Canonical: "ep1_360p.mp4", OverCap: 3, Indexed: true}
	if got.Interactions[0] != want {
		t.Errorf("interaction = %+v, want %+v", got.Interactions[0], want)
	}
}

// TestVariantCap879_AllRenditionsFit is the regression guard for the state the
// operator reached by raising the cap: with nothing over the cap the best
// rendition wins and no interaction is reported.
func TestVariantCap879_AllRenditionsFit(t *testing.T) {
	t.Parallel()
	fits := []ingest.DiscoveredFile{
		df("ep1_360p.mp4", 500),
		df("ep1_1080p.mp4", 65000),
	}

	got := ingest.SelectMediaVariantsWithCap(fits, nil, groupBest())

	if !slices.Equal(relPaths(got.Files), []string{"ep1_1080p.mp4"}) {
		t.Fatalf("expected the best rendition, got %v", relPaths(got.Files))
	}
	if len(got.Interactions) != 0 {
		t.Errorf("the cap changed nothing here, so nothing must be reported: %+v", got.Interactions)
	}
}

// TestVariantCap879_NoRenditionFits pins the decision for a group the cap
// excludes entirely: the whole group is skipped as size_cap, recorded ONCE on
// the rendition the policy would have chosen. One unindexable media is one
// coverage gap, not five.
func TestVariantCap879_NoRenditionFits(t *testing.T) {
	t.Parallel()
	over := []ingest.OversizeCandidate{
		oc("ep1_480p.mp4", 20000),
		oc("ep1_1080p.mp4", 65000),
		oc("ep1_720p.mp4", 40000),
	}

	got := ingest.SelectMediaVariantsWithCap(nil, over, groupBest())

	if len(got.Files) != 0 {
		t.Errorf("no rendition fits the cap, so nothing may be ingested: %v", relPaths(got.Files))
	}
	if !slices.Equal(oversizePaths(got.Oversize), []string{"ep1_1080p.mp4"}) {
		t.Fatalf("expected one size_cap row on the rendition the policy would have chosen, got %v", oversizePaths(got.Oversize))
	}
	want := ingest.VariantCapInteraction{Canonical: "ep1_1080p.mp4", OverCap: 3, Indexed: false}
	if len(got.Interactions) != 1 || got.Interactions[0] != want {
		t.Errorf("interactions = %+v, want exactly %+v", got.Interactions, want)
	}
}

// TestVariantCap879_GroupingOffPassesEverythingThrough pins the default. With
// `media.variants.group: false` the files and the size-cap drops are returned
// exactly as they arrived, so every deployment that never opted in is untouched.
func TestVariantCap879_GroupingOffPassesEverythingThrough(t *testing.T) {
	t.Parallel()
	files := []ingest.DiscoveredFile{df("ep1_360p.mp4", 500), df("notes.txt", 10)}
	over := []ingest.OversizeCandidate{oc("ep1_1080p.mp4", 65000), oc("ep1_720p.mp4", 40000)}

	got := ingest.SelectMediaVariantsWithCap(files, over, ingest.MediaVariantOptions{Group: false})

	if !slices.Equal(relPaths(got.Files), relPaths(files)) {
		t.Errorf("files = %v, want them unchanged %v", relPaths(got.Files), relPaths(files))
	}
	if !slices.Equal(oversizePaths(got.Oversize), oversizePaths(over)) {
		t.Errorf("size-cap drops = %v, want them unchanged %v", oversizePaths(got.Oversize), oversizePaths(over))
	}
	if len(got.Interactions) != 0 {
		t.Errorf("grouping is off, so no group can be reported: %+v", got.Interactions)
	}
}

// TestVariantCap879_NonMediaAndLoneMediaKeepTheirRows guards the two cases the
// change must not touch: an over-cap file that is not media at all, and a lone
// over-cap media file with no siblings. Both keep their ordinary size_cap row,
// and neither is an interaction between two settings.
func TestVariantCap879_NonMediaAndLoneMediaKeepTheirRows(t *testing.T) {
	t.Parallel()
	over := []ingest.OversizeCandidate{oc("archive.zip", 90000), oc("lecture.mp4", 70000)}

	got := ingest.SelectMediaVariantsWithCap(nil, over, groupBest())

	if !slices.Equal(oversizePaths(got.Oversize), []string{"archive.zip", "lecture.mp4"}) {
		t.Errorf("size-cap drops = %v, want both kept", oversizePaths(got.Oversize))
	}
	if len(got.Interactions) != 0 {
		t.Errorf("neither file is a multi-rendition group: %+v", got.Interactions)
	}
}

// TestVariantCap879_NoReportWhenTheCapChangedNothing keeps the operator warning
// honest. Under `select: first` the lexically-lowest rendition wins; here it
// fits the cap, so the excluded sibling would have lost anyway. The sibling
// still leaves no row (grouping discarded it), but nothing is reported: a
// warning that fires when the cap changed nothing is noise.
func TestVariantCap879_NoReportWhenTheCapChangedNothing(t *testing.T) {
	t.Parallel()
	opts := ingest.MediaVariantOptions{Group: true, Select: ingest.MediaVariantSelectFirst}
	fits := []ingest.DiscoveredFile{df("ep1_1080p.mp4", 500)}
	over := []ingest.OversizeCandidate{oc("ep1_720p.mp4", 65000)}

	got := ingest.SelectMediaVariantsWithCap(fits, over, opts)

	if !slices.Equal(relPaths(got.Files), []string{"ep1_1080p.mp4"}) {
		t.Fatalf("expected the lexically-lowest rendition, got %v", relPaths(got.Files))
	}
	if len(got.Oversize) != 0 {
		t.Errorf("the discarded rendition must not keep a size_cap row, got %v", oversizePaths(got.Oversize))
	}
	if len(got.Interactions) != 0 {
		t.Errorf("the cap did not change the choice, so nothing must be reported: %+v", got.Interactions)
	}
}

// renditionSizes879 is the archive shape from the issue: one episode in four
// renditions, only the smallest of which fits a 1 MiB cap.
var renditionSizes879 = map[string]int{
	"ep1_1080p.mp4": 3 * 1024 * 1024,
	"ep1_720p.mp4":  2 * 1024 * 1024,
	"ep1_480p.mp4":  1024*1024 + 512*1024,
	"ep1_360p.mp4":  256 * 1024,
}

// writeRenditions879 writes the four renditions plus one bare-stem subtitle
// sidecar, so whichever rendition survives is transcribed from the sidecar and
// no STT provider is needed.
func writeRenditions879(t *testing.T, root string) {
	t.Helper()
	for rel, size := range renditionSizes879 {
		writeCorpusFile(t, root, rel, bytes.Repeat([]byte("v"), size))
	}
	writeCorpusFile(t, root, "ep1.ru.vtt", []byte(vtt876("Вторая волна")))
}

// scan879 is one finished scan: the store it wrote, the discovery log, the run
// counters and the §8.6.11 run manifest.
type scan879 struct {
	store    *store.SQLiteStore
	log      string
	counters appstate.IndexingSnapshot
	manifest []map[string]any
}

// runScan879 runs one full scan over root. The run manifest is enabled on every
// scan, so the FILE_TOO_LARGE entries (§14.4) the same size-cap drops feed are
// observable beside the document rows.
func runScan879(t *testing.T, root string, capMB int, group bool) scan879 {
	t.Helper()
	// getLogger falls back to log.Default() when no per-service logger is set,
	// so redirect the process logger to observe the discovery lines. Mutates
	// global state; these tests must not be parallelised.
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	ctx := context.Background()
	stateDir := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	manifestPath := filepath.Join(stateDir, "run.jsonl")
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.STTProvider = "off"
	cfg.IngestMaxFileMB = capMB
	cfg.MediaVariantsGroup = group
	cfg.MediaVariantsSelect = "best"
	cfg.MediaBatchManifest = manifestPath

	svc := mustNewIngestService(t, cfg, st)
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return scan879{
		store:    st,
		log:      logBuf.String(),
		counters: state.Snapshot(),
		manifest: readManifest(t, manifestPath),
	}
}

// logLine879 returns the one discovery line that contains every wanted
// fragment. Matching the fragments against the whole buffer would pass on
// fragments spread over several unrelated lines.
func logLine879(t *testing.T, logged string, want ...string) string {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		hit := true
		for _, w := range want {
			if !strings.Contains(line, w) {
				hit = false
				break
			}
		}
		if hit {
			return line
		}
	}
	t.Errorf("no single log line contains %v; got:\n%s", want, logged)
	return ""
}

// manifestCodes879 returns the manifest error codes recorded for a rel_path.
func manifestCodes879(records []map[string]any, relPath string) []string {
	out := []string{}
	for _, rec := range records {
		if rel, _ := rec["rel_path"].(string); rel != relPath {
			continue
		}
		code, _ := rec["error_code"].(string)
		out = append(out, code)
	}
	return out
}

// assertNoRow879 asserts that a rendition grouping discarded left no document
// row at all.
func assertNoRow879(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	if doc, err := st.GetDocumentByPath(context.Background(), relPath); err == nil {
		t.Errorf("%s must not be a document row (grouping discarded it), got status=%q skip_reason=%q",
			relPath, doc.Status, doc.SkipReason)
	}
}

// TestVariantCap879_ScanIndexesTheRenditionItReports is the end-to-end half of
// the issue: with grouping on and only the 360p rendition under the cap, the
// corpus ingests that rendition, says so, and records nothing for the three
// renditions grouping discarded. On main the same corpus wrote three size_cap
// rows and reported nothing.
func TestVariantCap879_ScanIndexesTheRenditionItReports(t *testing.T) {
	root := t.TempDir()
	writeRenditions879(t, root)

	scan := runScan879(t, root, 1, true)

	doc, err := scan.store.GetDocumentByPath(context.Background(), "ep1_360p.mp4")
	if err != nil {
		t.Fatalf("the rendition under the cap must be ingested: %v", err)
	}
	if doc.Status != "ok" || doc.SkipReason != "" {
		t.Errorf("ep1_360p.mp4 status = %q skip_reason = %q, want status=ok with no skip reason", doc.Status, doc.SkipReason)
	}
	for _, rel := range []string{"ep1_1080p.mp4", "ep1_720p.mp4", "ep1_480p.mp4"} {
		assertNoRow879(t, scan.store, rel)
	}

	stats, err := scan.store.CorpusStats(context.Background())
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary != nil && stats.SkipSummary.Categories[model.SkipReasonSizeCap] != 0 {
		t.Errorf("size_cap skips = %d, want 0: the group ended on a rendition that fits",
			stats.SkipSummary.Categories[model.SkipReasonSizeCap])
	}

	// A discarded rendition is not a skip of this run either: it is not counted,
	// and it produces no FILE_TOO_LARGE manifest entry (§14.4).
	// scanned counts the surviving rendition and the sidecar file, and nothing
	// else: a rendition grouping discarded is not a file this run judged. (The
	// sidecar is a skipped row of its own, for binary_ignored, so `skipped` is
	// not the counter to read here; the size_cap total above is.)
	if scan.counters.Scanned != 2 {
		t.Errorf("scanned = %d, want 2 (the chosen rendition and the sidecar); a discarded rendition must not be counted",
			scan.counters.Scanned)
	}
	for _, rel := range []string{"ep1_1080p.mp4", "ep1_720p.mp4", "ep1_480p.mp4"} {
		if codes := manifestCodes879(scan.manifest, rel); len(codes) != 0 {
			t.Errorf("%s has manifest records %v, want none (grouping discarded it)", rel, codes)
		}
	}

	// The choice must be explicit, not accidental, and it must be ONE line.
	logLine879(t, scan.log, "media.variants.select=best", "ep1_360p.mp4", "ingest.max_file_mb")
}

// TestVariantCap879_ScanKeepsTheBestRenditionWhenAllFit is the regression guard
// for the state the operator reached by raising the cap: the 1080p rendition is
// the document, and no rendition leaves a stray row.
func TestVariantCap879_ScanKeepsTheBestRenditionWhenAllFit(t *testing.T) {
	root := t.TempDir()
	writeRenditions879(t, root)

	scan := runScan879(t, root, 100, true)

	doc, err := scan.store.GetDocumentByPath(context.Background(), "ep1_1080p.mp4")
	if err != nil {
		t.Fatalf("the best rendition must be ingested: %v", err)
	}
	if doc.Status != "ok" || doc.SkipReason != "" {
		t.Errorf("ep1_1080p.mp4 status = %q skip_reason = %q, want status=ok with no skip reason", doc.Status, doc.SkipReason)
	}
	for _, rel := range []string{"ep1_720p.mp4", "ep1_480p.mp4", "ep1_360p.mp4"} {
		assertNoRow879(t, scan.store, rel)
	}
	if strings.Contains(scan.log, "media.variants.select=") {
		t.Errorf("the cap excluded nothing, so no group may be reported; got: %q", scan.log)
	}
}

// TestVariantCap879_ScanWithGroupingOffIsUnchanged pins the default deployment:
// with `media.variants.group: false` every rendition is judged on its own and
// the three over-cap renditions keep their size_cap rows, exactly as before.
func TestVariantCap879_ScanWithGroupingOffIsUnchanged(t *testing.T) {
	root := t.TempDir()
	writeRenditions879(t, root)

	scan := runScan879(t, root, 1, false)

	for _, rel := range []string{"ep1_1080p.mp4", "ep1_720p.mp4", "ep1_480p.mp4"} {
		assertSkipReason(t, context.Background(), scan.store, rel, "skipped", model.SkipReasonSizeCap)
		// Each drop is still counted, logged and reported to the manifest.
		logLine879(t, scan.log, "discovery: skipping "+rel, "ingest.max_file_mb")
		if codes := manifestCodes879(scan.manifest, rel); len(codes) != 1 || codes[0] != "FILE_TOO_LARGE" {
			t.Errorf("%s manifest records = %v, want one FILE_TOO_LARGE", rel, codes)
		}
	}
	if scan.counters.Skipped < 3 {
		t.Errorf("skipped = %d, want at least the 3 over-cap renditions", scan.counters.Skipped)
	}
	if _, err := scan.store.GetDocumentByPath(context.Background(), "ep1_360p.mp4"); err != nil {
		t.Fatalf("the rendition under the cap must still be ingested: %v", err)
	}
	if strings.Contains(scan.log, "media.variants.select=") {
		t.Errorf("grouping is off, so no group may be reported; got: %q", scan.log)
	}
}

// TestVariantCap879_ScanSkipsTheWholeGroupWhenNothingFits pins the decision for
// a group the cap excludes entirely: one size_cap row on the rendition the
// policy would have chosen, no rows for its siblings, and a line that says the
// whole media is out rather than one line per rendition.
func TestVariantCap879_ScanSkipsTheWholeGroupWhenNothingFits(t *testing.T) {
	root := t.TempDir()
	for rel, size := range renditionSizes879 {
		if rel == "ep1_360p.mp4" {
			continue // leave nothing under the cap
		}
		writeCorpusFile(t, root, rel, bytes.Repeat([]byte("v"), size))
	}

	scan := runScan879(t, root, 1, true)

	assertSkipReason(t, context.Background(), scan.store, "ep1_1080p.mp4", "skipped", model.SkipReasonSizeCap)
	for _, rel := range []string{"ep1_720p.mp4", "ep1_480p.mp4"} {
		assertNoRow879(t, scan.store, rel)
		if codes := manifestCodes879(scan.manifest, rel); len(codes) != 0 {
			t.Errorf("%s has manifest records %v, want none (grouping discarded it)", rel, codes)
		}
	}
	// One media out of the corpus is one skip, one manifest entry and one line.
	if scan.counters.Skipped != 1 {
		t.Errorf("skipped = %d, want 1: the media is one coverage gap, not three", scan.counters.Skipped)
	}
	if codes := manifestCodes879(scan.manifest, "ep1_1080p.mp4"); len(codes) != 1 || codes[0] != "FILE_TOO_LARGE" {
		t.Errorf("ep1_1080p.mp4 manifest records = %v, want one FILE_TOO_LARGE", codes)
	}
	logLine879(t, scan.log, "no rendition of this media fits", "ep1_1080p.mp4", "3 renditions")
}

// TestVariantCap879_UpgradeTombstonesTheStrayRows covers the upgrade a deployment
// with grouping already on will make. The scan before this change wrote a
// size_cap row for every over-cap rendition. Those rows describe files the
// corpus no longer tracks, so the first scan on the new code must retire them
// instead of leaving them in the coverage aggregate forever.
func TestVariantCap879_UpgradeTombstonesTheStrayRows(t *testing.T) {
	root := t.TempDir()
	writeRenditions879(t, root)
	ctx := context.Background()

	// Stand in for the old ordering: judged one by one, the three large
	// renditions each get a size_cap row.
	st := runScan879(t, root, 1, false).store
	for _, rel := range []string{"ep1_1080p.mp4", "ep1_720p.mp4", "ep1_480p.mp4"} {
		assertSkipReason(t, ctx, st, rel, "skipped", model.SkipReasonSizeCap)
	}

	// The same store, rescanned with grouping on.
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.STTProvider = "off"
	cfg.IngestMaxFileMB = 1
	cfg.MediaVariantsGroup = true
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	for _, rel := range []string{"ep1_1080p.mp4", "ep1_720p.mp4", "ep1_480p.mp4"} {
		doc, err := st.GetDocumentByPath(ctx, rel)
		if err != nil {
			continue // the row is gone entirely, which is also correct
		}
		if !doc.Deleted {
			t.Errorf("%s is still an active row (status=%q, skip_reason=%q); it describes a rendition the corpus no longer tracks",
				rel, doc.Status, doc.SkipReason)
		}
	}
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary != nil && stats.SkipSummary.Categories[model.SkipReasonSizeCap] != 0 {
		t.Errorf("size_cap skips = %d, want 0 after the stray rows are retired",
			stats.SkipSummary.Categories[model.SkipReasonSizeCap])
	}
}

// TestVariantCap879_WorseOverCapSiblingIsSilent pins the quiet half of the rule.
// The best rendition fits the cap and a WORSE rendition does not. Grouping would
// discard the worse one anyway, so it leaves no row, no count and no line: the
// cap changed nothing that an operator has to act on.
func TestVariantCap879_WorseOverCapSiblingIsSilent(t *testing.T) {
	t.Parallel()
	fits := []ingest.DiscoveredFile{df("ep1_1080p.mp4", 500)}
	over := []ingest.OversizeCandidate{oc("ep1_480p.mp4", 65000)}

	got := ingest.SelectMediaVariantsWithCap(fits, over, groupBest())

	if !slices.Equal(relPaths(got.Files), []string{"ep1_1080p.mp4"}) {
		t.Fatalf("expected the best rendition, got %v", relPaths(got.Files))
	}
	if len(got.Oversize) != 0 {
		t.Errorf("the worse over-cap rendition must leave no size_cap row, got %v", oversizePaths(got.Oversize))
	}
	if len(got.Interactions) != 0 {
		t.Errorf("the cap did not change the choice, so nothing must be reported: %+v", got.Interactions)
	}
}

// TestVariantCap879_InteractionsOrderedByPath pins the reporting order. The
// groups come out of a map and their candidates arrive in two separate lists, so
// the order needs a rule of its own: the rel_path each group ends on.
func TestVariantCap879_InteractionsOrderedByPath(t *testing.T) {
	t.Parallel()
	fits := []ingest.DiscoveredFile{
		df("b/ep_360p.mp4", 500),
		df("a/ep_360p.mp4", 500),
	}
	over := []ingest.OversizeCandidate{
		oc("b/ep_1080p.mp4", 65000),
		oc("a/ep_1080p.mp4", 65000),
		oc("c/ep_1080p.mp4", 65000),
		oc("c/ep_720p.mp4", 40000),
	}

	got := ingest.SelectMediaVariantsWithCap(fits, over, groupBest())

	want := []string{"a/ep_360p.mp4", "b/ep_360p.mp4", "c/ep_1080p.mp4"}
	reported := make([]string, 0, len(got.Interactions))
	for _, interaction := range got.Interactions {
		reported = append(reported, interaction.Canonical)
	}
	if !slices.Equal(reported, want) {
		t.Fatalf("reported groups = %v, want %v", reported, want)
	}
	if got.Interactions[2].Indexed {
		t.Errorf("the c/ group fits nothing, so it must not be reported as indexed: %+v", got.Interactions[2])
	}
}
