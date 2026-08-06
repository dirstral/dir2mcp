package index

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/provider"
)

type IndexedFile struct {
	// Path specifies the filesystem location where the index should be
	// persisted and restored. The previous version of this struct also
	// contained a Name field which was only ever used in struct literals
	// for human readability. The field was not referenced anywhere in the
	// package or exported APIs, so it has been removed to avoid dead code.
	Path  string
	Index model.Index
}

type PersistenceManager struct {
	indices  []IndexedFile
	interval time.Duration
	onError  func(error)

	// saveMu must be held while iterating over indices and invoking
	// Index.Save. persistence.Start spawns a goroutine that periodically
	// calls SaveAll, and users may call SaveAll/StopAndSave manually as
	// well; serializing the calls protects indices that are not themselves
	// safe for concurrent Save invocations.
	saveMu sync.Mutex

	stateMu sync.Mutex
	running bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func NewPersistenceManager(indices []IndexedFile, interval time.Duration, onError func(error)) *PersistenceManager {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &PersistenceManager{
		indices:  indices,
		interval: interval,
		onError:  onError,
	}
}

// LoadAll invokes Load on every registered index, checking the provided
// context before and after each call. Note that LoadAll holds the same
// saveMu mutex used by SaveAll; this prevents concurrent loads and saves
// (including the ticker goroutine started by Start) from running at the
// same time. Callers should therefore expect LoadAll to block briefly if a
// save is in progress, but it is otherwise safe to call after Start. The
// lock is released when the method returns so the ticker can resume.
func (m *PersistenceManager) LoadAll(ctx context.Context) error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	for _, idx := range m.indices {
		if idx.Index == nil {
			continue
		}
		// always check the context *before* doing any work, so callers can
		// bail out early if cancellation has already been requested.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Persistence is an optional capability (issue #247): networked
		// backends (Qdrant, pgvector) own their storage and don't implement
		// Persistable, so they are silently skipped here.
		p, ok := idx.Index.(model.Persistable)
		if !ok {
			continue
		}
		if err := p.Load(ctx, idx.Path); err != nil {
			return err
		}

		// check again after the call in case the context was cancelled
		// while the load was in progress; we can't interrupt the load
		// itself, but callers still need to see the error promptly.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return nil
}

// AutosaveTicker is an optional capability (issue #429 C-a): an index that can
// throttle its periodic autosave, skipping the full snapshot/compaction when the
// accumulated mutations are below a threshold and the max interval has not
// elapsed. The autosave ticker prefers it; StopAndSave/SaveAll always use the
// force path (Save) so shutdown durability is unaffected. Backends that do not
// implement it fall back to Save on every tick (the pre-C-a behavior).
type AutosaveTicker interface {
	AutosaveTick(ctx context.Context, path string) error
}

// autosaveTick is the periodic-save path invoked by the ticker goroutine. Unlike
// SaveAll (the force path used by StopAndSave), it routes each index through its
// AutosaveTicker capability when available, so a long ingest doesn't rewrite the
// whole index on every tick (issue #429 C-a). It shares saveMu with SaveAll/Load
// so saves never overlap.
func (m *PersistenceManager) autosaveTick() error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	var combined error
	for _, idx := range m.indices {
		if idx.Index == nil {
			continue
		}
		p, ok := idx.Index.(model.Persistable)
		if !ok {
			continue
		}
		var err error
		if t, ok := idx.Index.(AutosaveTicker); ok {
			err = t.AutosaveTick(context.Background(), idx.Path)
		} else {
			err = p.Save(context.Background(), idx.Path)
		}
		if err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func (m *PersistenceManager) SaveAll() error {
	// protect against concurrent callers; the underlying model.Index
	// implementations are not required to be goroutine‑safe so we serialize
	// accesses here. callers such as the ticker goroutine and external
	// StopAndSave/SaveAll invocations all use this same lock.
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	var combined error
	for _, idx := range m.indices {
		if idx.Index == nil {
			continue
		}
		// Save is an optional capability (issue #247); skip backends that own
		// their own persistence.
		p, ok := idx.Index.(model.Persistable)
		if !ok {
			continue
		}
		if err := p.Save(context.Background(), idx.Path); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

// EnsureIdentity reconciles an index's recorded corpus-lifetime embed identity
// (SPEC 8.1.4) with the configured one, per index (issue #247). When the index
// is fresh (empty recorded identity) or the recorded identity is INCOMPATIBLE
// with the configured one, the index is Reset to the configured identity —
// discarding any vectors built under a different embed provider/model/endpoint/
// dimension so vector spaces are never silently mixed. A compatible recording is
// a no-op. This complements the process-level config.VerifyEmbedIdentity check,
// which fails the startup before any index work when a populated corpus's
// snapshot identity changed.
//
// Compatibility is provider.EmbedIdentityMatches — the SAME comparison
// VerifyEmbedIdentity uses — not string equality (issue #705). The two must
// agree: VerifyEmbedIdentity migrates a legacy recording (the 8.1.4 field-count
// ladder, and the blank-model grace) and lets startup proceed, so a byte
// comparison here would answer "different" for exactly those migrated corpora
// and silently Reset — a full, unannounced re-embed of a corpus the operator was
// just told was compatible. Reset stays reserved for a genuine mismatch, which
// VerifyEmbedIdentity has already reported as CONFIG_INVALID, and for a fresh
// index (recorded == ""), where Reset is what RECORDS the identity.
func EnsureIdentity(ctx context.Context, idx model.Index, configuredIdentity string) error {
	if idx == nil {
		return nil
	}
	recorded, err := idx.Identity(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(recorded) != "" && provider.EmbedIdentityMatches(recorded, configuredIdentity) {
		return nil
	}
	return idx.Reset(ctx, configuredIdentity)
}

func (m *PersistenceManager) Start(ctx context.Context) {
	if len(m.indices) == 0 {
		return
	}

	m.stateMu.Lock()
	if m.running {
		m.stateMu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.running = true
	// increment the wait group while we still hold stateMu so that
	// StopAndSave cannot observe a zero count and return early. this
	// mirrors the original deferred Done in the goroutine below.
	m.wg.Add(1)
	m.stateMu.Unlock()

	go func() {
		defer m.wg.Done()
		defer func() {
			m.stateMu.Lock()
			m.cancel = nil
			m.running = false
			m.stateMu.Unlock()
		}()

		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := m.autosaveTick(); err != nil {
					m.emitError(err)
				}
			}
		}
	}()
}

// StopAndSave cancels any running autosave goroutine and waits for it
// to exit before performing a final SaveAll. The provided context is used to
// bound the wait; if it expires the method returns ctx.Err() and the final
// save may not occur. This prevents callers (such as CLI shutdown hooks)
// from blocking forever on uncooperative indices or hung goroutines.
func (m *PersistenceManager) StopAndSave(ctx context.Context) error {
	m.stateMu.Lock()
	cancel := m.cancel
	m.cancel = nil
	m.stateMu.Unlock()

	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// normal
	case <-ctx.Done():
		return ctx.Err()
	}

	return m.SaveAll()
}

func (m *PersistenceManager) emitError(err error) {
	if err == nil {
		return
	}
	if m.onError != nil {
		m.onError(err)
	}
}
