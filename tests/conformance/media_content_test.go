package conformance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// clipBytes is the canned payload the stub extractor returns. It stands in for
// real media bytes so the suite needs no ffmpeg on PATH.
var clipBytes = []byte("FAKE_MEDIA_CLIP_BYTES")

// seedMediaClipCorpus builds a one-document corpus for dir2mcp_open_media_clip:
// a media file on disk, its store row, a transcript representation, and one chunk
// with a time span. It returns the config, the server options that wire the store
// and a stub extractor, and the chunk id to call the tool with.
func seedMediaClipCorpus(t *testing.T, relPath, docType string) (config.Config, []mcp.ServerOption, int64) {
	t.Helper()
	ctx := context.Background()

	rootDir := t.TempDir()
	stateDir := filepath.Join(rootDir, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, relPath), []byte("source-media"), 0o644); err != nil {
		t.Fatalf("write media file: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:     relPath,
		DocType:     docType,
		SourceType:  "filesystem",
		SizeBytes:   int64(len("source-media")),
		MTimeUnix:   1,
		ContentHash: "h1",
		Status:      "ok",
	}); err != nil {
		t.Fatalf("upsert document: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("get document: %v", err)
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
	}, []model.Span{{Kind: "time", StartMS: 1000, EndMS: 4000}})
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = rootDir
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	extract := func(_ context.Context, _ string, _, _ int) ([]byte, error) { return clipBytes, nil }
	opts := []mcp.ServerOption{mcp.WithStore(st), mcp.WithExtractSegment(extract)}
	return cfg, opts, chunkID
}

// mediaContentItem is one entry of the tool result's content array.
type mediaContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MIMEType string `json:"mimeType"`
	Resource *struct {
		URI      string `json:"uri"`
		MIMEType string `json:"mimeType"`
		Blob     string `json:"blob"`
		Text     string `json:"text"`
	} `json:"resource"`
}

// callOpenMediaClip runs dir2mcp_open_media_clip for chunkID against a running
// endpoint and returns the content items plus the structured output.
func callOpenMediaClip(t *testing.T, mcpURL string, chunkID int64) ([]mediaContentItem, map[string]interface{}) {
	t.Helper()
	sid := initSession(t, mcpURL)
	body := `{"jsonrpc":"2.0","id":90,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"chunk_id":` +
		strconv.FormatInt(chunkID, 10) + `}}}`
	resp := sendRPC(t, mcpURL, sid, body, nil)
	raw := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open_media_clip: status=%d want=200 body=%s", resp.StatusCode, raw)
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			Content           []mediaContentItem     `json:"content"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("open_media_clip: decode: %v body=%s", err, raw)
	}
	if envelope.Result.IsError {
		t.Fatalf("open_media_clip: unexpected tool error body=%s", raw)
	}
	return envelope.Result.Content, envelope.Result.StructuredContent
}

// assertClipContentCarriesBytes checks the canonical content[] delivery for an
// inline clip: exactly one item, it carries the clip bytes, and it is never an
// empty text item. SPEC §15.11 forbids media bytes in a text item and requires
// content[] to carry the clip when return=inline.
func assertClipContentCarriesBytes(t *testing.T, content []mediaContentItem, wantMIME string) {
	t.Helper()
	if len(content) != 1 {
		t.Fatalf("content items = %d, want 1: %+v", len(content), content)
	}
	item := content[0]
	wantData := base64.StdEncoding.EncodeToString(clipBytes)

	if item.Type == "text" {
		t.Fatalf("content item is a text item (text=%q): media bytes must never travel as text, and an empty text item drops the clip entirely", item.Text)
	}
	switch {
	case item.Data != "":
		if item.Data != wantData {
			t.Fatalf("content item data = %q, want base64(%q)", item.Data, clipBytes)
		}
		if item.MIMEType != wantMIME {
			t.Fatalf("content item mimeType = %q, want %q", item.MIMEType, wantMIME)
		}
	case item.Resource != nil && item.Resource.Blob != "":
		if item.Resource.Blob != wantData {
			t.Fatalf("embedded resource blob = %q, want base64(%q)", item.Resource.Blob, clipBytes)
		}
		if item.Resource.MIMEType != wantMIME {
			t.Fatalf("embedded resource mimeType = %q, want %q", item.Resource.MIMEType, wantMIME)
		}
	default:
		t.Fatalf("content item type=%q carries no clip bytes: %+v", item.Type, item)
	}
}

// TestMediaContent_AudioClipReachesTheClient pins the working half of the
// conversion: an audio clip becomes an audio content item with the clip bytes.
func TestMediaContent_AudioClipReachesTheClient(t *testing.T) {
	t.Parallel()
	cfg, opts, chunkID := seedMediaClipCorpus(t, "voice.mp3", "audio")
	srv := newServer(t, cfg, opts...)
	defer srv.Close()

	content, structured := callOpenMediaClip(t, srv.URL+cfg.MCPPath, chunkID)
	if structured["mime_type"] != "audio/mpeg" {
		t.Fatalf("structured mime_type = %#v, want audio/mpeg", structured["mime_type"])
	}
	// Check the item count and the bytes first: it reports the protocol failure
	// instead of panicking on an empty content array.
	assertClipContentCarriesBytes(t, content, "audio/mpeg")
	if content[0].Type != "audio" {
		t.Fatalf("content item type = %q, want audio", content[0].Type)
	}
}

// TestMediaContent_VideoClipReachesTheClient is the regression test for issue
// #663: a video clip must reach the client as playable content, not as an empty
// text item.
//
// The tool builds a `video`-typed item whose bytes live in Data. The production
// SDK conversion maps only text and audio, so a video falls through to
// TextContent{Text: item.Text}. Text is empty for a media item, so the client sees
// content[] = [{"type":"text","text":""}]: a successful call with nothing in it.
//
// The test is skipped because the fix is blocked on the SPEC, not on the code.
// SPEC §15.11 requires "an `audio`- or `video`-typed item" for return=inline, but
// the pinned MCP generation 2025-11-25 defines no `video` content type, and the
// go-sdk Content interface is closed to outside implementations, so a
// `video`-typed item cannot be produced at all. A conformant fix has to carry the
// clip in a shape the SPEC does not yet name (an embedded resource with a blob).
// dir2mcp is spec-first, so a dirstral-spec PR must define that shape and merge
// before the code lands. dirstral/dirstral-spec#60 tracks that decision. Remove
// this skip in the follow-up.
//
// Run it to see the failure:
//
//	go test ./tests/conformance -run TestMediaContent_VideoClipReachesTheClient
func TestMediaContent_VideoClipReachesTheClient(t *testing.T) {
	t.Parallel()
	t.Skip("blocked on dirstral-spec#60: SPEC §15.11 mandates a video-typed content item, which MCP 2025-11-25 does not define (dir2mcp #663)")
	cfg, opts, chunkID := seedMediaClipCorpus(t, "clip.mp4", "video")
	srv := newServer(t, cfg, opts...)
	defer srv.Close()

	content, structured := callOpenMediaClip(t, srv.URL+cfg.MCPPath, chunkID)
	if structured["mime_type"] != "video/mp4" {
		t.Fatalf("structured mime_type = %#v, want video/mp4", structured["mime_type"])
	}
	assertClipContentCarriesBytes(t, content, "video/mp4")
}
