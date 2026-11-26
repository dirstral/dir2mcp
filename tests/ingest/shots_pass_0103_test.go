package tests

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/shots"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §8.10.6/§8.10.7 (spec 0.103.0): shot detection is its own step. It
// never causes the other representations of a document to be derived again,
// and a failure never holds back the done marker. Recognition is the step
// that would be expensive to repeat, so these tests count its calls.

type fakeShotDetector struct {
	cuts  []int
	fail  int // fail this many calls first
	calls int
}

func (d *fakeShotDetector) Detect(_ context.Context, _ string) (shots.Result, error) {
	d.calls++
	if d.calls <= d.fail {
		return shots.Result{}, errors.New("shots backend returned status 502")
	}
	return shots.Result{Identity: d.LocalIdentity(), CutsMS: d.cuts}, nil
}

func (d *fakeShotDetector) LocalIdentity() shots.Identity {
	return shots.Identity{Detector: "serve", Name: "fake", Version: "1", MinShotS: 0.5}
}

type countingRecognizer struct{ calls int }

func (r *countingRecognizer) Recognize(_ context.Context, _ string) (model.RecognizeResult, error) {
	r.calls++
	return model.RecognizeResult{Name: "test", Version: "1", Annotations: []model.RecognizedAnnotation{
		{StartMS: 0, EndMS: 1000, Text: "a presenter at a desk", Event: "caption"},
	}}, nil
}

func newShotsService(t *testing.T, root string, st *store.SQLiteStore, rec *countingRecognizer) *ingest.Service {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = testutil.TempDir(t)
	cfg.RecognizeProvider = "serve"
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(rec)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 10 * time.Second, nil }
	return svc
}

func newShotsStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	return st
}

func TestShots0103_TurningDetectionOnDoesNotReprocessTheCorpus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "news.mp4"), "fake-video-bytes")
	st := newShotsStore(t)
	rec := &countingRecognizer{}

	// Scan 1: detection off. The video is indexed through recognition.
	svc := newShotsService(t, root, st, rec)
	svc.SetShotDetector(nil)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("recognition calls after scan 1 = %d, want 1", rec.calls)
	}

	// Scan 2: detection on for the indexed corpus.
	det := &fakeShotDetector{cuts: []int{2000, 6000}}
	svc = newShotsService(t, root, st, rec)
	svc.SetShotDetector(det)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if det.calls != 1 {
		t.Fatalf("detector calls = %d, want 1: an indexed video with no shot set must be detected", det.calls)
	}
	if rec.calls != 1 {
		t.Fatalf("recognition calls = %d, want still 1: turning shots on must not reprocess the video", rec.calls)
	}
	doc := documentByPath(t, st, "news.mp4")
	got, err := st.Shots(ctx, doc.DocID)
	if err != nil {
		t.Fatalf("Shots: %v", err)
	}
	want := []shots.Shot{{Index: 0, StartMS: 0, EndMS: 2000}, {Index: 1, StartMS: 2000, EndMS: 6000}, {Index: 2, StartMS: 6000, EndMS: 10000}}
	if len(got) != len(want) {
		t.Fatalf("shots = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shots = %v, want %v", got, want)
		}
	}

	// Scan 3: nothing changed, so the video is not decoded for shots again.
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 3: %v", err)
	}
	if det.calls != 1 || rec.calls != 1 {
		t.Fatalf("an unchanged video was processed again: detector %d, recognition %d", det.calls, rec.calls)
	}
}

func TestShots0103_AFailureRetriesOnlyShotDetection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	writeFile(t, filepath.Join(root, "news.mp4"), "fake-video-bytes")
	st := newShotsStore(t)
	rec := &countingRecognizer{}
	det := &fakeShotDetector{cuts: []int{4000}, fail: 1}
	svc := newShotsService(t, root, st, rec)
	svc.SetShotDetector(det)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	doc := documentByPath(t, st, "news.mp4")
	if doc.Status != "ok" || doc.ContentHash == "" {
		t.Fatalf("status=%q content_hash=%q: a shot failure must not fail the document or hold back its done marker",
			doc.Status, doc.ContentHash)
	}
	if got, err := st.Shots(ctx, doc.DocID); err != nil || len(got) != 0 {
		t.Fatalf("a failed detection stored shots: %v (err %v)", got, err)
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if det.calls != 2 {
		t.Fatalf("detector calls = %d, want 2: the failure must be retried", det.calls)
	}
	if rec.calls != 1 {
		t.Fatalf("recognition calls = %d, want 1: a shot failure must not rerun recognition", rec.calls)
	}
	if got, err := st.Shots(ctx, doc.DocID); err != nil || len(got) != 2 {
		t.Fatalf("shots after the retry = %v (err %v), want 2", got, err)
	}
}

func TestShots0103_NeedsShotDetection(t *testing.T) {
	want := shots.Identity{Detector: "ffmpeg", Version: "6.1.1", Threshold: 0.2, MinShotS: 0.5, Height: 360}
	current := store.VideoShotState{
		ContentHash: "c1", HasSet: true, IdentityJSON: want.JSON(), SetContentHash: "c1",
	}
	if ingest.NeedsShotDetection(current, want, false) {
		t.Fatal("a current shot set must not be detected again")
	}
	cases := map[string]func(s *store.VideoShotState){
		"no set":          func(s *store.VideoShotState) { s.HasSet = false },
		"content changed": func(s *store.VideoShotState) { s.ContentHash = "c2" },
		"last failed":     func(s *store.VideoShotState) { s.FailedUnix = 1 },
		"identity changed": func(s *store.VideoShotState) {
			id := want
			id.Threshold = 0.3
			s.IdentityJSON = id.JSON()
		},
	}
	for name, change := range cases {
		st := current
		change(&st)
		if !ingest.NeedsShotDetection(st, want, false) {
			t.Fatalf("%s: want a new detection", name)
		}
	}
	if !ingest.NeedsShotDetection(current, want, true) {
		t.Fatal("a forced reindex must detect again")
	}
	withheld := current
	withheld.ContentHash = ""
	if ingest.NeedsShotDetection(withheld, want, true) {
		t.Fatal("a document whose done marker is withheld must wait")
	}
}

func TestShots0103_TombstoneDeletesShots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := testutil.TempDir(t)
	video := filepath.Join(root, "news.mp4")
	writeFile(t, video, "fake-video-bytes")
	st := newShotsStore(t)
	svc := newShotsService(t, root, st, &countingRecognizer{})
	svc.SetShotDetector(&fakeShotDetector{cuts: []int{4000}})
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 1: %v", err)
	}
	doc := documentByPath(t, st, "news.mp4")
	if got, err := st.Shots(ctx, doc.DocID); err != nil || len(got) != 2 {
		t.Fatalf("shots = %v (err %v), want 2", got, err)
	}
	if err := os.Remove(video); err != nil {
		t.Fatal(err)
	}
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run 2: %v", err)
	}
	if got, err := st.Shots(ctx, doc.DocID); err != nil || len(got) != 0 {
		t.Fatalf("a tombstoned document kept its shots: %v (err %v)", got, err)
	}
}

// TestShots0103_CancelledResolutionIsRetried: a scan cancelled while the
// detector is resolved must not leave the service without a detector for
// every later scan.
func TestShots0103_CancelledResolutionIsRetried(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	cfg := config.Default()
	cfg.RootDir = testutil.TempDir(t)
	cfg.StateDir = testutil.TempDir(t)
	cfg.ShotsDetector = "ffmpeg"
	svc := mustNewIngestService(t, cfg, newShotsStore(t))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if d := svc.ResolveShotDetector(cancelled); d != nil {
		t.Fatalf("a cancelled resolution returned a detector: %T", d)
	}
	if d := svc.ResolveShotDetector(context.Background()); d == nil {
		t.Fatal("the resolution after a cancelled one must build the detector")
	}
}
