package retrieval

import "testing"

// TestSearchHitForLabel_CacheMissReturnsZeroSpan pins the optibot #597 fix: the
// metadata-cache-miss fallback must return a ZERO span (empty Kind), not the
// degenerate Span{Kind:"lines"} placeholder — otherwise hybrid.go's
// `cached.Span.Kind != ""` guard treats the miss as a real span and skips
// overwriting it with a properly resolved one, re-introducing the F6 (#403)
// lines-0-0 citation on the hybrid/vector path.
func TestSearchHitForLabel_CacheMissReturnsZeroSpan(t *testing.T) {
	s := &Service{}
	hit := s.searchHitForLabel("text", 12345)
	if hit.Span.Kind != "" {
		t.Errorf("cache-miss span Kind = %q, want \"\" (no degenerate placeholder that clobbers a resolved span)", hit.Span.Kind)
	}
	if hit.ChunkID != 12345 {
		t.Errorf("cache-miss ChunkID = %d, want 12345 (label preserved)", hit.ChunkID)
	}
}
