package tests

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Regression guards for #894, the recognition wall-clock ceiling.
//
// Two defects, measured on a live pilot: a hard-coded ten-minute bound on one
// /recognize call, which no configuration could raise, so vision recognition
// could not run on long media at all; and a failure mode where the timeout
// stamped the document status="error", which hides EVERY chunk that document has
// (store.liveParentDocument) and left a 3h24m broadcast with 892 already-indexed
// annotations serving nothing.

// deadlineRecordingRecognizer reports the deadline it was called with and returns
// a canned result. It is how the tests observe the bound the service applied.
type deadlineRecordingRecognizer struct {
	result   model.RecognizeResult
	calls    int
	hadNone  bool
	deadline time.Time
}

func (d *deadlineRecordingRecognizer) Recognize(ctx context.Context, _ string) (model.RecognizeResult, error) {
	d.calls++
	dl, ok := ctx.Deadline()
	if !ok {
		d.hadNone = true
		return d.result, nil
	}
	d.deadline = dl
	return d.result, nil
}

// blockingRecognizer models the pilot's backend: it accepted the call and was
// still working when the budget ran out. It never returns on its own.
type blockingRecognizer struct{ calls int }

func (b *blockingRecognizer) Recognize(ctx context.Context, _ string) (model.RecognizeResult, error) {
	b.calls++
	<-ctx.Done()
	return model.RecognizeResult{}, ctx.Err()
}

// TestRecognizeCallTimeout_Arithmetic pins the bound's arithmetic so nobody has to
// read the code to learn it, defaults included.
func TestRecognizeCallTimeout_Arithmetic(t *testing.T) {
	t.Parallel()
	const hour = time.Hour
	cases := []struct {
		name  string
		base  time.Duration
		ratio float64
		media time.Duration
		want  time.Duration
	}{
		// The flat floor governs short media: the shipped defaults leave a
		// 30-second clip on exactly the old ten-minute bound.
		{"defaults short clip", config.DefaultRecognizeTimeout, config.DefaultRecognizeTimeoutPerMediaSecond,
			30 * time.Second, 10 * time.Minute},
		// The ratio governs long media: the pilot's 3h24m broadcast gets 6h48m.
		{"defaults long broadcast", config.DefaultRecognizeTimeout, config.DefaultRecognizeTimeoutPerMediaSecond,
			3*hour + 24*time.Minute, 6*hour + 48*time.Minute},
		// The maximum, not the ratio alone: a ratio can never TIGHTEN the floor.
		{"ratio below the floor", 10 * time.Minute, 0.1, 10 * time.Minute, 10 * time.Minute},
		// An unprobeable duration leaves the floor as the only bound.
		{"unknown duration", 42 * time.Minute, 2.0, 0, 42 * time.Minute},
		// A zero ratio disables the scaling entirely.
		{"scaling disabled", 20 * time.Minute, 0, 8 * hour, 20 * time.Minute},
		// A zero/negative base is never an unbounded call.
		{"zero base falls back", 0, 0, 0, config.DefaultRecognizeTimeout},
		{"negative base falls back", -5 * time.Minute, 0, 0, config.DefaultRecognizeTimeout},
		// A ratio of 1 is "real time": one wall-clock second per second of media.
		{"real time ratio", 0, 1, 4 * hour, 4 * hour},
		// NaN fails every comparison, so it must be rejected explicitly rather than
		// reaching a float-to-int conversion Go leaves implementation-defined.
		{"nan ratio", 30 * time.Minute, math.NaN(), 4 * hour, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ingest.RecognizeCallTimeout(tc.base, tc.ratio, tc.media); got != tc.want {
				t.Fatalf("RecognizeCallTimeout(%s, %v, %s) = %s, want %s",
					tc.base, tc.ratio, tc.media, got, tc.want)
			}
		})
	}
}

// TestRecognizeCallTimeout_OverflowClamps guards the arithmetic against wrapping
// int64 into a negative duration, which would be an ALREADY-EXPIRED deadline: the
// worst possible failure of a bound meant to be generous.
func TestRecognizeCallTimeout_OverflowClamps(t *testing.T) {
	t.Parallel()
	for _, ratio := range []float64{1e18, math.Inf(1)} {
		got := ingest.RecognizeCallTimeout(10*time.Minute, ratio, 10000*time.Hour)
		if got <= 0 {
			t.Fatalf("bound for ratio %v = %s, want a positive clamp, never a wrapped negative deadline",
				ratio, got)
		}
	}
}

// TestRecognizeTimeout_ConfiguredBoundIsHonoured pins that the configured value
// reaches the backend call, including a value LARGER than the old ten minutes.
// Before the fix the recognizer was called with no deadline at all and the client
// capped every call at ten minutes, so no configuration could exceed it.
func TestRecognizeTimeout_ConfiguredBoundIsHonoured(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.RecognizeTimeout = 45 * time.Minute
	cfg.RecognizeTimeoutPerMediaSecond = 0

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	rec := &deadlineRecordingRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("GenerateRecognitionRepresentation: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("recognizer calls = %d, want 1", rec.calls)
	}
	if rec.hadNone {
		t.Fatal("the recognizer was called with no deadline; the configured bound never reached the call")
	}
	if budget := time.Until(rec.deadline); budget <= 10*time.Minute {
		t.Fatalf("call budget = %s, want the configured 45m: a value above the old ten-minute "+
			"ceiling must be honoured", budget)
	}
}

// TestRecognizeTimeout_ScalesWithMediaDuration pins the duration-scaled half end
// to end: the probed media duration, not a flat constant, sets the bound.
func TestRecognizeTimeout_ScalesWithMediaDuration(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "broadcast.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	rec := &deadlineRecordingRecognizer{result: recognizeTestResult()}
	svc.SetRecognizer(rec)
	// The pilot's file: 12236 seconds of 1080p broadcast.
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) {
		return 12236 * time.Second, nil
	}

	doc := model.Document{DocID: 1, RelPath: "broadcast.mp4", DocType: "video"}
	if err := svc.GenerateRecognitionRepresentation(context.Background(), doc); err != nil {
		t.Fatalf("GenerateRecognitionRepresentation: %v", err)
	}
	if rec.hadNone {
		t.Fatal("the recognizer was called with no deadline; the duration-scaled bound never applied")
	}
	// 12236s x the default ratio of 2 is 24472s, so the budget must be hours, not
	// the ten minutes that made the capability unusable on this file.
	if budget := time.Until(rec.deadline); budget < 6*time.Hour {
		t.Fatalf("call budget = %s for a 3h24m file, want the duration-scaled bound (about 6h48m)", budget)
	}
}

// TestRecognizeTimeout_ClassifiedApartFromABrokenBackend pins the line the fix
// draws. A deadline expiry carries ErrRecognizeTimeout, which the media pipeline
// degrades. It still carries ErrRecognitionProviderFailure, so the canonical
// RECOGNIZE_FAILED classification (§14.4) is unchanged.
func TestRecognizeTimeout_ClassifiedApartFromABrokenBackend(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.RecognizeTimeout = 40 * time.Millisecond
	cfg.RecognizeTimeoutPerMediaSecond = 0

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(&blockingRecognizer{})
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(context.Background(), doc)
	if err == nil {
		t.Fatal("expected the expired budget to be reported")
	}
	if !errors.Is(err, ingest.ErrRecognizeTimeout) {
		t.Fatalf("err = %v, want it to carry ErrRecognizeTimeout", err)
	}
	if !errors.Is(err, ingest.ErrRecognitionProviderFailure) {
		t.Fatalf("err = %v, want it to still classify as RECOGNIZE_FAILED", err)
	}
	if len(st.reps) != 0 {
		t.Fatalf("expected no representation from a call that never answered, got %+v", st.reps)
	}
}

// TestRecognizeTimeout_BrokenBackendIsNotATimeout is the other side of that line.
// A backend that answers with an error status is misconfigured or broken, not slow,
// so it must NOT be classified as a timeout and must keep failing loudly. This is
// what stops a typo'd base_url from silently indexing nothing.
func TestRecognizeTimeout_BrokenBackendIsNotATimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(ingest.NewRecognizeServeClient(srv.URL))
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(context.Background(), doc)
	if err == nil {
		t.Fatal("expected a backend error status to propagate")
	}
	if errors.Is(err, ingest.ErrRecognizeTimeout) {
		t.Fatalf("err = %v, must NOT be classified as a timeout: a broken backend fails loudly", err)
	}
	if !errors.Is(err, ingest.ErrRecognitionProviderFailure) {
		t.Fatalf("err = %v, want RECOGNIZE_FAILED classification", err)
	}
}

// TestRecognizeServeClient_HonoursCallerDeadline pins that the serve client takes
// its bound from the request context. The client no longer carries a fixed
// ten-minute http.Client.Timeout, which is what capped every configured value.
func TestRecognizeServeClient_HonoursCallerDeadline(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	client := ingest.NewRecognizeServeClient(srv.URL)
	// A fixed client-level ceiling would cap the per-document deadline the service
	// computes, whichever is shorter, which is exactly the ten-minute wall #894
	// removed. There must be none.
	if got := client.HTTPClientTimeout(); got != 0 {
		t.Fatalf("http.Client.Timeout = %s, want none: a client-level ceiling caps every "+
			"configured per-document bound", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := client.Recognize(ctx, "/corpus/game7.mp4"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline to bound the call", err)
	}
}

// TestRecognizeTimeout_DoesNotEmptyTheCorpus is the defect the pilot hit, end to
// end through the real store.
//
// A video with an indexed subtitle transcript is recognized by a backend that
// never answers. Before the fix the document was stamped status="error", which
// hides every chunk of that document from lexical and vector search alike
// (store.liveParentDocument), so a corpus that answered questions a minute
// earlier answered nothing. The transcript that DID arrive must survive, and the
// document must still be retried on the next scan.
func TestRecognizeTimeout_DoesNotEmptyTheCorpus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "broadcast.mp4"), "fake-video-bytes")
	writeFile(t, filepath.Join(root, "broadcast.vtt"),
		"WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nWebb delivers the pitch\n\n"+
			"00:00:02.000 --> 00:00:05.000\nFreeman flies out to centrefield\n")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	// An isolated state dir: the default is ./.dir2mcp relative to the process
	// working directory, which these parallel tests would otherwise share with each
	// other and leave behind in the package directory.
	cfg.StateDir = t.TempDir()
	cfg.RecognizeProvider = "serve"
	cfg.RecognizeTimeout = 40 * time.Millisecond
	cfg.RecognizeTimeoutPerMediaSecond = 0

	svc := mustNewIngestService(t, cfg, st)
	rec := &blockingRecognizer{}
	svc.SetRecognizer(rec)
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("recognizer calls = %d, want 1", rec.calls)
	}

	doc := documentByPath(t, st, "broadcast.mp4")
	if doc.Status == "error" {
		t.Fatalf("status = %q (%q); a recognition timeout must not fail a document that "+
			"already carries a transcript, because status=error hides every one of its chunks",
			doc.Status, doc.ErrorMessage)
	}
	// The retry is what a withheld done marker buys: an empty content_hash makes the
	// next incremental scan reprocess this document instead of calling it finished.
	if doc.ContentHash != "" {
		t.Errorf("content_hash = %q, want it withheld so the next scan retries recognition",
			doc.ContentHash)
	}

	hits, err := st.SearchBM25(ctx, "Freeman", 5, "")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("the transcript that DID arrive is no longer searchable: the timeout emptied the corpus")
	}
}

// TestRecognizeTimeout_EmptyDocumentStillFailsLoudly pins the other half of the
// degrade rule. A video with NOTHING indexed — no sidecar, no transcript, no
// earlier annotations — has no chunks that a status="error" stamp could hide, and
// it genuinely is not searchable. That document keeps failing hard, so the failure
// is recorded durably and shows up in recent_failures after a restart, instead of
// being reported as an indexed document that answers nothing.
func TestRecognizeTimeout_EmptyDocumentStillFailsLoudly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "clip.mp4"), "fake-video-bytes")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	// An isolated state dir: the default is ./.dir2mcp relative to the process
	// working directory, which these parallel tests would otherwise share with each
	// other and leave behind in the package directory.
	cfg.StateDir = t.TempDir()
	cfg.RecognizeProvider = "serve"
	cfg.RecognizeTimeout = 40 * time.Millisecond
	cfg.RecognizeTimeoutPerMediaSecond = 0

	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(&blockingRecognizer{})
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	doc := documentByPath(t, st, "clip.mp4")
	if doc.Status != "error" {
		t.Fatalf("status = %q, want \"error\": a document with nothing indexed loses nothing to "+
			"the stamp, and an unsearchable document must be recorded durably", doc.Status)
	}
	if doc.ContentHash != "" {
		t.Errorf("content_hash = %q, want it withheld so the next scan retries", doc.ContentHash)
	}
}

// TestRecognizeTimeout_ParentCancellationIsNotATimeout pins the parent-context half
// of the classification. A cancelled parent means the DAEMON is going away
// (shutdown, an outer deadline), which says nothing about the backend, so it must
// keep the hard path and must NOT be degraded as a recognition timeout.
func TestRecognizeTimeout_ParentCancellationIsNotATimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	// Generous, so the only expiry that can happen is the parent's cancellation.
	cfg.RecognizeTimeout = time.Hour
	cfg.RecognizeTimeoutPerMediaSecond = 0

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	entered := make(chan struct{})
	svc.SetRecognizer(&signallingBlockingRecognizer{entered: entered})
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	defer cancel()

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(ctx, doc)
	if err == nil {
		t.Fatal("expected the cancelled parent to be reported")
	}
	if errors.Is(err, ingest.ErrRecognizeTimeout) {
		t.Fatalf("err = %v, must NOT be a recognition timeout: the daemon was shutting down", err)
	}
	if !errors.Is(err, ingest.ErrRecognitionProviderFailure) {
		t.Fatalf("err = %v, want RECOGNIZE_FAILED classification", err)
	}
}

// TestRecognizeTimeout_ParentDeadlineIsNotATimeout is the case the parent-context
// guard exists for. A cancelled parent is already distinguishable (the child
// context reports Canceled), but an EXPIRED parent makes the child report
// DeadlineExceeded too. The budget this daemon granted was nowhere near spent, so
// that is not evidence about the backend and must keep the hard path.
func TestRecognizeTimeout_ParentDeadlineIsNotATimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.RecognizeTimeout = time.Hour
	cfg.RecognizeTimeoutPerMediaSecond = 0

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(&blockingRecognizer{})
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(ctx, doc)
	if err == nil {
		t.Fatal("expected the expired parent to be reported")
	}
	if errors.Is(err, ingest.ErrRecognizeTimeout) {
		t.Fatalf("err = %v, must NOT be a recognition timeout: the recognition budget was 1h "+
			"and it was the caller's own deadline that expired", err)
	}
	if !errors.Is(err, ingest.ErrRecognitionProviderFailure) {
		t.Fatalf("err = %v, want RECOGNIZE_FAILED classification", err)
	}
}

// signallingBlockingRecognizer announces that it was entered, then blocks until its
// context ends. It lets a test cancel the parent context at a deterministic point.
type signallingBlockingRecognizer struct{ entered chan struct{} }

func (s *signallingBlockingRecognizer) Recognize(ctx context.Context, _ string) (model.RecognizeResult, error) {
	close(s.entered)
	<-ctx.Done()
	return model.RecognizeResult{}, ctx.Err()
}

// TestRecognizeServeClient_FallbackDeadlineDoesNotBreakTheCall covers the bare
// client's no-deadline path: a caller that supplies no deadline still gets a bound,
// and that bound must be the shipped default, not a zero (already-expired) one.
// The 10-minute value itself cannot be observed from outside the client without
// adding injection API purely for a test, so what is pinned here is that the
// fallback leaves a normal call working.
func TestRecognizeServeClient_FallbackDeadlineDoesNotBreakTheCall(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte(`{"recognizer":{"name":"r","version":"1"},"annotations":[]}`))
	}))
	t.Cleanup(srv.Close)

	client := ingest.NewRecognizeServeClient(srv.URL)
	res, err := client.Recognize(context.Background(), "/corpus/game7.mp4")
	if err != nil {
		t.Fatalf("a call with no caller deadline must still succeed: %v", err)
	}
	if res.Name != "r" {
		t.Fatalf("recognizer identity not decoded: %+v", res)
	}
}

// TestRecognizeTimeout_UnreachableBackendIsNotATimeout is the connection-stage
// half of the classification. A refused port fails fast, but a route that DROPS
// packets hangs until the clock runs out, so an unreachable or firewalled
// base_url can present as a deadline and would otherwise be degraded like a slow
// recognizer. The serve client reports when it can prove the request never
// reached the backend, and that keeps the hard path where a misconfiguration
// belongs.
//
// The address is RFC 5737 TEST-NET-1, reserved for documentation and never
// routable. The assertion holds whichever way the host behaves: a dropped route
// expires the deadline, an immediate unreachable error returns sooner, and in both
// cases the request was never delivered.
func TestRecognizeTimeout_UnreachableBackendIsNotATimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.RecognizeTimeout = 150 * time.Millisecond
	cfg.RecognizeTimeoutPerMediaSecond = 0

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(ingest.NewRecognizeServeClient("http://192.0.2.1:9"))
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(context.Background(), doc)
	if err == nil {
		t.Fatal("expected an unreachable backend to be reported")
	}
	if errors.Is(err, ingest.ErrRecognizeTimeout) {
		t.Fatalf("err = %v, must NOT be a recognition timeout: the request never reached a backend, "+
			"which is the misconfiguration hard propagation exists to catch", err)
	}
	if !errors.Is(err, ingest.ErrRecognitionProviderFailure) {
		t.Fatalf("err = %v, want RECOGNIZE_FAILED classification", err)
	}
}

// TestRecognizeTimeout_DeliveredRequestThatStallsIsATimeout is the positive side of
// the delivery evidence, through the real serve client. The backend accepts the
// connection and the whole request, then answers nothing: that is a working
// backend that ran out of wall clock, so it MUST classify as a recognition
// timeout and not as a broken binding.
func TestRecognizeTimeout_DeliveredRequestThatStallsIsATimeout(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "game7.mp4"), "fake-video")

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.RecognizeTimeout = 60 * time.Millisecond
	cfg.RecognizeTimeoutPerMediaSecond = 0

	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, cfg, st)
	svc.SetRecognizer(ingest.NewRecognizeServeClient(srv.URL))
	svc.ProbeDurationFunc = func(context.Context, string) (time.Duration, error) { return 0, os.ErrNotExist }

	doc := model.Document{DocID: 1, RelPath: "game7.mp4", DocType: "video"}
	err := svc.GenerateRecognitionRepresentation(context.Background(), doc)
	if err == nil {
		t.Fatal("expected the expired budget to be reported")
	}
	if !errors.Is(err, ingest.ErrRecognizeTimeout) {
		t.Fatalf("err = %v, want a recognition timeout: the backend took the whole request "+
			"and was still working when the budget ran out", err)
	}
}
