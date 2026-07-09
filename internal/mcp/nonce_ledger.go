package mcp

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	storepkg "github.com/dirstral/dir2mcp/internal/store"
)

const (
	// nonceLedgerDefaultTTL is the floor durability window for a ledger entry.
	// The effective TTL is the greater of this, the matched maxTimeoutSeconds,
	// and the time remaining until the authorization validBefore — so a consumed
	// nonce survives at least its full validity window (per the adapter spec),
	// after which the payment is independently time-expired and cannot be
	// replayed regardless.
	nonceLedgerDefaultTTL = 15 * time.Minute
	// nonceLedgerMaxTTL caps how far in the future an entry may be retained so a
	// hostile validBefore cannot pin an entry indefinitely.
	nonceLedgerMaxTTL = 24 * time.Hour
	// nonceLedgerMaxEntries bounds the in-memory ledger; oldest entries are
	// evicted first once the cap is exceeded.
	nonceLedgerMaxEntries = 20000
)

// nonceLedgerEntry is the in-memory single-use replay ledger record for one
// authorization nonce. Consumed=false is a reservation (blocks concurrent
// replays while settle is in flight); Consumed=true is durable spend.
type nonceLedgerEntry struct {
	RequestKey   string
	ExecutionKey string
	Consumed     bool
	ExpiresAt    time.Time
	UpdatedAt    time.Time
}

type nonceLedgerPersistenceStore interface {
	UpsertMCPNonceLedger(ctx context.Context, rec storepkg.MCPNonceLedgerRecord) error
	DeleteMCPNonceLedger(ctx context.Context, nonce string) error
	ListMCPNonceLedger(ctx context.Context) ([]storepkg.MCPNonceLedgerRecord, error)
	GetMCPNonceLedger(ctx context.Context, nonce string) (storepkg.MCPNonceLedgerRecord, bool, error)
	DeleteExpiredMCPNonceLedger(ctx context.Context, nowUnix int64) error
}

type nonceDecisionKind int

const (
	// nonceProceed: the nonce is unseen for this request, or is an in-flight
	// reservation for the same request — the caller may proceed.
	nonceProceed nonceDecisionKind = iota
	// nonceReplay: the nonce was already recorded for a DIFFERENT logical request
	// — a replay/misuse attempt that must be rejected via the `rejected` branch.
	nonceReplay
	// nonceConsumed: the nonce was durably consumed for THIS request — an
	// idempotent retry whose recorded outcome must be re-surfaced (never
	// re-executed or re-charged).
	nonceConsumed
	// nonceError: the durable ledger could not be consulted or written (a store
	// is configured but the read/write failed). The single-use guarantee cannot
	// be proven, so the caller MUST fail closed with a retryable error rather
	// than admit a possible replay.
	nonceError
)

type nonceDecision struct {
	kind         nonceDecisionKind
	executionKey string
}

// nonceLedgerTTL computes the durability window for a nonce given the matched
// maxTimeoutSeconds and (optionally) the authorization validBefore.
func nonceLedgerTTL(parsed parsedPaymentPayload, maxTimeoutSeconds int, now time.Time) time.Duration {
	ttl := nonceLedgerDefaultTTL
	if maxTimeoutSeconds > 0 {
		if d := time.Duration(maxTimeoutSeconds) * time.Second; d > ttl {
			ttl = d
		}
	}
	if parsed.HasWindow {
		if until := time.Unix(parsed.ValidBefore, 0).UTC().Sub(now.UTC()); until > ttl {
			ttl = until
		}
	}
	if ttl > nonceLedgerMaxTTL {
		ttl = nonceLedgerMaxTTL
	}
	return ttl
}

// classifyEntry maps a ledger entry for a nonce to a decision for requestKey.
func classifyEntry(e nonceLedgerEntry, requestKey string) nonceDecision {
	if e.RequestKey != requestKey {
		return nonceDecision{kind: nonceReplay}
	}
	if e.Consumed {
		return nonceDecision{kind: nonceConsumed, executionKey: e.ExecutionKey}
	}
	return nonceDecision{kind: nonceProceed, executionKey: e.ExecutionKey}
}

// lookupNonceLocked returns the authoritative ledger entry for a nonce: the
// in-memory cache when present, else the durable store (the source of truth) so
// an entry evicted from the cache under the item cap is still enforced. It
// hydrates the cache on a store hit. A store read error returns errored=true so
// the caller fails closed rather than admitting a possible replay. Caller must
// hold nonceMu.
func (s *Server) lookupNonceLocked(nonce string, now time.Time) (entry nonceLedgerEntry, found bool, errored bool) {
	if e, ok := s.nonceLedger[nonce]; ok {
		return e, true, false
	}
	store, ok := s.store.(nonceLedgerPersistenceStore)
	if !ok || store == nil {
		return nonceLedgerEntry{}, false, false
	}
	rec, ok, err := store.GetMCPNonceLedger(context.Background(), nonce)
	if err != nil {
		s.emitPaymentEvent("warning", "nonce_ledger_read_failed", map[string]interface{}{"err": err.Error()})
		return nonceLedgerEntry{}, false, true
	}
	if !ok {
		return nonceLedgerEntry{}, false, false
	}
	if !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now) {
		return nonceLedgerEntry{}, false, false
	}
	e := nonceLedgerEntry{
		RequestKey:   rec.RequestKey,
		ExecutionKey: rec.ExecutionKey,
		Consumed:     rec.Consumed,
		ExpiresAt:    rec.ExpiresAt.UTC(),
		UpdatedAt:    rec.UpdatedAt.UTC(),
	}
	s.nonceLedger[nonce] = e // hydrate cache
	return e, true, false
}

// classifyNonce performs a read-only classification of a nonce for a given
// request without mutating the ledger (beyond hydrating the cache from the
// durable store). It is used before the facilitator verify round-trip so an
// invalid/transient verify never creates a ledger entry.
func (s *Server) classifyNonce(nonce, requestKey string) nonceDecision {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return nonceDecision{kind: nonceProceed}
	}
	now := time.Now().UTC()
	s.nonceMu.Lock()
	defer s.nonceMu.Unlock()
	s.pruneExpiredNonceLocked(now)
	e, ok, errored := s.lookupNonceLocked(nonce, now)
	if errored {
		return nonceDecision{kind: nonceError}
	}
	if !ok {
		return nonceDecision{kind: nonceProceed}
	}
	return classifyEntry(e, requestKey)
}

// reserveNonce atomically reserves a nonce for a request. It is the enforcement
// point for cross-request replay: a concurrent request presenting the same nonce
// with a different logical request loses the race and is classified nonceReplay.
func (s *Server) reserveNonce(nonce, requestKey, executionKey string, expiresAt time.Time) nonceDecision {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return nonceDecision{kind: nonceProceed}
	}
	now := time.Now().UTC()

	s.nonceMu.Lock()
	s.pruneExpiredNonceLocked(now)
	e, ok, errored := s.lookupNonceLocked(nonce, now)
	if errored {
		s.nonceMu.Unlock()
		return nonceDecision{kind: nonceError}
	}
	if ok {
		if e.RequestKey != requestKey {
			s.nonceMu.Unlock()
			return nonceDecision{kind: nonceReplay}
		}
		if e.Consumed {
			s.nonceMu.Unlock()
			return nonceDecision{kind: nonceConsumed, executionKey: e.ExecutionKey}
		}
		// Existing reservation for the same request: keep it, refresh expiry. The
		// durable record already enforces single-use, so a failed expiry refresh
		// is non-fatal — log and proceed rather than reject a legitimate retry.
		e.ExpiresAt = expiresAt
		e.UpdatedAt = now
		s.nonceLedger[nonce] = e
		rec := nonceRecord(nonce, e)
		s.nonceMu.Unlock()
		if err := s.persistNonce(rec); err != nil {
			s.emitPaymentEvent("warning", "nonce_ledger_persist_failed", map[string]interface{}{"err": err.Error()})
		}
		return nonceDecision{kind: nonceProceed, executionKey: executionKey}
	}
	// New reservation. Without a durable store the in-memory ledger is the only
	// record, so evicting an unexpired entry to make room would open a replay
	// window — fail closed instead of silently dropping a live nonce.
	if !s.hasNonceStore() && len(s.nonceLedger) >= s.nonceCap() {
		s.nonceMu.Unlock()
		s.emitPaymentEvent("warning", "nonce_ledger_capacity_exhausted", map[string]interface{}{"cap": s.nonceCap()})
		return nonceDecision{kind: nonceError}
	}
	entry := nonceLedgerEntry{
		RequestKey:   requestKey,
		ExecutionKey: executionKey,
		Consumed:     false,
		ExpiresAt:    expiresAt,
		UpdatedAt:    now,
	}
	s.nonceLedger[nonce] = entry
	s.enforceNonceCapLocked() // safe: store-backed, so evicted entries are found on miss
	rec := nonceRecord(nonce, entry)
	s.nonceMu.Unlock()
	// Fail closed on persistence failure: a reservation that cannot be made
	// durable must not proceed to execution/settlement, since it would be lost on
	// restart and replayable. Nothing has been charged yet, so rolling back is
	// safe.
	if err := s.persistNonce(rec); err != nil && s.hasNonceStore() {
		s.emitPaymentEvent("warning", "nonce_ledger_persist_failed", map[string]interface{}{"err": err.Error()})
		s.nonceMu.Lock()
		if cur, ok := s.nonceLedger[nonce]; ok && !cur.Consumed && cur.ExecutionKey == executionKey {
			delete(s.nonceLedger, nonce)
		}
		s.nonceMu.Unlock()
		return nonceDecision{kind: nonceError}
	}
	return nonceDecision{kind: nonceProceed, executionKey: executionKey}
}

// commitNonce durably marks a reservation consumed on settlement success.
func (s *Server) commitNonce(nonce, requestKey, executionKey string, expiresAt time.Time) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return
	}
	now := time.Now().UTC()
	s.nonceMu.Lock()
	entry, ok := s.nonceLedger[nonce]
	if !ok {
		entry = nonceLedgerEntry{RequestKey: requestKey, ExecutionKey: executionKey, ExpiresAt: expiresAt}
	}
	entry.Consumed = true
	entry.UpdatedAt = now
	if entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = expiresAt
	}
	if strings.TrimSpace(entry.ExecutionKey) == "" {
		entry.ExecutionKey = executionKey
	}
	s.nonceLedger[nonce] = entry
	rec := nonceRecord(nonce, entry)
	s.nonceMu.Unlock()
	// Settlement already succeeded (funds captured), so we cannot reject the
	// request here. Durably record the consumed nonce with a bounded retry; if it
	// ultimately fails, emit a CRITICAL event so the durability gap is visible
	// (a restart before a later write persists it could admit one replay within
	// the validity window). The in-memory entry still blocks replays for the
	// process lifetime.
	if err := s.persistNonceWithRetry(rec, 3); err != nil && s.hasNonceStore() {
		s.emitPaymentEvent("critical", "nonce_ledger_consume_persist_failed", map[string]interface{}{
			"err":   err.Error(),
			"nonce": redactNonce(nonce),
		})
	}
}

// releaseNonceReservation marks a reservation as no-longer-in-flight after a
// non-settled outcome (the gated tool errored, so no payment was captured). It
// does NOT delete the entry: the (nonce, requestKey) binding is retained until
// expiry so the single-use nonce cannot be reused for a DIFFERENT request (a
// cross-request replay), while the SAME (nonce, request) may still be retried
// against the surviving reservation. A consumed nonce is never touched.
//
// Deleting the entry here would drop the binding and let a later request present
// the same nonce with a different request key and pass classifyNonce as fresh.
func (s *Server) releaseNonceReservation(nonce string) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return
	}
	// No state change is required — the reservation created by reserveNonce
	// already blocks different-request replays and admits same-request retries.
	// This is a named no-op so the call sites document the retain-until-expiry
	// contract rather than silently omitting a rollback.
}

// nonceCap is the in-memory ledger entry cap.
func (s *Server) nonceCap() int {
	if s.nonceMaxItems > 0 {
		return s.nonceMaxItems
	}
	return nonceLedgerMaxEntries
}

// hasNonceStore reports whether a durable ledger store is configured. When true,
// the persisted ledger is the source of truth and cache eviction is safe
// (evicted entries are re-read from the store on a miss).
func (s *Server) hasNonceStore() bool {
	store, ok := s.store.(nonceLedgerPersistenceStore)
	return ok && store != nil
}

// pruneExpiredNonceLocked drops only expired entries from the cache. Caller must
// hold nonceMu. Expired persisted rows are reclaimed by the periodic sweeper.
func (s *Server) pruneExpiredNonceLocked(now time.Time) {
	for nonce, e := range s.nonceLedger {
		if !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(now) {
			delete(s.nonceLedger, nonce)
		}
	}
}

// enforceNonceCapLocked bounds the in-memory cache by evicting the oldest
// entries once over the cap. Caller must hold nonceMu. This is only safe to call
// when a durable store backs the ledger (an evicted entry — including a consumed
// one — is re-read from the store on a miss, so single-use is preserved). With
// no store, reserveNonce fails closed before admitting past the cap instead.
func (s *Server) enforceNonceCapLocked() {
	if !s.hasNonceStore() {
		return
	}
	maxItems := s.nonceCap()
	if len(s.nonceLedger) <= maxItems {
		return
	}
	type kv struct {
		nonce string
		at    time.Time
	}
	entries := make([]kv, 0, len(s.nonceLedger))
	for nonce, e := range s.nonceLedger {
		entries = append(entries, kv{nonce: nonce, at: e.UpdatedAt})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	for i := 0; i < len(entries)-maxItems; i++ {
		delete(s.nonceLedger, entries[i].nonce)
	}
}

func nonceRecord(nonce string, e nonceLedgerEntry) storepkg.MCPNonceLedgerRecord {
	updatedAt := e.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	expiresAt := e.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = updatedAt.Add(nonceLedgerDefaultTTL)
	}
	return storepkg.MCPNonceLedgerRecord{
		Nonce:        nonce,
		RequestKey:   e.RequestKey,
		ExecutionKey: e.ExecutionKey,
		Consumed:     e.Consumed,
		ExpiresAt:    expiresAt.UTC(),
		UpdatedAt:    updatedAt.UTC(),
	}
}

// persistNonce writes a ledger record to the durable store. It returns nil when
// no store is configured (in-memory-only mode) and the store's error otherwise,
// so callers can fail closed. It no longer swallows the error — logging is the
// caller's decision based on whether the failure is fatal to the flow.
func (s *Server) persistNonce(rec storepkg.MCPNonceLedgerRecord) error {
	store, ok := s.store.(nonceLedgerPersistenceStore)
	if !ok || store == nil {
		return nil
	}
	return store.UpsertMCPNonceLedger(context.Background(), rec)
}

// persistNonceWithRetry attempts persistence up to attempts times. Used on the
// post-settlement consume path where the request cannot be rejected but the
// consumed nonce must be made durable if at all possible.
func (s *Server) persistNonceWithRetry(rec storepkg.MCPNonceLedgerRecord, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = s.persistNonce(rec); err == nil {
			return nil
		}
	}
	return err
}

// redactNonce returns a short, non-reversible tag for a nonce so ledger errors
// can reference an entry without logging the full single-use secret.
func redactNonce(nonce string) string {
	nonce = strings.TrimSpace(nonce)
	if len(nonce) <= 8 {
		return "…"
	}
	return nonce[:4] + "…" + nonce[len(nonce)-4:]
}

func (s *Server) deletePersistedNonce(nonce string) {
	store, ok := s.store.(nonceLedgerPersistenceStore)
	if !ok || store == nil {
		return
	}
	if err := store.DeleteMCPNonceLedger(context.Background(), nonce); err != nil {
		s.emitPaymentEvent("warning", "nonce_ledger_delete_failed", map[string]interface{}{
			"err": err.Error(),
		})
	}
}

// loadPersistedNonceLedger hydrates the in-memory ledger from the store on
// startup so a consumed nonce survives process restart for its validity window.
// Expired rows are dropped (and deleted from the store).
func (s *Server) loadPersistedNonceLedger() {
	store, ok := s.store.(nonceLedgerPersistenceStore)
	if !ok || store == nil {
		return
	}
	records, err := store.ListMCPNonceLedger(context.Background())
	if err != nil {
		log.Printf("warning: failed loading persisted nonce ledger: %v", err)
		return
	}
	now := time.Now().UTC()
	var expired []string
	s.nonceMu.Lock()
	for _, rec := range records {
		nonce := strings.TrimSpace(rec.Nonce)
		if nonce == "" {
			continue
		}
		if !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now) {
			expired = append(expired, nonce)
			continue
		}
		s.nonceLedger[nonce] = nonceLedgerEntry{
			RequestKey:   rec.RequestKey,
			ExecutionKey: rec.ExecutionKey,
			Consumed:     rec.Consumed,
			ExpiresAt:    rec.ExpiresAt.UTC(),
			UpdatedAt:    rec.UpdatedAt.UTC(),
		}
	}
	s.pruneExpiredNonceLocked(now)
	s.enforceNonceCapLocked()
	s.nonceMu.Unlock()
	for _, nonce := range expired {
		s.deletePersistedNonce(nonce)
	}
}

// sweepExpiredNonces removes expired in-memory and persisted ledger entries. It
// is invoked from the periodic payment-outcome cleanup loop.
func (s *Server) sweepExpiredNonces(now time.Time) {
	s.nonceMu.Lock()
	for nonce, e := range s.nonceLedger {
		if !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(now) {
			delete(s.nonceLedger, nonce)
		}
	}
	s.nonceMu.Unlock()

	store, ok := s.store.(nonceLedgerPersistenceStore)
	if !ok || store == nil {
		return
	}
	// Delete expired rows directly in the database. A DB-level sweep (rather than
	// deleting only the nonces still present in the cache) reclaims entries that
	// were evicted from the in-memory cache under the item cap but left in the
	// store — otherwise they accumulate until the next process restart.
	if err := store.DeleteExpiredMCPNonceLedger(context.Background(), now.UTC().Unix()); err != nil {
		s.emitPaymentEvent("warning", "nonce_ledger_sweep_failed", map[string]interface{}{"err": err.Error()})
	}
}
