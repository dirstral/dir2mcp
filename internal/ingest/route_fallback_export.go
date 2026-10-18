package ingest

import (
	"context"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Test seams for the SPEC §8.2.4 route-fallback retry (dir2mcp #1122), for the
// tests/ tree (AGENTS.md: no new _test.go under internal/). Production code
// reads media.stt.route_retry_after from the config and the wall clock.

// SetRouteRetryAfterForTest overrides media.stt.route_retry_after.
func (s *Service) SetRouteRetryAfterForTest(d time.Duration) {
	s.routeRetryAfter = d
}

// SetClockForTest overrides the time source of the route-fallback rule. Nil
// restores time.Now.
func (s *Service) SetClockForTest(now func() time.Time) {
	s.clock = now
}

// DeriveTranscriptTranslationsForTest runs the derivation pass's transcript
// read for one document (§8.6.11), which must not retry a fallback result.
func (s *Service) DeriveTranscriptTranslationsForTest(ctx context.Context, doc model.Document, content []byte) error {
	return s.deriveTranscriptTranslations(ctx, doc, content)
}

// RouteFallbackStaleForTest exposes the §8.2.4 re-derivation gate for one
// document.
func (s *Service) RouteFallbackStaleForTest(ctx context.Context, relPath string) bool {
	reader, ok := s.store.(representationMetaReader)
	return ok && s.routeFallbackStale(ctx, reader, relPath)
}
