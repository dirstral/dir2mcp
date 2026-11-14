package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dirstral/dir2mcp/internal/statefs"
)

// SecretPolicyProvenanceFile is the state file that records the secret
// pattern lists of the run before the first spec 0.93.0 run (SPEC §7.2).
//
// The store records the effective secret policy since spec 0.93.0. A corpus
// that an older release indexed has no such record, and the configured lists
// of THIS run do not show what the older run used. The effective-config
// snapshot (EffectiveConfigSnapshotFile) does: every release since March 2026
// writes `secret_patterns` into it on each config load. But each run
// overwrites that snapshot before its scan starts. So SaveEffectiveSnapshot
// records the lists of the snapshot it is about to replace into this file,
// once, while the file does not exist yet (with no lists when that snapshot
// holds none). Ingest and retrieval read it only while the store records no
// policy.
const SecretPolicyProvenanceFile = "secret-policy-previous.json"

// SecretPolicyProvenance is the content of SecretPolicyProvenanceFile.
type SecretPolicyProvenance struct {
	Secret []string `json:"secret"`
	Allow  []string `json:"allow"`
	// Source names the file the lists came from.
	Source string `json:"source"`
}

// PreviousSecretPolicy returns the secret pattern lists that the run before
// the first spec 0.93.0 run used, from SecretPolicyProvenanceFile. ok is false
// when no provenance exists: then the previous policy is unknown.
func PreviousSecretPolicy(stateDir string) (SecretPolicyProvenance, bool, error) {
	path := filepath.Join(stateDirOrDefault(stateDir), SecretPolicyProvenanceFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return SecretPolicyProvenance{}, false, nil
	}
	if err != nil {
		return SecretPolicyProvenance{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	var out SecretPolicyProvenance
	if err := json.Unmarshal(raw, &out); err != nil {
		return SecretPolicyProvenance{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	if out.Secret == nil {
		return SecretPolicyProvenance{}, false, nil
	}
	if out.Allow == nil {
		out.Allow = []string{}
	}
	return out, true, nil
}

// captureSecretPolicyProvenance records, once, what the snapshot that is
// about to be replaced says about the previous secret policy. It does nothing
// when SecretPolicyProvenanceFile exists. Otherwise it always writes the file,
// so that a later save never takes a newer snapshot (the lists of a run under
// this release) as the provenance of a corpus that an older release indexed:
//
//   - a snapshot with a `secret_patterns` key gives its lists. A snapshot
//     without `secret_allow_patterns` comes from a release before spec 0.93.0,
//     which had no allow list, so the allow list is empty;
//   - no snapshot, a snapshot that does not parse, or one without
//     `secret_patterns` (a release too old to record it) gives no lists: the
//     file records that the previous policy is unknown.
func captureSecretPolicyProvenance(stateDir string) error {
	dir := stateDirOrDefault(stateDir)
	target := filepath.Join(dir, SecretPolicyProvenanceFile)
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out := SecretPolicyProvenance{Source: EffectiveConfigSnapshotFile}
	raw, err := os.ReadFile(filepath.Join(dir, EffectiveConfigSnapshotFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		out.Source = ""
	case err != nil:
		return err
	default:
		var lists struct {
			Secret *[]string `yaml:"secret_patterns"`
			Allow  *[]string `yaml:"secret_allow_patterns"`
		}
		if yaml.Unmarshal(raw, &lists) == nil && lists.Secret != nil {
			out.Secret = trimmedList(*lists.Secret)
			out.Allow = []string{}
			if lists.Allow != nil {
				out.Allow = trimmedList(*lists.Allow)
			}
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return statefs.WriteFile(target, append(encoded, '\n'))
}

func trimmedList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func stateDirOrDefault(stateDir string) string {
	if trimmed := strings.TrimSpace(stateDir); trimmed != "" {
		return trimmed
	}
	return Default().StateDir
}
