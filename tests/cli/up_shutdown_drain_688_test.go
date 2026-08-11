package tests

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #688: `up` cancelled its run context and then returned. The initial
// ingest goroutine and the MCP transport goroutine were not in the drain group,
// so runUp could close the indexes and the store while the initial scan was
// still writing to them. These tests pin the drain: the run does not return
// until the initial ingest returns, and the shared resources are closed only
// after that.

// blockingIngestor holds its Run open until the test releases it, and records
// when Run returned. It is the stand-in for a slow initial scan that is part way
// through a store write when the operator stops the server.
type blockingIngestor struct {
	started  chan struct{}
	release  chan struct{}
	ctxSeen  atomic.Bool
	returned atomic.Bool
}

func newBlockingIngestor() *blockingIngestor {
	return &blockingIngestor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (i *blockingIngestor) Run(ctx context.Context) error {
	close(i.started)
	// Wait for the test, not for the context. A cancelled context must not be
	// enough to let shutdown continue: the scan is still inside its write.
	<-i.release
	if ctx.Err() != nil {
		i.ctxSeen.Store(true)
	}
	i.returned.Store(true)
	return nil
}

func (i *blockingIngestor) Reindex(context.Context) error { return nil }

// closeOrderStore records whether the initial ingest had already returned when
// the store was closed.
type closeOrderStore struct {
	ingest      *blockingIngestor
	closed      atomic.Bool
	closedEarly atomic.Bool
}

func (s *closeOrderStore) Init(context.Context) error                           { return nil }
func (s *closeOrderStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *closeOrderStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (s *closeOrderStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}

func (s *closeOrderStore) Close() error {
	if s.ingest != nil && !s.ingest.returned.Load() {
		s.closedEarly.Store(true)
	}
	s.closed.Store(true)
	return nil
}

// TestUpShutdownWaitsForInitialIngest proves `up` keeps running until the
// initial ingest returns, and only then closes the store the scan writes to.
//
// Before the fix the run returned as soon as the context was cancelled, which
// left the scan writing into a store that runUp was already closing.
func TestUpShutdownWaitsForInitialIngest(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")
	t.Setenv("DIR2MCP_SKIP_EMBED_PROBE", "1")

	ingestor := newBlockingIngestor()
	st := &closeOrderStore{ingest: ingestor}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return st },
		NewIngestor: func(config.Config, model.Store) (model.Ingestor, error) {
			return ingestor, nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	codeCh := make(chan int, 1)
	withWorkingDir(t, tmp, func() {
		go func() {
			codeCh <- app.RunWithContext(ctx, []string{"up", "--foreground", "--listen", "127.0.0.1:0"})
		}()

		select {
		case <-ingestor.started:
		case <-time.After(raceScaled(10 * time.Second)):
			cancel()
			close(ingestor.release)
			<-codeCh
			t.Fatal("the initial ingest never started")
		}

		// Stop the server the way a signal does, while the scan is in flight.
		cancel()

		// The run must stay inside its drain. A return here means shutdown
		// abandoned the scan.
		select {
		case code := <-codeCh:
			close(ingestor.release)
			t.Fatalf("up returned code %d while the initial ingest was still running; shutdown did not drain it (stderr=%s)",
				code, stderr.String())
		case <-time.After(raceScaled(500 * time.Millisecond)):
		}

		if st.closed.Load() {
			close(ingestor.release)
			<-codeCh
			t.Fatal("the store was closed while the initial ingest was still writing to it")
		}

		close(ingestor.release)

		select {
		case code := <-codeCh:
			if code != 0 {
				t.Fatalf("graceful stop exit code: got=%d want=0 stderr=%s", code, stderr.String())
			}
		case <-time.After(raceScaled(10 * time.Second)):
			t.Fatal("up did not return after the initial ingest finished")
		}
	})

	if !ingestor.ctxSeen.Load() {
		t.Fatal("the initial ingest did not observe the cancelled run context")
	}
	if !st.closed.Load() {
		t.Fatal("the store was never closed")
	}
	if st.closedEarly.Load() {
		t.Fatal("the store was closed before the initial ingest returned; the shutdown order is wrong")
	}
}
