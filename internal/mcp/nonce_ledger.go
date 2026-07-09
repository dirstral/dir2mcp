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

// classifyNonce performs a read-only classification of a nonce for a given
// request without mutating the ledger. It is used before the facilitator verify
// round-trip so an invalid/transient verify never creates a ledger entry.
func (s *Server) classifyNonce(nonce, requestKey string) nonceDecision {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return nonceDecision{kind: nonceProceed}
	}
	s.nonceMu.Lock()
	defer s.nonceMu.Unlock()
	s.pruneNonceLedgerLocked(time.Now().UTC())
	e, ok := s.nonceLedger[nonce]
	if !ok {
		return nonceDecision{kind: nonceProceed}
	}
	if e.RequestKey != requestKey {
		return nonceDecision{kind: nonceReplay}
	}
	if e.Consumed {
		return nonceDecision{kind: nonceConsumed, executionKey: e.ExecutionKey}
	}
	return nonceDecision{kind: nonceProceed, executionKey: e.ExecutionKey}
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
	s.pruneNonceLedgerLocked(now)
	if e, ok := s.nonceLedger[nonce]; ok {
		if e.RequestKey != requestKey {
			s.nonceMu.Unlock()
			return nonceDecision{kind: nonceReplay}
		}
		if e.Consumed {
			s.nonceMu.Unlock()
			return nonceDecision{kind: nonceConsumed, executionKey: e.ExecutionKey}
		}
		// Existing reservation for the same request: keep it, refresh expiry.
		e.ExpiresAt = expiresAt
		e.UpdatedAt = now
		s.nonceLedger[nonce] = e
		rec := nonceRecord(nonce, e)
		s.nonceMu.Unlock()
		s.persistNonce(rec)
		return nonceDecision{kind: nonceProceed, executionKey: executionKey}
	}
	entry := nonceLedgerEntry{
		RequestKey:   requestKey,
		ExecutionKey: executionKey,
		Consumed:     false,
		ExpiresAt:    expiresAt,
		UpdatedAt:    now,
	}
	s.nonceLedger[nonce] = entry
	rec := nonceRecord(nonce, entry)
	s.nonceMu.Unlock()
	s.persistNonce(rec)
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
	s.persistNonce(rec)
}

// rollbackNonce releases a reservation that never durably settled (e.g. the
// gated tool returned an error, so no payment was captured). A consumed nonce is
// never rolled back.
func (s *Server) rollbackNonce(nonce string) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return
	}
	s.nonceMu.Lock()
	e, ok := s.nonceLedger[nonce]
	if !ok || e.Consumed {
		s.nonceMu.Unlock()
		return
	}
	delete(s.nonceLedger, nonce)
	s.nonceMu.Unlock()
	s.deletePersistedNonce(nonce)
}

// pruneNonceLedgerLocked evicts expired entries and enforces the entry cap.
// Caller must hold nonceMu. It intentionally does not touch the store; expired
// persisted rows are cleaned by the periodic sweeper and on startup.
func (s *Server) pruneNonceLedgerLocked(now time.Time) {
	for nonce, e := range s.nonceLedger {
		if !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(now) {
			delete(s.nonceLedger, nonce)
		}
	}
	maxItems := s.nonceMaxItems
	if maxItems <= 0 {
		maxItems = nonceLedgerMaxEntries
	}
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

func (s *Server) persistNonce(rec storepkg.MCPNonceLedgerRecord) {
	store, ok := s.store.(nonceLedgerPersistenceStore)
	if !ok || store == nil {
		return
	}
	if err := store.UpsertMCPNonceLedger(context.Background(), rec); err != nil {
		s.emitPaymentEvent("warning", "nonce_ledger_persist_failed", map[string]interface{}{
			"err": err.Error(),
		})
	}
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
	s.pruneNonceLedgerLocked(now)
	s.nonceMu.Unlock()
	for _, nonce := range expired {
		s.deletePersistedNonce(nonce)
	}
}

// sweepExpiredNonces removes expired in-memory and persisted ledger entries. It
// is invoked from the periodic payment-outcome cleanup loop.
func (s *Server) sweepExpiredNonces(now time.Time) {
	store, _ := s.store.(nonceLedgerPersistenceStore)
	var toDelete []string
	s.nonceMu.Lock()
	for nonce, e := range s.nonceLedger {
		if !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(now) {
			delete(s.nonceLedger, nonce)
			toDelete = append(toDelete, nonce)
		}
	}
	s.nonceMu.Unlock()
	if store == nil {
		return
	}
	for _, nonce := range toDelete {
		s.deletePersistedNonce(nonce)
	}
}
