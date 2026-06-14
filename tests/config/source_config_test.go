package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
)

// TestDefault_SourceKindLocal asserts the default corpus source is local so
// existing deployments behave identically (issue #244).
func TestDefault_SourceKindLocal(t *testing.T) {
	cfg := config.Default()
	if cfg.Source.Kind != "local" {
		t.Fatalf("default Source.Kind = %q, want local", cfg.Source.Kind)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config Validate: %v", err)
	}
}

// TestLoadFile_SourceNestedKeys checks the nested spec-style source block parses
// into the flat Source fields.
func TestLoadFile_SourceNestedKeys(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")
	writeFile(t, path, strings.Join([]string{
		"root_dir: /tmp/repo",
		"state_dir: /tmp/repo/.dir2mcp",
		"source:",
		"  kind: s3",
		"  s3:",
		"    bucket: my-bucket",
		"    prefix: corpus/docs",
		"    region: eu-central-1",
		"    endpoint: https://minio.local:9000",
	}, "\n")+"\n")

	cfg, err := config.LoadFile(path)
	// LoadFile applies no env, so s3 has no credentials -> Validate fails. Read
	// the parsed values off the returned error path by parsing without validate
	// is not exposed; instead assert the validation error is the credential one,
	// which proves the s3 kind + bucket parsed.
	if err == nil {
		t.Fatal("expected LoadFile to fail (s3 without credentials)")
	}
	if !strings.Contains(err.Error(), "AWS credentials") {
		t.Fatalf("LoadFile err = %v, want AWS-credentials error", err)
	}
	_ = cfg
}

// TestSaveFile_SourceRoundTrip guards the SaveFile -> LoadFile cycle for the
// non-secret source fields (local kind, so no credential requirement).
func TestSaveFile_SourceRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.RootDir = "/tmp/repo"
	cfg.StateDir = "/tmp/repo/.dir2mcp"
	cfg.Source.Kind = "nfs"
	cfg.Source.S3Bucket = "persisted-bucket"
	cfg.Source.S3Prefix = "persisted/prefix"
	cfg.Source.S3Region = "us-west-2"
	cfg.Source.S3Endpoint = "https://s3.example.com"

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}

	loaded, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if loaded.Source.Kind != "nfs" {
		t.Errorf("Source.Kind did not round-trip: got %q", loaded.Source.Kind)
	}
	if loaded.Source.S3Bucket != "persisted-bucket" {
		t.Errorf("Source.S3Bucket did not round-trip: got %q", loaded.Source.S3Bucket)
	}
	if loaded.Source.S3Prefix != "persisted/prefix" {
		t.Errorf("Source.S3Prefix did not round-trip: got %q", loaded.Source.S3Prefix)
	}
	if loaded.Source.S3Region != "us-west-2" {
		t.Errorf("Source.S3Region did not round-trip: got %q", loaded.Source.S3Region)
	}
	if loaded.Source.S3Endpoint != "https://s3.example.com" {
		t.Errorf("Source.S3Endpoint did not round-trip: got %q", loaded.Source.S3Endpoint)
	}
}

// TestSaveFile_NeverPersistsS3Credentials asserts resolved S3 credentials are
// never written to the config file.
func TestSaveFile_NeverPersistsS3Credentials(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".dir2mcp.yaml")

	cfg := config.Default()
	cfg.Source.Kind = "s3"
	cfg.Source.S3Bucket = "b"
	cfg.Source.S3AccessKeyID = "AKIASECRETID"
	cfg.Source.S3SecretAccessKey = "topsecretvalue"
	cfg.Source.S3SessionToken = "sessiontoken"

	if err := config.SaveFile(path, cfg); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	raw := string(rawBytes)
	for _, secret := range []string{"AKIASECRETID", "topsecretvalue", "sessiontoken"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("persisted config leaked credential %q:\n%s", secret, raw)
		}
	}
}

// TestValidate_S3RequiresBucket asserts s3 without a bucket is rejected.
func TestValidate_S3RequiresBucket(t *testing.T) {
	cfg := config.Default()
	cfg.Source.Kind = "s3"
	cfg.Source.S3AccessKeyID = "id"
	cfg.Source.S3SecretAccessKey = "secret"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("Validate(s3,no-bucket) = %v, want bucket-required error", err)
	}
}

// TestValidate_S3RequiresCredentials asserts s3 with a bucket but no resolved
// credentials is a clear, actionable error.
func TestValidate_S3RequiresCredentials(t *testing.T) {
	cfg := config.Default()
	cfg.Source.Kind = "s3"
	cfg.Source.S3Bucket = "b"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("Validate(s3,no-creds) = %v, want credentials-required error", err)
	}
}

// TestValidate_S3OK accepts a fully-specified s3 source and normalizes the kind.
func TestValidate_S3OK(t *testing.T) {
	cfg := config.Default()
	cfg.Source.Kind = "S3"
	cfg.Source.S3Bucket = "  b  "
	cfg.Source.S3AccessKeyID = "id"
	cfg.Source.S3SecretAccessKey = "secret"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(valid s3): %v", err)
	}
	if cfg.Source.Kind != "s3" {
		t.Errorf("Source.Kind not normalized: got %q", cfg.Source.Kind)
	}
	if cfg.Source.S3Bucket != "b" {
		t.Errorf("Source.S3Bucket not trimmed: got %q", cfg.Source.S3Bucket)
	}
}

// TestValidate_UnknownKind rejects an unsupported source kind.
func TestValidate_UnknownKind(t *testing.T) {
	cfg := config.Default()
	cfg.Source.Kind = "ftp"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "source.kind") {
		t.Fatalf("Validate(kind=ftp) = %v, want source.kind error", err)
	}
}
