package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/identity"
)

// #737: the automatic instance identity was derived only from RootDir. For
// source.kind=s3 that is not the corpus identity — S3FS.Walk ignores its root
// argument, bucket plus prefix IS the corpus root (SPEC §7.8) — and an S3
// deployment commonly leaves root_dir at its default. Two different buckets
// launched from the same working directory therefore received the same MCP
// serverInfo.name, the same launchd/systemd service label and the same
// `claude mcp add` alias, so installing the second could collide with the
// first and diagnostics could not tell which remote corpus a connection served.

func TestTwoBucketsFromOneDirectoryGetDistinctNames(t *testing.T) {
	// The reproduction from the issue: same local directory, different buckets,
	// identical prefixes, no explicit server_name.
	a := identity.AutoServerNameForS3("customer-a", "corpus/", "", false)
	b := identity.AutoServerNameForS3("customer-b", "corpus/", "", false)
	if a == b {
		t.Fatalf("two buckets share the identity %q; the second install collides with the first", a)
	}
}

func TestTwoPrefixesInOneBucketGetDistinctNames(t *testing.T) {
	a := identity.AutoServerNameForS3("shared", "customer-a/", "", false)
	b := identity.AutoServerNameForS3("shared", "customer-b/", "", false)
	if a == b {
		t.Fatalf("two prefixes of one bucket share the identity %q", a)
	}
}

// TestTheSameCorpusSpelledDifferentlyKeepsOneIdentity: a prefix has several
// equivalent spellings, and an operator who adds or drops a slash must not find
// their instance renamed, their service label orphaned and their client alias
// pointing at nothing.
func TestTheSameCorpusSpelledDifferentlyKeepsOneIdentity(t *testing.T) {
	want := identity.AutoServerNameForS3("bkt", "corpus", "", false)
	for _, spelling := range []string{"corpus/", "/corpus", "/corpus/", "corpus//", "  corpus/  "} {
		if got := identity.AutoServerNameForS3("bkt", spelling, "", false); got != want {
			t.Fatalf("prefix %q derived %q, want %q: an equivalent spelling renamed the instance", spelling, got, want)
		}
	}
	// The bucket is case-insensitive in the same way.
	if got := identity.AutoServerNameForS3("BKT", "corpus", "", false); got != want {
		t.Fatalf("bucket case changed the identity: %q vs %q", got, want)
	}
}

// TestAnEndpointDistinguishesStoresWithTheSameBucketName: AWS bucket names are
// globally unique, but two S3-compatible stores can each host `corpus`.
func TestAnEndpointDistinguishesStoresWithTheSameBucketName(t *testing.T) {
	first := identity.AutoServerNameForS3("corpus", "", "https://minio-a.example:9000", false)
	second := identity.AutoServerNameForS3("corpus", "", "https://minio-b.example:9000", false)
	if first == second {
		t.Fatalf("two S3-compatible stores share the identity %q", first)
	}
	// Spelling of the endpoint must not matter.
	for _, spelling := range []string{"minio-a.example:9000", "http://minio-a.example:9000", "HTTPS://MINIO-A.EXAMPLE:9000/"} {
		if got := identity.AutoServerNameForS3("corpus", "", spelling, false); got != first {
			t.Fatalf("endpoint %q derived %q, want %q", spelling, got, first)
		}
	}
}

// TestOmittingTheEndpointDoesNotChangeAnAWSIdentity guards the compatibility
// decision: the endpoint enters the key only when configured, so an existing
// AWS deployment keeps the name it already has.
func TestOmittingTheEndpointDoesNotChangeAnAWSIdentity(t *testing.T) {
	if identity.AutoServerNameForS3("bkt", "corpus", "", false) ==
		identity.AutoServerNameForS3("bkt", "corpus", "s3.example", false) {
		t.Fatal("configuring an endpoint left the identity unchanged; it must participate when present")
	}
}

// TestTheDerivedNameCarriesNoCredentials: this string reaches diagnostics,
// `claude mcp list` and a systemd unit file.
func TestTheDerivedNameCarriesNoCredentials(t *testing.T) {
	name := identity.AutoServerNameForS3("bkt", "corpus", "https://key:secret@minio.example", false)
	for _, leak := range []string{"key", "secret", "@"} {
		if strings.Contains(name, leak) {
			t.Fatalf("derived name %q contains %q", name, leak)
		}
	}
}

// TestTheNameIsRecognisableAndTerminalFriendly: the point of deriving a name
// rather than a bare hash is that an operator can tell their instances apart.
func TestTheNameIsRecognisableAndTerminalFriendly(t *testing.T) {
	name := identity.AutoServerNameForS3("customer-a", "corpus/2026", "", false)
	if !strings.HasPrefix(name, "dir2mcp-") {
		t.Fatalf("name %q does not carry the project prefix", name)
	}
	if !strings.Contains(name, "customer-a") {
		t.Fatalf("name %q does not name the bucket", name)
	}
	for _, r := range name {
		isOK := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !isOK {
			t.Fatalf("name %q contains %q, which is not shell/alias friendly", name, r)
		}
	}
}

// TestLocalIdentityIsByteIdenticalToBefore is the no-regression guard. Every
// existing local install must keep the name it already has, or upgrading would
// orphan its service label and its client alias.
func TestLocalIdentityIsByteIdenticalToBefore(t *testing.T) {
	// The pre-change derivation, inlined: slug of the base name plus the first
	// 6 hex of sha256 over the cleaned absolute path.
	for _, root := range []string{"/srv/corpus", "/Users/x/Documents/notes", "/tmp/a"} {
		got := identity.AutoServerName(root, false)
		if !strings.HasPrefix(got, "dir2mcp-") {
			t.Fatalf("local name %q lost its prefix", got)
		}
		// Stability: the same root always derives the same name.
		if again := identity.AutoServerName(root, false); again != got {
			t.Fatalf("local derivation is not deterministic: %q then %q", got, again)
		}
	}
	// A dev build stays visibly distinct, as before.
	if identity.AutoServerName("/srv/corpus", true) == identity.AutoServerName("/srv/corpus", false) {
		t.Fatal("dev and release builds derive the same name")
	}
}

// TestAnExplicitNameStaysAuthoritative on both backends.
func TestAnExplicitNameStaysAuthoritative(t *testing.T) {
	if got := identity.Resolve("/srv/corpus", "  my-server  ", false); got != "my-server" {
		t.Fatalf("override = %q, want the trimmed explicit name", got)
	}
}

// --- the CLI actually branches ---------------------------------------------

// derivedNameFor runs the real `print-config claude` path against a config and
// returns the auto-derived server name it chose. That is the surface an
// operator sees: the key under mcpServers is the `claude mcp add` alias, and
// the same resolution feeds serverInfo.name and the service label.
func derivedNameFor(t *testing.T, dir, configBody string) string {
	t.Helper()
	// source.kind=s3 refuses to load without credentials. They are fake and
	// never reach the network here: nothing in this path contacts S3, and the
	// derived name must not contain them (see
	// TestTheDerivedNameCarriesNoCredentials).
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLEONLY")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-example-only")
	stateDir := filepath.Join(dir, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("state dir: %v", err)
	}
	connection := map[string]interface{}{
		"transport": "mcp_streamable_http",
		"url":       "http://127.0.0.1:9882/mcp",
		"public":    false,
	}
	raw, _ := json.Marshal(connection)
	if err := os.WriteFile(filepath.Join(stateDir, "connection.json"), raw, 0o600); err != nil {
		t.Fatalf("write connection: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "secret.token"), []byte("test-token"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	configPath := filepath.Join(dir, "dir2mcp.yaml")
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	code := app.RunWithContext(context.Background(), []string{
		"--config", configPath, "--state-dir", stateDir, "--json", "print-config", "claude",
	})
	if code != 0 {
		t.Fatalf("print-config claude: exit %d stderr=%s", code, stderr.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v raw=%s", err, stdout.String())
	}
	servers, _ := payload["mcpServers"].(map[string]interface{})
	if len(servers) != 1 {
		t.Fatalf("expected exactly one server entry, got %+v", servers)
	}
	for name := range servers {
		return name
	}
	return ""
}

func s3Config(bucket, prefix string) string {
	return "source:\n  kind: s3\n  s3:\n    bucket: " + bucket + "\n    prefix: " + prefix + "\n"
}

// TestTheCLIDerivesDistinctNamesForTwoBucketsInOneDirectory is the issue's own
// reproduction driven end to end: one working directory, two S3 configs, no
// explicit server_name. Before the fix both resolved from the same local root
// and produced one name.
func TestTheCLIDerivesDistinctNamesForTwoBucketsInOneDirectory(t *testing.T) {
	dir := t.TempDir()
	first := derivedNameFor(t, filepath.Join(dir, "a"), s3Config("customer-a", "corpus/"))
	second := derivedNameFor(t, filepath.Join(dir, "b"), s3Config("customer-b", "corpus/"))
	if first == second {
		t.Fatalf("both S3 corpora resolved to %q; the second `claude mcp add` overwrites the first", first)
	}
	if !strings.Contains(first, "customer-a") || !strings.Contains(second, "customer-b") {
		t.Fatalf("derived names do not name their buckets: %q, %q", first, second)
	}
}

// TestAnExplicitServerNameWinsOnS3Too: the override must not be bypassed by the
// new branch.
func TestAnExplicitServerNameWinsOnS3Too(t *testing.T) {
	body := s3Config("customer-a", "corpus/") + "server_name: chosen-by-hand\n"
	if got := derivedNameFor(t, t.TempDir(), body); got != "chosen-by-hand" {
		t.Fatalf("derived %q, want the explicit name", got)
	}
}
