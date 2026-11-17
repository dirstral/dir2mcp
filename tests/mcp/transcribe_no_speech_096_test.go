package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC 0.96.0 §8.6.13: dir2mcp_transcribe follows the no-speech guard. With
// retranscribe=false it sends no decode request for a recording that has a
// stored no-speech result under the current STT derivation identity, and it
// returns a success result with indexed false and no segments. retranscribe
// =true is exempt from the guard: the recording is decoded again. When the
// new decode is again a no-speech result, the tool returns the same success
// result and an earlier transcript is retired. dir2mcp_transcribe_and_ask has
// no retranscribe argument and behaves as retranscribe=false.

// sttUpstream096 is an STT upstream that counts its requests. It answers with
// speech while speech is true, and with the measured no-speech body otherwise.
type sttUpstream096 struct {
	srv    *httptest.Server
	calls  atomic.Int32
	speech atomic.Bool
}

func newSTTUpstream096(t *testing.T) *sttUpstream096 {
	t.Helper()
	u := &sttUpstream096{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		u.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if u.speech.Load() {
			_, _ = io.WriteString(w, `{"segments":[{"start":1,"end":2,"text":"alpha"},{"start":3,"end":4,"text":"beta"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"text":"","segments":[]}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// transcribeEnv096 is one MCP server over a real store and the counting STT
// upstream, with the audio document voice.wav. The STT profile resolves, so
// the transcript cache is keyed on the bytes and the STT identity.
type transcribeEnv096 struct {
	cfg      config.Config
	store    *store.SQLiteStore
	upstream *sttUpstream096
	url      string
	session  string
	nextID   int
}

func newTranscribeEnv096(t *testing.T) *transcribeEnv096 {
	t.Helper()
	cfg, st, _ := setupMCPToolStore(t, "voice.wav", "audio", []byte("RIFF0000WAVEfmt data"))
	up := newSTTUpstream096(t)
	cfg = withMistralUpstream(t, cfg, "mistral-ocr", up.srv.URL)
	retriever := &askAudioRetrieverStub{askResult: model.AskResult{Answer: "no evidence"}}
	srv := httptest.NewServer(mcp.NewServer(cfg, retriever, mcp.WithStore(st)).Handler())
	t.Cleanup(srv.Close)
	url := srv.URL + cfg.MCPPath
	return &transcribeEnv096{cfg: cfg, store: st, upstream: up, url: url, session: initializeSession(t, url), nextID: 100}
}

// call runs one tool call and returns its structured content. It fails the
// test when the tool reports an error.
func (e *transcribeEnv096) call(t *testing.T, tool string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	e.nextID++
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, e.nextID, tool, argsJSON)
	resp := postRPC(t, e.url, e.session, body)
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var envelope struct {
		Result struct {
			IsError           bool                   `json:"isError"`
			StructuredContent map[string]interface{} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode %s response: %v (body=%s)", tool, err, raw)
	}
	if envelope.Result.IsError {
		t.Fatalf("%s returned an error, want a success result: %s", tool, raw)
	}
	return envelope.Result.StructuredContent
}

// assertNoSpeechTranscribeResult checks the success result of a no-speech
// recording and its transcribed_now value.
func assertNoSpeechTranscribeResult(t *testing.T, sc map[string]interface{}, wantNow bool, when string) {
	t.Helper()
	if indexed, _ := sc["indexed"].(bool); indexed {
		t.Errorf("%s: indexed = true, want false", when)
	}
	if segs, _ := sc["segments"].([]interface{}); len(segs) != 0 {
		t.Errorf("%s: segments = %v, want none", when, segs)
	}
	if transcribed, _ := sc["transcribed"].(bool); transcribed {
		t.Errorf("%s: transcribed = true, want false", when)
	}
	if now, _ := sc["transcribed_now"].(bool); now != wantNow {
		t.Errorf("%s: transcribed_now = %v, want %v (true only when a provider request was sent)", when, now, wantNow)
	}
	assertCanonicalToolPayload(t, "transcribe.json", sc)
}

func (e *transcribeEnv096) doc(t *testing.T) model.Document {
	t.Helper()
	doc, err := e.store.GetDocumentByPath(context.Background(), "voice.wav")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	return doc
}

// assertNoSpeechState checks the state that a scan writes for a recording
// with no speech and no other representation: the row is skipped as
// no_speech, and the no-speech record holds the STT identity.
func (e *transcribeEnv096) assertNoSpeechState(t *testing.T, when string) {
	t.Helper()
	if doc := e.doc(t); doc.Status != "skipped" || doc.SkipReason != model.SkipReasonNoSpeech {
		t.Errorf("%s: document row status=%q skip_reason=%q, want skipped/no_speech (what a scan writes)", when, doc.Status, doc.SkipReason)
	}
	if rec, err := e.store.GetSetting(context.Background(), store.NoSpeechIdentitySettingKey("voice.wav")); err != nil || rec == "" {
		t.Errorf("%s: no-speech record = %q, %v; want the STT identity that found no speech", when, rec, err)
	}
}

func (e *transcribeEnv096) repTypes(t *testing.T) []string {
	t.Helper()
	types, err := e.store.RepresentationTypesByPath(context.Background(), "voice.wav")
	if err != nil {
		t.Fatalf("RepresentationTypesByPath: %v", err)
	}
	return types
}

// TestTranscribe_NoSpeechGuardAndRetranscribe: the first call decodes and
// finds no speech. A second call with retranscribe=false reads the stored
// result and sends no request. A call with retranscribe=true decodes again,
// with a resolved STT profile, and returns the same success result.
//
// Mutants killed: removing a cache path that is not the one ingest wrote (no
// new decode on retranscribe=true), and reporting transcribed_now from that
// path.
func TestTranscribe_NoSpeechGuardAndRetranscribe(t *testing.T) {
	env := newTranscribeEnv096(t)
	args := map[string]interface{}{"rel_path": "voice.wav", "timestamps": true}

	assertNoSpeechTranscribeResult(t, env.call(t, "dir2mcp_transcribe", args), true, "first call")
	if n := env.upstream.calls.Load(); n != 1 {
		t.Fatalf("decode requests after the first call = %d, want 1", n)
	}
	env.assertNoSpeechState(t, "after the first decode")

	assertNoSpeechTranscribeResult(t, env.call(t, "dir2mcp_transcribe", args), false, "retranscribe=false")
	if n := env.upstream.calls.Load(); n != 1 {
		t.Fatalf("decode requests after retranscribe=false = %d, want 1: a stored no-speech result is not decoded again", n)
	}

	args["retranscribe"] = true
	assertNoSpeechTranscribeResult(t, env.call(t, "dir2mcp_transcribe", args), true, "retranscribe=true")
	if n := env.upstream.calls.Load(); n != 2 {
		t.Fatalf("decode requests after retranscribe=true = %d, want 2: an explicit retranscribe decodes again", n)
	}
}

// TestTranscribeAndAsk_NoSpeechSendsNoRequest: dir2mcp_transcribe_and_ask has
// no retranscribe argument and behaves as retranscribe=false, so a stored
// no-speech result is not decoded again.
func TestTranscribeAndAsk_NoSpeechSendsNoRequest(t *testing.T) {
	env := newTranscribeEnv096(t)
	env.call(t, "dir2mcp_transcribe", map[string]interface{}{"rel_path": "voice.wav"})
	if n := env.upstream.calls.Load(); n != 1 {
		t.Fatalf("decode requests after the first call = %d, want 1", n)
	}

	sc := env.call(t, "dir2mcp_transcribe_and_ask", map[string]interface{}{"rel_path": "voice.wav", "question": "what is said?", "k": 5})
	if n := env.upstream.calls.Load(); n != 1 {
		t.Fatalf("decode requests after transcribe_and_ask = %d, want 1: it behaves as retranscribe=false", n)
	}
	if transcribed, _ := sc["transcribed"].(bool); transcribed {
		t.Errorf("transcribed = true, want false")
	}
	if now, _ := sc["transcribed_now"].(bool); now {
		t.Errorf("transcribed_now = true, want false: no provider request was sent")
	}
}

// TestTranscribe_RetranscribeThatFindsNoSpeechRetiresTheTranscript: the first
// call decodes speech and indexes a transcript. A call with retranscribe=true
// then finds no speech: the earlier transcript is retired, the no-speech
// record is stored, and the document row is what a scan writes for a
// recording with no other representation (skipped, no_speech).
//
// Mutant killed: returning the empty result and leaving the earlier
// transcript searchable.
func TestTranscribe_RetranscribeThatFindsNoSpeechRetiresTheTranscript(t *testing.T) {
	env := newTranscribeEnv096(t)
	env.upstream.speech.Store(true)
	sc := env.call(t, "dir2mcp_transcribe", map[string]interface{}{"rel_path": "voice.wav"})
	if indexed, _ := sc["indexed"].(bool); !indexed {
		t.Fatalf("first call: indexed = false, want a transcript; the fixture is wrong")
	}
	if now, _ := sc["transcribed_now"].(bool); !now {
		t.Errorf("first call: transcribed_now = false, want true")
	}
	if types := env.repTypes(t); len(types) == 0 {
		t.Fatalf("first call stored no representation; the fixture is wrong")
	}

	// A second call with no retranscribe reads the cached transcript.
	sc = env.call(t, "dir2mcp_transcribe", map[string]interface{}{"rel_path": "voice.wav"})
	if now, _ := sc["transcribed_now"].(bool); now {
		t.Errorf("cached call: transcribed_now = true, want false")
	}
	if n := env.upstream.calls.Load(); n != 1 {
		t.Fatalf("decode requests after the cached call = %d, want 1", n)
	}

	env.upstream.speech.Store(false)
	sc = env.call(t, "dir2mcp_transcribe", map[string]interface{}{"rel_path": "voice.wav", "retranscribe": true, "timestamps": true})
	assertNoSpeechTranscribeResult(t, sc, true, "retranscribe=true with no speech")
	if n := env.upstream.calls.Load(); n != 2 {
		t.Fatalf("decode requests after retranscribe=true = %d, want 2", n)
	}
	if types := env.repTypes(t); len(types) != 0 {
		t.Errorf("live representations after the no-speech retranscribe = %v, want none", types)
	}
	env.assertNoSpeechState(t, "after the no-speech retranscribe")
}

// TestTranscribe_SpeechAfterNoSpeechRestoresTheDocument: a recording whose
// stored outcome is no speech is decoded again with retranscribe=true and now
// holds speech. The transcript is indexed, the no-speech record goes, and the
// document row is ok again.
func TestTranscribe_SpeechAfterNoSpeechRestoresTheDocument(t *testing.T) {
	env := newTranscribeEnv096(t)
	env.call(t, "dir2mcp_transcribe", map[string]interface{}{"rel_path": "voice.wav"})
	env.assertNoSpeechState(t, "after the first decode")

	env.upstream.speech.Store(true)
	sc := env.call(t, "dir2mcp_transcribe", map[string]interface{}{"rel_path": "voice.wav", "retranscribe": true})
	if indexed, _ := sc["indexed"].(bool); !indexed {
		t.Errorf("indexed = false after a decode with speech")
	}
	if rec, err := env.store.GetSetting(context.Background(), store.NoSpeechIdentitySettingKey("voice.wav")); err == nil && rec != "" {
		t.Errorf("no-speech record %q is still stored after a decode with speech", rec)
	}
	if doc := env.doc(t); doc.Status != "ok" || doc.SkipReason != "" {
		t.Errorf("document row after a decode with speech: status=%q skip_reason=%q, want ok", doc.Status, doc.SkipReason)
	}
	if types := env.repTypes(t); len(types) == 0 {
		t.Errorf("no live representation after a decode with speech")
	}
}
