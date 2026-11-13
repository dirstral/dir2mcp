package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/secretmatch"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// SPEC §7.2 (spec 0.93.0): open_file reads its source in chunks, and the
// decision over those chunks must equal the whole-text decision. An allow match
// that ends at a read boundary must not cancel a generic token match that the
// whole text does not cancel.
func TestOpenFile_AllowMatchAtAReadBoundary_DoesNotRelease(t *testing.T) {
	tok := "to" + "ken"
	cases := []struct {
		name   string
		body   string
		refuse bool
	}{
		{"run goes on past the allow match", tok + "=" + strings.Repeat("A", 100000) + "!", true},
		{"whole source is one allow match", tok + "=" + strings.Repeat("A", 100000), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testutil.TempDir(t)
			relPath := "docs/run.txt"
			abs := filepath.Join(root, filepath.FromSlash(relPath))
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(abs, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			svc := newBoundedService(root)
			if err := svc.SetSecretPolicy([]string{secretmatch.GenericTokenPattern}, []string{`^` + tok + `=A+$`}); err != nil {
				t.Fatalf("SetSecretPolicy: %v", err)
			}
			_, err := svc.OpenFile(context.Background(), relPath, model.Span{}, 20000)
			if tc.refuse && !errors.Is(err, model.ErrForbidden) {
				t.Fatalf("OpenFile err = %v, want ErrForbidden (the whole-text matcher refuses this source)", err)
			}
			if !tc.refuse && err != nil {
				t.Fatalf("OpenFile err = %v, want the head of the source (the whole-text matcher serves it)", err)
			}
		})
	}
}
