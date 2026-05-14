package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

func TestOpenFile_SecretsBlocked(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("token = AAAAAAAAAAAAAAAAAAAAAAAAA"), 0o644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	_, err := svc.OpenFile(context.Background(), "docs/secret.txt", model.Span{}, 200)
	if !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected forbidden on secret content, got %v", err)
	}
}

// TestOpenFile_NonSecretsAllowed confirms that non-secret content is not
// blocked by the service.  It mirrors the setup in
// TestOpenFile_SecretsBlocked but writes a benign file and asserts the
// retrieval succeeds and returns the expected data.
func TestOpenFile_NonSecretsAllowed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "readme.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	content := "hello world"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	out, err := svc.OpenFile(context.Background(), "docs/readme.txt", model.Span{}, 200)
	if err != nil {
		t.Fatalf("expected no error on benign content, got %v", err)
	}
	if !strings.Contains(out, content) {
		t.Fatalf("returned content did not contain expected text, got %q", out)
	}
}

func TestOpenFile_PathExcludeOverrides(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("non-secret but excluded"), 0o644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetPathExcludes([]string{"**/private/**"})
	_, err := svc.OpenFile(context.Background(), "private/secret.txt", model.Span{}, 200)
	if !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected forbidden on path exclude, got %v", err)
	}
}

func TestOpenFile_ContentPatternOverride(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs", "data.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("custom-sensitive-value"), 0o644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	if err := svc.SetSecretPatterns([]string{"custom-sensitive-value"}); err != nil {
		t.Fatalf("SetSecretPatterns failed: %v", err)
	}
	_, err := svc.OpenFile(context.Background(), "docs/data.txt", model.Span{}, 200)
	if !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("expected forbidden on custom content pattern, got %v", err)
	}
}
