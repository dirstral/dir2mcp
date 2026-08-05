package tests

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #696: session state transitions were ordered only in memory. Every mutation
// released sessionMu and THEN wrote to the store, so the store could observe
// the writes in an order that contradicted the in-memory order:
//
//  1. Request A validates the session, updates lastSeen under sessionMu,
//     releases the lock, and is descheduled before/inside UpsertMCPSession.
//  2. Request B handles DELETE for the same id, removes the in-memory entry,
//     releases the lock, and completes DeleteMCPSession.
//  3. Request A resumes; its older upsert lands AFTER the delete.
//  4. The live process still rejects the id (its memory entry is gone), so
//     nothing looks wrong. On the next restart restoreSessions reads the
//     resurrected row and the terminated session is valid again.
//
// A session that was explicitly terminated coming back to life defeats the
// point of terminating it, so these tests pin the ordering rather than the
// symptom.

// TestTerminatedSessionIsNotResurrectedByARacingTouch_696 reproduces the exact
// interleaving above and asserts the outcome that actually matters: after a
// restart, the terminated id is still terminated.
//
// The interleaving is forced through the persistence store rather than with a
// sleep. Request A is parked INSIDE UpsertMCPSession, which is a point that
// exists both before and after the fix, so this test compiles and runs against
// the unfixed source.
func TestTerminatedSessionIsNotResurrectedByARacingTouch_696(t *testing.T) {
	dir := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(dir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	paused := newPausingSessionStore(st)

	cfg := config.Config{MCPPath: "/mcp", AuthMode: "none", StateDir: dir}
	url, stop := startSessionRaceServer(t, cfg, paused)

	sessionID := initializeSession(t, url)

	// Arm the interleaving BEFORE either request is in flight.
	upsertEntered, releaseUpsert := paused.pauseNextUpsert(sessionID)
	deleteLanded := paused.watchDelete(sessionID)

	// Request A: an ordinary call that touches the session. It completes its
	// in-memory lastSeen update, then parks inside the store write.
	touchErr := make(chan error, 1)
	go func() {
		_, err := doRPC(url, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		touchErr <- err
	}()
	select {
	case <-upsertEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("request A never reached UpsertMCPSession; the fixture cannot force the #696 interleaving")
	}

	// Request B: DELETE the session while A's older write is still in flight.
	deleteErr := make(chan error, 1)
	go func() {
		deleteErr <- doDelete(url, sessionID)
	}()

	// Wait for B's delete to actually reach the store, which is what makes A's
	// pending write definitively stale.
	//
	// This is asymmetric on purpose, and it is not a "sleep and hope". Against
	// the UNFIXED code nothing serializes B behind A, so B's delete lands
	// promptly and deleteLanded always fires: the resurrection is forced, not
	// raced. Against the FIXED code B is parked behind the very serialization
	// the fix introduces, so the signal cannot arrive until A is released; the
	// timeout is this test observing that the fix is doing its job.
	select {
	case <-deleteLanded:
	case <-time.After(2 * time.Second):
	}

	close(releaseUpsert)
	if err := <-touchErr; err != nil {
		t.Fatalf("request A: %v", err)
	}
	if err := <-deleteErr; err != nil {
		t.Fatalf("request B (DELETE): %v", err)
	}

	// The persisted row is exactly what a restart reads back, so check it
	// directly before simulating the restart.
	if ids := persistedSessionIDs(t, st); contains(ids, sessionID) {
		t.Fatalf("a terminated session is still persisted (%v); an older touch overtook the DELETE and a restart will revive it (#696)", ids)
	}

	// Simulate the restart: a fresh server over the same store runs
	// restoreSessions during construction.
	stop()
	restartURL, restartStop := startSessionRaceServer(t, cfg, st)
	defer restartStop()

	status, err := doRPC(restartURL, sessionID, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
	if err != nil {
		t.Fatalf("post-restart request: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("a terminated session was accepted after restart (status %d, want 404); the DELETE was undone by an older touch write (#696)", status)
	}
}

// TestExpiredSessionIsNotResurrectedByARacingTouch_696 covers the second half
// of the bug: the same ordering can undo expiry, not just explicit DELETE.
//
// It asserts on the persisted row rather than on a restart, deliberately.
// restoreSessions re-applies the expiry rules it was configured with, so under
// an unchanged config a resurrected EXPIRED row would be filtered out again on
// load and a restart-based assertion would pass vacuously against broken code.
// The durable defect is the row surviving the expiry at all.
func TestExpiredSessionIsNotResurrectedByARacingTouch_696(t *testing.T) {
	dir := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(dir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	paused := newPausingSessionStore(st)

	// A short absolute lifetime makes the expiry fire deterministically off the
	// session's creation time, with no dependency on the idle clock.
	const maxLifetime = 150 * time.Millisecond
	cfg := config.Config{MCPPath: "/mcp", AuthMode: "none", StateDir: dir, SessionMaxLifetime: maxLifetime}
	url, stop := startSessionRaceServer(t, cfg, paused)
	defer stop()

	sessionID := initializeSession(t, url)

	upsertEntered, releaseUpsert := paused.pauseNextUpsert(sessionID)
	deleteLanded := paused.watchDelete(sessionID)

	// Request A touches while the session is still inside its lifetime, then
	// parks in the store write.
	touchErr := make(chan error, 1)
	go func() {
		_, err := doRPC(url, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		touchErr <- err
	}()
	select {
	case <-upsertEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("request A never reached UpsertMCPSession; the fixture cannot force the #696 interleaving")
	}

	// Let the absolute lifetime lapse, then make request B observe the expiry.
	time.Sleep(2 * maxLifetime)
	expireErr := make(chan error, 1)
	go func() {
		_, err := doRPC(url, sessionID, `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`)
		expireErr <- err
	}()

	// See the note in the DELETE test: fires immediately pre-fix, times out
	// post-fix because the fix serializes B behind A.
	select {
	case <-deleteLanded:
	case <-time.After(2 * time.Second):
	}

	close(releaseUpsert)
	if err := <-touchErr; err != nil {
		t.Fatalf("request A: %v", err)
	}
	if err := <-expireErr; err != nil {
		t.Fatalf("request B (expiring call): %v", err)
	}

	if ids := persistedSessionIDs(t, st); contains(ids, sessionID) {
		t.Fatalf("an expired session is still persisted (%v); an older touch write undid the expiry (#696)", ids)
	}
}

// TestConcurrentSessionTouchesStayConsistent_696 guards the other direction:
// the ordering machinery must not drop or corrupt ordinary concurrent touches,
// which are the common case. Run under -race.
//
// This one drives the plain HTTP handler rather than the SDK transport. The
// touch path under test (the session middleware calling hasActiveSession) is
// identical, and the SDK's streamable transport does not serve many concurrent
// POSTs on a single session id — that is a property of the SDK's per-session
// stream handling, unrelated to #696, and it stalls this test on unfixed and
// fixed code alike.
func TestConcurrentSessionTouchesStayConsistent_696(t *testing.T) {
	dir := t.TempDir()
	st := store.NewSQLiteStore(filepath.Join(dir, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{MCPPath: "/mcp", AuthMode: "none", StateDir: dir}
	srv := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	defer srv.Close()
	url := srv.URL + cfg.MCPPath

	sessionID := initializeSession(t, url)

	const concurrency = 16
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, err := doRPC(url, sessionID, `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{}}`)
			if err != nil {
				errs <- err
				return
			}
			if status != http.StatusOK {
				t.Errorf("concurrent touch returned status %d, want 200", status)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent touch: %v", err)
	}

	// A live session must still be persisted exactly once after the storm.
	ids := persistedSessionIDs(t, st)
	if !contains(ids, sessionID) {
		t.Fatalf("a live session was dropped from the store by concurrent touches (%v)", ids)
	}
	seen := 0
	for _, id := range ids {
		if id == sessionID {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("session persisted %d times, want exactly 1", seen)
	}
}

// pausingSessionStore wraps a real SQLiteStore and lets a test park or observe
// individual session persistence calls.
//
// It embeds the real store rather than reimplementing model.Store: the point is
// to control the ORDER of the session writes, not to fake their behaviour, and
// the restart assertion needs a genuine database underneath.
type pausingSessionStore struct {
	*store.SQLiteStore

	mu sync.Mutex
	// pauseUpsert holds the one-shot release channel for a session id whose
	// next upsert should park; upsertEntered is closed when it does park.
	pauseUpsert   map[string]chan struct{}
	upsertEntered map[string]chan struct{}
	// deleteLanded is closed AFTER the delete has actually been applied, so a
	// test that waits on it knows the row is gone rather than merely that the
	// call started.
	deleteLanded map[string]chan struct{}
}

func newPausingSessionStore(st *store.SQLiteStore) *pausingSessionStore {
	return &pausingSessionStore{
		SQLiteStore:   st,
		pauseUpsert:   make(map[string]chan struct{}),
		upsertEntered: make(map[string]chan struct{}),
		deleteLanded:  make(map[string]chan struct{}),
	}
}

// pauseNextUpsert arms a one-shot park on the next UpsertMCPSession for id. It
// returns a channel closed once that call has parked, and the channel the test
// closes to let it proceed.
func (p *pausingSessionStore) pauseNextUpsert(id string) (entered <-chan struct{}, release chan struct{}) {
	enteredCh := make(chan struct{})
	releaseCh := make(chan struct{})
	p.mu.Lock()
	p.upsertEntered[id] = enteredCh
	p.pauseUpsert[id] = releaseCh
	p.mu.Unlock()
	return enteredCh, releaseCh
}

// watchDelete returns a channel closed once a DeleteMCPSession for id has been
// applied to the underlying store.
func (p *pausingSessionStore) watchDelete(id string) <-chan struct{} {
	ch := make(chan struct{})
	p.mu.Lock()
	p.deleteLanded[id] = ch
	p.mu.Unlock()
	return ch
}

func (p *pausingSessionStore) UpsertMCPSession(ctx context.Context, sessionID string, created, lastSeen time.Time, authScope string) error {
	p.mu.Lock()
	release := p.pauseUpsert[sessionID]
	entered := p.upsertEntered[sessionID]
	if release != nil {
		// One-shot: later upserts for this id run unimpeded.
		delete(p.pauseUpsert, sessionID)
		delete(p.upsertEntered, sessionID)
	}
	p.mu.Unlock()

	if release != nil {
		close(entered)
		<-release
	}
	return p.SQLiteStore.UpsertMCPSession(ctx, sessionID, created, lastSeen, authScope)
}

func (p *pausingSessionStore) DeleteMCPSession(ctx context.Context, sessionID string) error {
	p.mu.Lock()
	landed := p.deleteLanded[sessionID]
	if landed != nil {
		delete(p.deleteLanded, sessionID)
	}
	p.mu.Unlock()

	err := p.SQLiteStore.DeleteMCPSession(ctx, sessionID)
	if landed != nil {
		close(landed)
	}
	return err
}

// startSessionRaceServer serves an MCP server over the SDK transport, which is
// the transport that implements the DELETE session-termination verb.
func startSessionRaceServer(t *testing.T, cfg config.Config, st model.Store) (baseURL string, stop func()) {
	t.Helper()
	srv := mcp.NewServer(cfg, nil, mcp.WithStore(st))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tr := mcp.NewSDKTransport(srv, ln, "", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tr.Serve(ctx, srv.Handler())
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve returned unexpected error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("timeout waiting for Serve to exit")
			}
			_ = ln.Close()
		})
	}
	return "http://" + ln.Addr().String() + cfg.MCPPath, stop
}

// doRPC and doDelete return errors instead of calling t.Fatalf because they run
// on non-test goroutines, where Fatalf is not allowed.
func doRPC(url, sessionID, body string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(protocol.MCPSessionHeader, sessionID)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func doDelete(url, sessionID string) error {
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(protocol.MCPSessionHeader, sessionID)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

func persistedSessionIDs(t *testing.T, st *store.SQLiteStore) []string {
	t.Helper()
	records, err := st.ListMCPSessions(context.Background())
	if err != nil {
		t.Fatalf("list persisted sessions: %v", err)
	}
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.ID)
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
