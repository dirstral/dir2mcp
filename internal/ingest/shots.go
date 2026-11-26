package ingest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/shots"
	"github.com/dirstral/dir2mcp/internal/store"
)

// manifestErrShotsFailed (SPEC §14.4) records a failed shot detection on the
// run manifest. It is never the document's error: shots are not searchable.
const manifestErrShotsFailed = "SHOTS_FAILED"

// shotStore is the optional store capability the shots pass needs (SPEC §5.7).
// A store without it (a test double) has no shots and the pass is a no-op.
type shotStore interface {
	VideoShotStates(ctx context.Context) ([]store.VideoShotState, error)
	ReplaceShots(ctx context.Context, docID int64, identity shots.Identity, contentHash string, nowUnix int64, list []shots.Shot) error
	MarkShotsFailed(ctx context.Context, docID int64, nowUnix int64) error
}

// SetShotDetector overrides the shot detector (tests). A nil detector turns
// detection off for this service.
func (s *Service) SetShotDetector(d shots.Detector) {
	s.shotsResolve.Do(func() {})
	s.shotDetector = d
}

// resolveShotDetector builds the configured detector once (SPEC §8.10.1). A
// host without ffmpeg gets one warning and no detection for the run; that is
// not a per-document failure.
func (s *Service) resolveShotDetector(ctx context.Context) shots.Detector {
	s.shotsResolve.Do(func() {
		minShotS := s.cfg.ShotsMinShotS
		switch strings.ToLower(strings.TrimSpace(s.cfg.ShotsDetector)) {
		case shots.DetectorFFmpeg:
			d, err := shots.NewFFmpeg(ctx, s.cfg.ShotsThreshold, minShotS)
			if err != nil {
				s.getLogger().Printf("warning: media.shots.detector is ffmpeg but %v; no shots are detected (SPEC §8.10.1)", err)
				return
			}
			s.shotDetector = d
		case shots.DetectorServe:
			s.shotDetector = shots.NewServe(s.cfg.ShotsBaseURL, minShotS)
		}
	})
	return s.shotDetector
}

// runShotsPassLogged runs the shots pass at the end of a scan. It is never
// fatal: shots are not searchable, so a failure must not fail the scan that
// indexed the corpus. Only a cancelled context stops it early.
func (s *Service) runShotsPassLogged(ctx context.Context, force bool) {
	if err := s.runShotsPass(ctx, force); err != nil && ctx.Err() == nil {
		s.getLogger().Printf("warning: shot detection pass: %v", err)
	}
}

// runShotsPass detects the shots of every video that needs it (SPEC §8.10.6).
// Detection is its own step: it reads only the shot sets and never causes the
// other representations of a document to be derived again.
func (s *Service) runShotsPass(ctx context.Context, force bool) error {
	detector := s.resolveShotDetector(ctx)
	if detector == nil {
		return nil
	}
	st, ok := s.store.(shotStore)
	if !ok {
		return nil
	}
	states, err := st.VideoShotStates(ctx)
	if err != nil {
		return fmt.Errorf("list video shot states: %w", err)
	}
	want := detector.LocalIdentity()
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !needsShotDetection(state, want, force) {
			continue
		}
		s.detectShotsForDocument(ctx, st, detector, state)
	}
	return ctx.Err()
}

// needsShotDetection applies the re-detection rules of SPEC §8.10.6. A
// document whose done marker is withheld (empty content_hash) waits: its
// content is not settled yet, and the next scan sees it again.
func needsShotDetection(state store.VideoShotState, want shots.Identity, force bool) bool {
	if state.ContentHash == "" {
		return false
	}
	if force || !state.HasSet || state.FailedUnix != 0 || state.SetContentHash != state.ContentHash {
		return true
	}
	return !shots.ParseIdentity(state.IdentityJSON).LocalFieldsMatch(want)
}

// NeedsShotDetection is the exported counterpart of needsShotDetection, so the
// tests/ tree can pin the re-detection rules.
func NeedsShotDetection(state store.VideoShotState, want shots.Identity, force bool) bool {
	return needsShotDetection(state, want, force)
}

// detectShotsForDocument detects and stores the shots of one video. Every
// failure is soft (SPEC §8.10.7): a warning, the run's error count, a
// SHOTS_FAILED manifest record and a failure mark on the shot set. The done
// marker and every representation of the document stay as they are.
func (s *Service) detectShotsForDocument(ctx context.Context, st shotStore, detector shots.Detector, state store.VideoShotState) {
	result, durationMS, err := s.detectShots(ctx, detector, state.RelPath)
	if err != nil {
		if ctx.Err() != nil {
			return // the daemon stops; this says nothing about the document
		}
		if errors.Is(err, errShotsNoDuration) {
			s.getLogger().Printf("warning: shots: %s: media duration unknown; no shots detected (SPEC §8.10.4)", state.RelPath)
			return
		}
		s.recordShotsFailure(ctx, st, state, err)
		return
	}
	list := shots.CutsToShots(result.CutsMS, durationMS, int(s.cfg.ShotsMinShotS*1000+0.5))
	if err := st.ReplaceShots(ctx, state.DocID, result.Identity, state.ContentHash, time.Now().Unix(), list); err != nil {
		s.recordShotsFailure(ctx, st, state, err)
	}
}

// errShotsNoDuration marks a video whose duration cannot be read. SPEC
// §8.10.4 requires the duration; the document gets no shots and a warning,
// and it is not counted as a failure.
var errShotsNoDuration = errors.New("media duration unknown")

// detectShots localizes the media, reads its duration and runs the detector
// under the bound of SPEC §8.10.3 for a served detector.
func (s *Service) detectShots(ctx context.Context, detector shots.Detector, relPath string) (shots.Result, int, error) {
	localPath, cleanup, err := s.corpusFS().Localize(ctx, relPath)
	if err != nil {
		return shots.Result{}, 0, fmt.Errorf("localize: %w", err)
	}
	defer cleanup()
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return shots.Result{}, 0, fmt.Errorf("resolve media path: %w", err)
	}
	duration := s.recognizeMediaDuration(ctx, absPath)
	if duration <= 0 {
		return shots.Result{}, 0, errShotsNoDuration
	}
	callCtx := ctx
	if _, served := detector.(*shots.Serve); served {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx,
			recognizeCallTimeout(s.cfg.ShotsTimeout, s.cfg.ShotsTimeoutPerMediaSecond, duration))
		defer cancel()
	}
	result, err := detector.Detect(callCtx, absPath)
	if err != nil {
		return shots.Result{}, 0, err
	}
	return result, int(duration.Milliseconds()), nil
}

// recordShotsFailure applies the soft-failure rules of SPEC §8.10.7.
func (s *Service) recordShotsFailure(ctx context.Context, st shotStore, state store.VideoShotState, cause error) {
	message := cause.Error()
	s.getLogger().Printf("warning: shot detection failed for %s: %s; its earlier shots and representations stay, and the next scan retries shot detection alone (SPEC §8.10.7)",
		state.RelPath, message)
	s.addErrors(1)
	s.batch.recordStepFailure(state.RelPath, "shots", manifestErrShotsFailed, message)
	if err := st.MarkShotsFailed(ctx, state.DocID, time.Now().Unix()); err != nil {
		s.getLogger().Printf("warning: record shot failure for %s: %v", state.RelPath, err)
	}
}
