package tests

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/avutil"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// clipExtractStub records the last extraction request and returns canned bytes
// (or an error), so open_media_clip can be exercised without ffmpeg on PATH.
type clipExtractStub struct {
	data    []byte
	err     error
	gotPath string
	gotFrom int
	gotTo   int
}

func (c *clipExtractStub) extract(_ context.Context, path string, startMS, endMS int) ([]byte, error) {
	c.gotPath = path
	c.gotFrom = startMS
	c.gotTo = endMS
	if c.err != nil {
		return nil, c.err
	}
	return c.data, nil
}

// seedAudioChunk inserts an audio document + transcript representation + a chunk
// carrying a `time` span, returning the chunk id so chunk_id resolution can be
// exercised.
func seedAudioChunk(t *testing.T, st *store.SQLiteStore, relPath string, startMS, endMS int) int64 {
	t.Helper()
	ctx := context.Background()
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("get document by path %q: %v", relPath, err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:       doc.DocID,
		RepType:     "transcript",
		RepHash:     "rep-hash",
		CreatedUnix: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("upsert representation: %v", err)
	}
	chunkID, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:   repID,
		Ordinal: 0,
		Text:    "hello world",
	}, []model.Span{{Kind: "time", StartMS: startMS, EndMS: endMS}})
	if err != nil {
		t.Fatalf("insert chunk with spans: %v", err)
	}
	return chunkID
}

func decodeClipResult(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=200 body=%s", resp.StatusCode, string(body))
	}
	var envelope struct {
		Result struct {
			IsError           bool                     `json:"isError"`
			Content           []map[string]interface{} `json:"content"`
			StructuredContent map[string]interface{}   `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if envelope.Result.IsError {
		t.Fatalf("expected success, got error: %#v", envelope.Result.StructuredContent)
	}
	if envelope.Result.StructuredContent == nil {
		t.Fatalf("expected structuredContent in successful response, got nil")
	}
	// Carry the content array through for the bytes-channel assertion.
	envelope.Result.StructuredContent["__content"] = envelope.Result.Content
	return envelope.Result.StructuredContent
}

func TestOpenMediaClip_ByChunkID_InlineBase64Shape(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("RIFF0000WAVEfmt data"))
	chunkID := seedAudioChunk(t, st, "voice.mp3", 1000, 4000)

	clip := &clipExtractStub{data: []byte("FAKE_MP3_CLIP_BYTES")}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment(clip.extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"chunk_id":`+strconv.FormatInt(chunkID, 10)+`}}}`)
	defer func() { _ = resp.Body.Close() }()

	sc := decodeClipResult(t, resp)
	if sc["rel_path"] != "voice.mp3" {
		t.Fatalf("rel_path = %#v, want voice.mp3", sc["rel_path"])
	}
	if sc["doc_type"] != "audio" {
		t.Fatalf("doc_type = %#v, want audio", sc["doc_type"])
	}
	if sc["return"] != "inline" {
		t.Fatalf("return = %#v, want inline", sc["return"])
	}
	if sc["mime_type"] != "audio/mpeg" {
		t.Fatalf("mime_type = %#v, want audio/mpeg", sc["mime_type"])
	}
	wantData := base64.StdEncoding.EncodeToString(clip.data)
	if sc["data"] != wantData {
		t.Fatalf("data = %#v, want base64(%q)", sc["data"], clip.data)
	}
	if sc["size_bytes"].(float64) != float64(len(clip.data)) {
		t.Fatalf("size_bytes = %#v, want %d", sc["size_bytes"], len(clip.data))
	}
	if sc["duration_ms"].(float64) != 3000 {
		t.Fatalf("duration_ms = %#v, want 3000", sc["duration_ms"])
	}
	span, _ := sc["span"].(map[string]interface{})
	if span == nil || span["kind"] != "time" {
		t.Fatalf("span = %#v, want time span", sc["span"])
	}
	if clip.gotFrom != 1000 || clip.gotTo != 4000 {
		t.Fatalf("extract called with [%d,%d), want [1000,4000)", clip.gotFrom, clip.gotTo)
	}
	assertSingleMediaContentItem(t, sc, "audio", wantData)
}

// assertSingleMediaContentItem verifies the tool returned exactly one typed
// media content item carrying the base64 bytes, and never a text item (media
// bytes must travel via data/uri only, per SPEC §15.11).
func assertSingleMediaContentItem(t *testing.T, sc map[string]interface{}, wantType, wantData string) {
	t.Helper()
	content, _ := sc["__content"].([]map[string]interface{})
	if len(content) != 1 {
		t.Fatalf("content items = %d, want 1: %#v", len(content), content)
	}
	if content[0]["type"] != wantType {
		t.Fatalf("content item type = %#v, want %s", content[0]["type"], wantType)
	}
	if content[0]["data"] != wantData {
		t.Fatalf("content item data mismatch")
	}
	if _, hasText := content[0]["text"]; hasText {
		t.Fatalf("content item must not carry a text field for media bytes")
	}
}

func TestOpenMediaClip_ExplicitRangeOverridesChunkSpan(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))
	chunkID := seedAudioChunk(t, st, "voice.mp3", 1000, 4000)

	clip := &clipExtractStub{data: []byte("X")}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment(clip.extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"chunk_id":`+strconv.FormatInt(chunkID, 10)+`,"start_ms":500,"end_ms":1500}}}`)
	defer func() { _ = resp.Body.Close() }()

	sc := decodeClipResult(t, resp)
	if sc["duration_ms"].(float64) != 1000 {
		t.Fatalf("duration_ms = %#v, want 1000 (explicit override)", sc["duration_ms"])
	}
	if clip.gotFrom != 500 || clip.gotTo != 1500 {
		t.Fatalf("extract called with [%d,%d), want [500,1500)", clip.gotFrom, clip.gotTo)
	}
}

func TestOpenMediaClip_ByRelPathAndRange(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "clip.mp4", "video", []byte("data"))

	clip := &clipExtractStub{data: []byte("VIDEO")}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment(clip.extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"clip.mp4","start_ms":0,"end_ms":2000}}}`)
	defer func() { _ = resp.Body.Close() }()

	sc := decodeClipResult(t, resp)
	if sc["doc_type"] != "video" {
		t.Fatalf("doc_type = %#v, want video", sc["doc_type"])
	}
	if sc["mime_type"] != "video/mp4" {
		t.Fatalf("mime_type = %#v, want video/mp4", sc["mime_type"])
	}
	content, _ := sc["__content"].([]map[string]interface{})
	if len(content) != 1 || content[0]["type"] != "video" {
		t.Fatalf("content item = %#v, want one video item", content)
	}
}

func TestOpenMediaClip_OverDuration_ClipTooLarge(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))
	cfg.MediaClipMaxDurationMS = 5000

	clip := &clipExtractStub{data: []byte("should-not-be-reached")}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment(clip.extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"voice.mp3","start_ms":0,"end_ms":6000}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "CLIP_TOO_LARGE")
	if clip.gotPath != "" {
		t.Fatalf("extraction must not run for an over-duration request; got path %q", clip.gotPath)
	}
}

func TestOpenMediaClip_OverBytes_ClipTooLarge(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))
	cfg.MediaClipMaxBytes = 8

	clip := &clipExtractStub{data: make([]byte, 64)}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment(clip.extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"voice.mp3","start_ms":0,"end_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "CLIP_TOO_LARGE")
}

func TestOpenMediaClip_NonMediaDocType_Unsupported(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "notes.txt", "text", []byte("plain text"))

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment((&clipExtractStub{}).extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":45,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"notes.txt","start_ms":0,"end_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "DOC_TYPE_UNSUPPORTED")
}

func TestOpenMediaClip_ExtractionFailure_MediaClipFailed(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))

	clip := &clipExtractStub{err: avutil.ErrToolNotFound}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment(clip.extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":46,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"voice.mp3","start_ms":0,"end_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "MEDIA_CLIP_FAILED")
}

func TestOpenMediaClip_MissingChunk_FileNotFound(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment((&clipExtractStub{}).extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":47,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"chunk_id":999999}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "FILE_NOT_FOUND")
}

func TestOpenMediaClip_ReferenceFallsBackToInline(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))

	clip := &clipExtractStub{data: []byte("BYTES")}
	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment(clip.extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":48,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"voice.mp3","start_ms":0,"end_ms":1000,"return":"reference"}}}`)
	defer func() { _ = resp.Body.Close() }()

	sc := decodeClipResult(t, resp)
	if sc["return"] != "inline" {
		t.Fatalf("return = %#v, want inline (reference fallback)", sc["return"])
	}
	if sc["data"] == nil || sc["data"] == "" {
		t.Fatalf("expected inline data on reference fallback, got %#v", sc["data"])
	}
	if _, hasURI := sc["uri"]; hasURI {
		t.Fatalf("inline fallback must not emit a uri")
	}
	if sc["reference_fallback"] == nil || sc["reference_fallback"] == "" {
		t.Fatalf("expected reference_fallback note, got %#v", sc["reference_fallback"])
	}
}

func TestOpenMediaClip_InvalidRange(t *testing.T) {
	cfg, st, _ := setupMCPToolStore(t, "voice.mp3", "audio", []byte("data"))

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st), mcp.WithExtractSegment((&clipExtractStub{}).extract)).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID, `{"jsonrpc":"2.0","id":49,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"voice.mp3","start_ms":2000,"end_ms":2000}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "INVALID_RANGE")
}
