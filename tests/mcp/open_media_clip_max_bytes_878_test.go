package tests

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/mcp"
)

// Issue #878, spec 0.54.0: max_bytes lets the caller bound the clip bytes and
// the server re-encode a reduced-fidelity preview to fit; the preview field's
// PRESENCE marks the result as a re-encode, so a preview is never mistakable
// for a source-fidelity cut.

// clipPreviewStub records the preview request and returns canned bytes, so the
// re-encode path can be exercised without ffmpeg on PATH.
type clipPreviewStub struct {
	data      []byte
	rendition string
	err       error
	gotMax    int
	gotVideo  bool
	calls     int
}

func (c *clipPreviewStub) extract(_ context.Context, _ string, _, _, maxBytes int, video bool) ([]byte, string, error) {
	c.calls++
	c.gotMax = maxBytes
	c.gotVideo = video
	if c.err != nil {
		return nil, "", c.err
	}
	return c.data, c.rendition, nil
}

// newClipServer878 builds a server whose source cut is srcLen bytes and whose
// preview stub is supplied by the caller.
func newClipServer878(t *testing.T, srcLen int, preview *clipPreviewStub) (string, string) {
	t.Helper()
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))
	source := &clipExtractStub{data: make([]byte, srcLen)}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st),
		mcp.WithExtractSegment(source.extract),
		mcp.WithExtractSegmentPreview(preview.extract)).Handler())
	t.Cleanup(server.Close)
	return server.URL + cfg.MCPPath, initializeSession(t, server.URL+cfg.MCPPath)
}

const clipArgs878 = `{"jsonrpc":"2.0","id":878,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"voice.mp3","start_ms":0,"end_ms":1000%s}}}`

// TestOpenMediaClip878_MaxBytesServesAMarkedPreview is the feature: the source
// cut exceeds the caller's ceiling, the preview fits, and the response says so.
func TestOpenMediaClip878_MaxBytesServesAMarkedPreview(t *testing.T) {
	preview := &clipPreviewStub{data: make([]byte, 100), rendition: "aac ~96kbps, re-encoded to fit max_bytes"}
	mcpURL, sid := newClipServer878(t, 5000, preview)

	resp := postRPC(t, mcpURL, sid, sprintf878(`,"max_bytes":200`))
	defer func() { _ = resp.Body.Close() }()
	got := decodeClipResult(t, resp)

	if got["preview"] != preview.rendition {
		t.Fatalf("preview = %#v, want the rendition string; a re-encode must be marked", got["preview"])
	}
	if got["size_bytes"].(float64) != 100 {
		t.Fatalf("size_bytes = %v, want the PREVIEW's size 100, not the source cut's", got["size_bytes"])
	}
	// The preview is a new container; the mime type must describe the served
	// bytes, not the source's.
	if got["mime_type"] != "audio/mp4" {
		t.Fatalf("mime_type = %v, want audio/mp4 for a re-encoded audio preview", got["mime_type"])
	}
	if preview.gotMax != 200 {
		t.Fatalf("preview asked to fit %d bytes, want the caller's 200", preview.gotMax)
	}
	if preview.gotVideo {
		t.Fatal("audio document requested a video preview")
	}
}

// TestOpenMediaClip878_SourceCutThatFitsCarriesNoPreviewField is the other half
// of the presence contract: fidelity was not reduced, so the field must be
// absent, or a source cut becomes mistakable for a preview.
func TestOpenMediaClip878_SourceCutThatFitsCarriesNoPreviewField(t *testing.T) {
	preview := &clipPreviewStub{data: make([]byte, 1)}
	mcpURL, sid := newClipServer878(t, 100, preview)

	resp := postRPC(t, mcpURL, sid, sprintf878(`,"max_bytes":200`))
	defer func() { _ = resp.Body.Close() }()
	got := decodeClipResult(t, resp)

	if _, present := got["preview"]; present {
		t.Fatalf("preview present on a source-fidelity cut: %#v", got["preview"])
	}
	if preview.calls != 0 {
		t.Fatalf("re-encode ran although the source cut fit the ceiling (%d calls)", preview.calls)
	}
	if got["mime_type"] != "audio/mpeg" {
		t.Fatalf("mime_type = %v, want the source container's audio/mpeg", got["mime_type"])
	}
}

// TestOpenMediaClip878_NoMaxBytesKeepsTodaysRefusal pins that a caller who
// never asked for a byte budget never receives silently reduced fidelity: over
// the SERVER cap without max_bytes stays CLIP_TOO_LARGE, and the re-encoder is
// never consulted.
func TestOpenMediaClip878_NoMaxBytesKeepsTodaysRefusal(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))
	cfg.MediaClipMaxBytes = 8
	preview := &clipPreviewStub{data: make([]byte, 1)}
	source := &clipExtractStub{data: make([]byte, 64)}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st),
		mcp.WithExtractSegment(source.extract),
		mcp.WithExtractSegmentPreview(preview.extract)).Handler())
	defer server.Close()
	sid := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sid, sprintf878(``))
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "CLIP_TOO_LARGE")
	if preview.calls != 0 {
		t.Fatalf("re-encode ran without a caller budget (%d calls)", preview.calls)
	}
}

// TestOpenMediaClip878_EvenThePreviewOverBudgetIsRefused pins the failure the
// spec names: when even a re-encode cannot fit the span under the effective
// bound, CLIP_TOO_LARGE, and no oversized bytes are served.
func TestOpenMediaClip878_EvenThePreviewOverBudgetIsRefused(t *testing.T) {
	preview := &clipPreviewStub{data: make([]byte, 900), rendition: "aac ~24kbps, re-encoded to fit max_bytes"}
	mcpURL, sid := newClipServer878(t, 5000, preview)

	resp := postRPC(t, mcpURL, sid, sprintf878(`,"max_bytes":200`))
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "CLIP_TOO_LARGE")
}

// TestOpenMediaClip878_NoFfmpegMeansNoPreviewToOffer pins the spec sentence: a
// server that cannot re-encode serves a source cut when it fits and fails
// CLIP_TOO_LARGE when it does not, never MEDIA_CLIP_FAILED for the mere absence
// of a preview capability.
func TestOpenMediaClip878_NoFfmpegMeansNoPreviewToOffer(t *testing.T) {
	preview := &clipPreviewStub{err: avutil.ErrToolNotFound}
	mcpURL, sid := newClipServer878(t, 5000, preview)

	resp := postRPC(t, mcpURL, sid, sprintf878(`,"max_bytes":200`))
	defer func() { _ = resp.Body.Close() }()
	assertToolCallErrorCode(t, resp, "CLIP_TOO_LARGE")
}

// TestOpenMediaClip878_CallerCannotWidenTheServerCap pins the min(): a
// max_bytes above media.clip.max_bytes narrows to the server's cap, so the
// caller cannot use the new argument to bypass the server bound.
func TestOpenMediaClip878_CallerCannotWidenTheServerCap(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))
	cfg.MediaClipMaxBytes = 150
	preview := &clipPreviewStub{data: make([]byte, 100), rendition: "aac ~64kbps, re-encoded to fit max_bytes"}
	source := &clipExtractStub{data: make([]byte, 5000)}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st),
		mcp.WithExtractSegment(source.extract),
		mcp.WithExtractSegmentPreview(preview.extract)).Handler())
	defer server.Close()
	sid := initializeSession(t, server.URL+cfg.MCPPath)

	resp := postRPC(t, server.URL+cfg.MCPPath, sid, sprintf878(`,"max_bytes":99999999`))
	defer func() { _ = resp.Body.Close() }()
	got := decodeClipResult(t, resp)
	if preview.gotMax != 150 {
		t.Fatalf("effective bound handed to the re-encoder = %d, want the server cap 150", preview.gotMax)
	}
	if _, present := got["preview"]; !present {
		t.Fatal("re-encoded result not marked as a preview")
	}
}

func sprintf878(extra string) string {
	return strings.Replace(clipArgs878, "%s", extra, 1)
}
