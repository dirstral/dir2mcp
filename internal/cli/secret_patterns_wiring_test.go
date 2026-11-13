package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestBuildRetrieverForAsk_AppliesOperatorSecretPatterns pins the wiring of
// security.secret_patterns into the retrieval service. Before the fix no
// production site called SetSecretPatterns, so open_file screened only the
// built-in baseline and served text that an operator pattern matches.
func TestBuildRetrieverForAsk_AppliesOperatorSecretPatterns(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ticket ACME-SECRET-4711 for the vault\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "clean.txt"), []byte("nothing to hide here\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.SecretPatterns = append(append([]string(nil), cfg.SecretPatterns...), `ACME-SECRET-[0-9]{4}`)

	a := &App{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	ret, cleanup, err := a.buildRetrieverForAsk(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("buildRetrieverForAsk: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	if _, err := ret.OpenFile(context.Background(), "notes.txt", model.Span{}, 200); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("open_file on text an operator pattern matches: err = %v, want ErrForbidden", err)
	}
	got, err := ret.OpenFile(context.Background(), "clean.txt", model.Span{}, 200)
	if err != nil || !strings.Contains(got, "nothing to hide") {
		t.Fatalf("open_file on clean text = (%q, %v), want its text", got, err)
	}
}

// TestNewServingRetriever_AppliesOperatorSecretPatterns pins the same wiring
// on the `up` server path.
func TestNewServingRetriever_AppliesOperatorSecretPatterns(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ticket ACME-SECRET-4711 for the vault\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")
	cfg.SecretPatterns = append(append([]string(nil), cfg.SecretPatterns...), `ACME-SECRET-[0-9]{4}`)

	a := &App{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	ret, err := a.newServingRetriever(cfg, nil, nil, nil, servingModelClients{})
	if err != nil {
		t.Fatalf("newServingRetriever: %v", err)
	}
	if _, err := ret.OpenFile(context.Background(), "notes.txt", model.Span{}, 200); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("open_file on text an operator pattern matches: err = %v, want ErrForbidden", err)
	}

	cfg.SecretPatterns = []string{`(unclosed`}
	if _, err := a.newServingRetriever(cfg, nil, nil, nil, servingModelClients{}); err == nil || !strings.Contains(err.Error(), "compile secret patterns") {
		t.Fatalf("err = %v, want a compile secret patterns error", err)
	}
}

// TestBuildRetrieverForAsk_RejectsAnInvalidSecretPattern checks that a pattern
// that does not compile is an error, not a silently unscreened corpus.
func TestBuildRetrieverForAsk_RejectsAnInvalidSecretPattern(t *testing.T) {
	root := t.TempDir()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(root, ".dir2mcp")
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg.SecretPatterns = []string{`(unclosed`}

	a := &App{stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	_, cleanup, err := a.buildRetrieverForAsk(context.Background(), cfg, nil)
	if cleanup != nil {
		cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "compile secret patterns") {
		t.Fatalf("err = %v, want a compile secret patterns error", err)
	}
}
