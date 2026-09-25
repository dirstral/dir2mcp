package tests

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// initFailStore fails Init after the database would be open, and counts Close.
type initFailStore struct {
	commandTestNoopStore
	closes atomic.Int32
}

func (s *initFailStore) Init(context.Context) error { return errors.New("migration ran out of time") }
func (s *initFailStore) Close() error {
	s.closes.Add(1)
	return nil
}

// TestUp_StoreInitFailureClosesTheStore pins that `up` releases the store when
// Init fails. Init can fail after it opened the database, and `up` returned
// without a Close, so the handle leaked in the process (a real exit freed it,
// an embedding process kept it).
func TestUp_StoreInitFailureClosesTheStore(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "")
	st := &initFailStore{}
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(config.Config) model.Store { return st },
	})
	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), []string{"up", "--listen", "127.0.0.1:0"})
	})
	if code == 0 || !strings.Contains(stderr.String(), "initialize metadata store") {
		t.Fatalf("exit=%d stderr=%q, want the store init failure", code, stderr.String())
	}
	if n := st.closes.Load(); n != 1 {
		t.Fatalf("store closed %d times after a failed Init, want 1", n)
	}
}
