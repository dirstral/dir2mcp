package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #759: the MCP on-demand media paths reconstructed a local path as
// RootDir+rel_path. S3FS.Walk ignores RootDir and reports no local path
// (DiscoveredFile.AbsPath == ""), so nothing exists at that path for an
// object-store corpus and open_media_clip, on-demand audio init and on-demand
// content reads all failed on a corpus that indexed fine.
//
// The tests below drive the three entry points against a real corpusfs.S3FS
// over a network-free stub client (the shape of tests/corpusfs/s3_test.go's
// newFakeS3FS, which lives in another package), and pin the two guarantees that
// had to survive the fix: root containment and the #407 path-exclusion policy,
// both enforced BEFORE any object is fetched.

// stubS3 is a network-free stand-in for the S3 client surface corpusfs.S3FS
// uses. It serves objects from memory and records every GetObject key so a test
// can assert that a refused request fetched NOTHING.
type stubS3 struct {
	objects map[string][]byte
	gets    []string
}

func (f *stubS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for key, body := range f.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		out.Contents = append(out.Contents, s3types.Object{
			Key:          aws.String(key),
			Size:         aws.Int64(int64(len(body))),
			ETag:         aws.String("\"etag-" + key + "\""),
			LastModified: aws.Time(time.Unix(1700000000, 0)),
		})
	}
	return out, nil
}

func (f *stubS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(body)))}, nil
}

func (f *stubS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(in.Key)
	f.gets = append(f.gets, key)
	body, ok := f.objects[key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

// s3Corpus is the fixture for an S3-backed MCP server: the config, the live
// store, the stub client (for fetch assertions) and the download cache dir (for
// cleanup assertions).
type s3Corpus struct {
	cfg      config.Config
	store    *store.SQLiteStore
	client   *stubS3
	fsys     corpusfs.CorpusFS
	cacheDir string
}

const s3CorpusPrefix = "corpus/"

// newS3Corpus builds an MCP config whose corpus source is s3, plus a real
// corpusfs.S3FS over a stub client. objects are keyed by rel_path (the prefix is
// added here).
//
// RootDir points at a real, EMPTY directory: the root exists, so a failure can
// only come from the missing RootDir/rel_path file, which is exactly the #759
// bug and not a "root does not exist" artifact.
func newS3Corpus(t *testing.T, objects map[string][]byte) *s3Corpus {
	t.Helper()
	rootDir := t.TempDir()
	stateDir := t.TempDir()
	cacheDir := filepath.Join(stateDir, "corpus-cache")

	keyed := make(map[string][]byte, len(objects))
	for rel, body := range objects {
		keyed[s3CorpusPrefix+rel] = body
	}
	client := &stubS3{objects: keyed}
	fsys, err := corpusfs.NewS3FS(client, corpusfs.S3Config{
		Bucket:   "bkt",
		Prefix:   s3CorpusPrefix,
		CacheDir: cacheDir,
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = rootDir
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"
	cfg.Source.Kind = "s3"
	cfg.Source.S3Bucket = "bkt"
	cfg.Source.S3Prefix = s3CorpusPrefix
	cfg.Source.S3Region = "us-east-1"

	return &s3Corpus{cfg: cfg, store: st, client: client, fsys: fsys, cacheDir: cacheDir}
}

// seedDoc records an already-indexed document, the state an S3 corpus is in
// after a successful scan (which is precisely when #759 bit: indexing worked,
// the on-demand tools did not). SourceType is "filesystem" for an S3 corpus too:
// the store normalizes source_type to archive_member or filesystem, and there is
// no per-backend value.
func seedDoc(t *testing.T, st *store.SQLiteStore, relPath, docType string, size int) {
	t.Helper()
	if err := st.UpsertDocument(context.Background(), model.Document{
		RelPath: relPath, DocType: docType, SourceType: "filesystem",
		SizeBytes: int64(size), MTimeUnix: 1, ContentHash: "seed", Status: "ok",
	}); err != nil {
		t.Fatalf("upsert document %q: %v", relPath, err)
	}
}

// assertNoFetch fails when any object was fetched. It is the assertion that a
// refusal happened BEFORE the download, not after it.
func assertNoFetch(t *testing.T, c *s3Corpus) {
	t.Helper()
	if len(c.client.gets) != 0 {
		t.Fatalf("expected no object fetch, got GetObject for %v", c.client.gets)
	}
}

// assertCacheEmpty fails when a localized copy survived the request, which is
// how a missed cleanup on an error path shows up.
func assertCacheEmpty(t *testing.T, c *s3Corpus) {
	t.Helper()
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return // nothing was ever downloaded
		}
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("localized copies left behind in %s: %v", c.cacheDir, names)
	}
}

func (c *s3Corpus) start(t *testing.T, extra ...mcp.ServerOption) (*httptest.Server, string) {
	t.Helper()
	opts := append([]mcp.ServerOption{mcp.WithStore(c.store), mcp.WithCorpusFS(c.fsys)}, extra...)
	srv := httptest.NewServer(mcp.NewServer(c.cfg, nil, opts...).Handler())
	t.Cleanup(srv.Close)
	return srv, initializeSession(t, srv.URL+c.cfg.MCPPath)
}

// TestOpenMediaClip_S3Corpus_CutsFromLocalizedCopy pins entry point 1
// (handleOpenMediaClipTool). Before the fix the request died in
// resolveDocumentPath with ENOENT on RootDir/rel_path; extraction now runs
// against a localized copy whose bytes are the object's.
func TestOpenMediaClip_S3Corpus_CutsFromLocalizedCopy(t *testing.T) {
	media := []byte("ID3-S3-OBJECT-AUDIO-BYTES")
	c := newS3Corpus(t, map[string][]byte{"media/voice.mp3": media})
	seedDoc(t, c.store, "media/voice.mp3", "audio", len(media))

	var sawPath string
	var sawBytes []byte
	clip := &clipExtractStub{data: []byte("CLIPPED")}
	extract := func(ctx context.Context, path string, startMS, endMS int) ([]byte, error) {
		sawPath = path
		// Read the file the handler handed us WHILE it is still alive: the whole
		// point is that a real, readable path exists at extraction time.
		sawBytes, _ = os.ReadFile(path)
		return clip.extract(ctx, path, startMS, endMS)
	}

	srv, session := c.start(t, mcp.WithExtractSegment(extract))
	resp := postRPC(t, srv.URL+c.cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7591,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"media/voice.mp3","start_ms":0,"end_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	sc := decodeClipResult(t, resp)
	if sc["rel_path"] != "media/voice.mp3" {
		t.Fatalf("rel_path = %#v, want media/voice.mp3", sc["rel_path"])
	}
	if !bytes.Equal(sawBytes, media) {
		t.Fatalf("extraction read %q, want the object bytes %q", sawBytes, media)
	}
	if sawPath == filepath.Join(c.cfg.RootDir, "media", "voice.mp3") {
		t.Fatalf("extraction used the reconstructed RootDir path %q; it must come from the CorpusFS", sawPath)
	}
	if got := len(c.client.gets); got != 1 {
		t.Fatalf("GetObject called %d time(s), want exactly 1: %v", got, c.client.gets)
	}
	// The copy is released once the clip is cut.
	assertCacheEmpty(t, c)
}

// TestOpenMediaClip_S3Corpus_ReleasesCopyOnExtractFailure pins the cleanup
// contract on an error path: the localized copy must not survive a failed
// extraction.
func TestOpenMediaClip_S3Corpus_ReleasesCopyOnExtractFailure(t *testing.T) {
	media := []byte("ID3-S3-OBJECT-AUDIO-BYTES")
	c := newS3Corpus(t, map[string][]byte{"media/voice.mp3": media})
	seedDoc(t, c.store, "media/voice.mp3", "audio", len(media))

	clip := &clipExtractStub{err: os.ErrInvalid}
	srv, session := c.start(t, mcp.WithExtractSegment(clip.extract))
	resp := postRPC(t, srv.URL+c.cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7592,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"media/voice.mp3","start_ms":0,"end_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, "MEDIA_CLIP_FAILED")
	if clip.gotPath == "" {
		t.Fatal("extraction was never reached; the media was not localized")
	}
	assertCacheEmpty(t, c)
	if _, err := os.Stat(clip.gotPath); err == nil {
		t.Fatalf("localized copy %s survived the failed extraction", clip.gotPath)
	}
}

// TestTranscribeOnDemand_S3Corpus_InitializesDocumentFromObject pins entry
// point 2 (initAudioDocumentOnDemand): an audio object that is not yet indexed
// gets its document row created from the object's bytes. Transcription itself
// then fails for want of a provider, which is irrelevant here — the assertion
// is the persisted row, which only exists if the bytes were reachable.
func TestTranscribeOnDemand_S3Corpus_InitializesDocumentFromObject(t *testing.T) {
	media := []byte("RIFF0000WAVEfmt s3-audio")
	c := newS3Corpus(t, map[string][]byte{"fresh.wav": media})

	srv, session := c.start(t)
	resp := postRPC(t, srv.URL+c.cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7593,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"fresh.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()

	doc, err := c.store.GetDocumentByPath(context.Background(), "fresh.wav")
	if err != nil {
		t.Fatalf("on-demand init did not create the document row: %v", err)
	}
	if doc.DocType != "audio" {
		t.Fatalf("doc_type = %q, want audio", doc.DocType)
	}
	if doc.SizeBytes != int64(len(media)) {
		t.Fatalf("size_bytes = %d, want %d", doc.SizeBytes, len(media))
	}
	if want := ingest.ComputeContentHash(media); doc.ContentHash != want {
		t.Fatalf("content_hash = %q, want %q (the hash of the OBJECT bytes)", doc.ContentHash, want)
	}
	assertCacheEmpty(t, c)
}

// TestAnnotate_S3Corpus_ReadsDocumentContent pins entry point 3
// (readDocumentContent). The document's bytes carry a secret-pattern match, so
// a successful read is observable without any provider: the secret gate refuses
// with FORBIDDEN, which can only happen once the content has been read.
func TestAnnotate_S3Corpus_ReadsDocumentContent(t *testing.T) {
	content := []byte("aws key " + exampleAWSKey)
	c := newS3Corpus(t, map[string][]byte{"note.txt": content})
	seedDoc(t, c.store, "note.txt", "text", len(content))

	srv, session := c.start(t)
	resp := postRPC(t, srv.URL+c.cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7594,"method":"tools/call","params":{"name":"dir2mcp_annotate","arguments":{"rel_path":"note.txt","schema_json":{"type":"object"}}}}`)
	defer func() { _ = resp.Body.Close() }()

	// FORBIDDEN here is the secret-content gate, which runs on bytes that were
	// actually read. Before the fix this was PERMISSION_DENIED from the failed
	// path resolution.
	assertToolCallErrorCode(t, resp, protocol.ErrorCodeForbidden)
	if len(c.client.gets) == 0 {
		t.Fatal("no object was fetched; the content was not read through the CorpusFS")
	}
	assertCacheEmpty(t, c)
}

// TestTranscribeOnDemand_S3Corpus_ExcludedPathRefusedWithoutFetch is the
// security half of #759: the #407 exclusion policy must still refuse an
// operator-excluded path, and must do so BEFORE the object is downloaded.
func TestTranscribeOnDemand_S3Corpus_ExcludedPathRefusedWithoutFetch(t *testing.T) {
	c := newS3Corpus(t, map[string][]byte{"private/voice.wav": []byte("RIFF0000WAVEfmt secret")})
	c.cfg.PathExcludes = append(c.cfg.PathExcludes, "private/**")

	srv, session := c.start(t)
	resp := postRPC(t, srv.URL+c.cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7595,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"private/voice.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, protocol.ErrorCodeForbidden)
	assertNoFetch(t, c)
	assertCacheEmpty(t, c)
	if _, err := c.store.GetDocumentByPath(context.Background(), "private/voice.wav"); err == nil {
		t.Fatal("an excluded path was initialized as a document")
	}
}

// TestOpenMediaClip_S3Corpus_ExcludedPathRefusedWithoutFetch is the same
// guarantee on the clip path, for a document that was indexed before the
// operator added the exclusion.
func TestOpenMediaClip_S3Corpus_ExcludedPathRefusedWithoutFetch(t *testing.T) {
	media := []byte("ID3-private")
	c := newS3Corpus(t, map[string][]byte{"private/voice.mp3": media})
	seedDoc(t, c.store, "private/voice.mp3", "audio", len(media))
	c.cfg.PathExcludes = append(c.cfg.PathExcludes, "private/**")

	clip := &clipExtractStub{data: []byte("CLIPPED")}
	srv, session := c.start(t, mcp.WithExtractSegment(clip.extract))
	resp := postRPC(t, srv.URL+c.cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7596,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"private/voice.mp3","start_ms":0,"end_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, protocol.ErrorCodeForbidden)
	assertNoFetch(t, c)
	assertCacheEmpty(t, c)
	if clip.gotPath != "" {
		t.Fatalf("extraction ran on %q for an excluded path", clip.gotPath)
	}
}

// toolCallErrorCode decodes an errored tool result and returns its canonical
// code. It exists because a rel_path can be refused by more than one guard and
// some assertions care that it WAS refused (and that nothing was fetched) more
// than about which guard got there first.
func toolCallErrorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d want=%d body=%s", resp.StatusCode, http.StatusOK, string(payload))
	}
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !envelope.Result.IsError {
		t.Fatalf("expected isError=true, got success with structuredContent=%#v", envelope.Result.StructuredContent)
	}
	errObj, _ := envelope.Result.StructuredContent["error"].(map[string]interface{})
	code, _ := errObj["code"].(string)
	if code == "" {
		t.Fatalf("expected structuredContent.error.code, got %#v", envelope.Result.StructuredContent)
	}
	return code
}

// TestOnDemand_S3Corpus_RefusesPathsOutsideRoot pins root containment on the
// object-store backend, where EvalSymlinks means nothing.
//
// A `..`/absolute rel_path never reaches the on-demand branch at all: the store's
// own rel_path guard rejects it during the document lookup (STORE_CORRUPT), which
// is why the assertion is "refused, and nothing fetched" rather than a specific
// code. A doubled separator DOES reach it — the store cleans `media//voice.wav`
// to `media/voice.wav` for the lookup — and is refused by the backend-independent
// containment check, because an S3 key is a byte string and the two names are two
// different objects.
func TestOnDemand_S3Corpus_RefusesPathsOutsideRoot(t *testing.T) {
	cases := []struct {
		name     string
		relPath  string
		wantCode string // empty: any refusal is acceptable
	}{
		{name: "traversal", relPath: "../outside.wav"},
		{name: "nested traversal", relPath: "media/../../outside.wav"},
		{name: "absolute", relPath: "/etc/secret.wav"},
		{name: "doubled separator", relPath: "media//voice.wav", wantCode: "PATH_OUTSIDE_ROOT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newS3Corpus(t, map[string][]byte{"media/voice.wav": []byte("RIFF0000WAVEfmt x")})
			srv, session := c.start(t)
			resp := postRPC(t, srv.URL+c.cfg.MCPPath, session,
				`{"jsonrpc":"2.0","id":7597,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"`+tc.relPath+`"}}}`)
			defer func() { _ = resp.Body.Close() }()

			if got := toolCallErrorCode(t, resp); tc.wantCode != "" && got != tc.wantCode {
				t.Fatalf("error code = %q, want %q", got, tc.wantCode)
			}
			assertNoFetch(t, c)
			assertCacheEmpty(t, c)
		})
	}
}

// unusableCorpusFS fails the test if it is ever consulted. A local corpus must
// keep resolving through the filesystem, so an injected backend is inert.
type unusableCorpusFS struct{ t *testing.T }

func (f *unusableCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	f.t.Fatal("Walk called for a local corpus")
	return nil, nil
}

func (f *unusableCorpusFS) Open(context.Context, string) (io.ReadSeekCloser, error) {
	f.t.Fatal("Open called for a local corpus")
	return nil, nil
}

func (f *unusableCorpusFS) Localize(context.Context, string) (string, func(), error) {
	f.t.Fatal("Localize called for a local corpus: local resolution must be unchanged")
	return "", nil, nil
}

// TestOpenMediaClip_LocalCorpus_UsesInRootPathUnchanged pins that a local
// corpus is untouched by the fix: extraction gets the symlink-resolved in-root
// path, no copy is made, and an injected CorpusFS is never consulted.
func TestOpenMediaClip_LocalCorpus_UsesInRootPathUnchanged(t *testing.T) {
	media := []byte("ID3-local-audio")
	cfg, st, rootDir := setupMCPToolStore(t, "media/voice.mp3", "audio", media)

	clip := &clipExtractStub{data: []byte("CLIPPED")}
	srv := httptest.NewServer(mcp.NewServer(cfg, nil,
		mcp.WithStore(st),
		mcp.WithExtractSegment(clip.extract),
		mcp.WithCorpusFS(&unusableCorpusFS{t: t}),
	).Handler())
	defer srv.Close()

	session := initializeSession(t, srv.URL+cfg.MCPPath)
	resp := postRPC(t, srv.URL+cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7598,"method":"tools/call","params":{"name":"dir2mcp_open_media_clip","arguments":{"rel_path":"media/voice.mp3","start_ms":0,"end_ms":1000}}}`)
	defer func() { _ = resp.Body.Close() }()

	decodeClipResult(t, resp)
	// The in-root file itself, symlink-resolved (t.TempDir hands back a path
	// under a symlinked /var on macOS), and NOT a copy.
	want, err := filepath.EvalSymlinks(filepath.Join(rootDir, "media", "voice.mp3"))
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	if clip.gotPath != want {
		t.Fatalf("extraction path = %q, want the in-root file %q", clip.gotPath, want)
	}
	got, err := os.ReadFile(clip.gotPath)
	if err != nil || !bytes.Equal(got, media) {
		t.Fatalf("extraction path does not hold the source bytes: %v / %q", err, got)
	}
}

// TestOnDemand_LocalCorpus_RefusesTraversal pins that containment on the local
// backend still refuses a traversal rel_path, and that an escaping path reaching
// the on-demand branch is refused there with PATH_OUTSIDE_ROOT.
func TestOnDemand_LocalCorpus_RefusesTraversal(t *testing.T) {
	cfg, st, rootDir := setupMCPToolStore(t, "voice.wav", "audio", []byte("RIFF0000WAVEfmt x"))
	// A real file one level above the corpus root: the traversal must not reach it.
	if err := os.WriteFile(filepath.Join(filepath.Dir(rootDir), "outside.wav"), []byte("RIFF0000WAVEfmt outside"), 0o644); err != nil {
		t.Fatalf("write out-of-root file: %v", err)
	}

	srv := httptest.NewServer(mcp.NewServer(cfg, nil,
		mcp.WithStore(st),
		mcp.WithCorpusFS(&unusableCorpusFS{t: t}),
	).Handler())
	defer srv.Close()

	session := initializeSession(t, srv.URL+cfg.MCPPath)
	resp := postRPC(t, srv.URL+cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7599,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"../outside.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()

	// The store's rel_path guard refuses it during the lookup, before the
	// on-demand branch; either refusal is fine, reaching the file is not.
	toolCallErrorCode(t, resp)
	if _, err := st.GetDocumentByPath(context.Background(), "outside.wav"); err == nil {
		t.Fatal("an out-of-root file was initialized as a document")
	}
}

// TestOnDemand_LocalCorpus_RefusesSymlinkToExcludedTarget pins the local-only
// half of the exclusion guarantee: a symlink whose REAL target is excluded is
// refused. That check is expressed in resolved symlinks and has no object-store
// analogue, which is why a local corpus must keep resolving through the
// filesystem rather than through an injected backend.
func TestOnDemand_LocalCorpus_RefusesSymlinkToExcludedTarget(t *testing.T) {
	cfg, st, rootDir := setupMCPToolStore(t, "private/voice.wav", "audio", []byte("RIFF0000WAVEfmt secret"))
	cfg.PathExcludes = append(cfg.PathExcludes, "private/**")
	if err := os.Symlink(filepath.Join(rootDir, "private", "voice.wav"), filepath.Join(rootDir, "link.wav")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	srv := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer srv.Close()

	session := initializeSession(t, srv.URL+cfg.MCPPath)
	resp := postRPC(t, srv.URL+cfg.MCPPath, session,
		`{"jsonrpc":"2.0","id":7600,"method":"tools/call","params":{"name":"dir2mcp_transcribe","arguments":{"rel_path":"link.wav"}}}`)
	defer func() { _ = resp.Body.Close() }()

	assertToolCallErrorCode(t, resp, protocol.ErrorCodeForbidden)
}
