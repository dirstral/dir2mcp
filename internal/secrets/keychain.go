// Package secrets provides an OS keychain backend for provider credentials
// (SPEC §16.1.1: env → keychain → file → session). It wraps the cross-platform
// keyring (macOS Keychain, Linux Secret Service, Windows Credential Manager) so
// API keys can live encrypted at rest instead of in a plaintext .env.local.
//
// Resolution stays env-var-keyed: a credential stored under key "MISTRAL_API_KEY"
// satisfies the same ${MISTRAL_API_KEY} placeholder the env/.env.local sources
// use, so keychain is a drop-in, higher-security source with no config churn.
package secrets

import (
	"errors"

	keyring "github.com/zalando/go-keyring"
)

// DefaultService is the keyring service (collection) name under which dir2mcp
// stores credentials. The account/key is the provider env var name.
const DefaultService = "dir2mcp"

// DisableEnvVar, when set to a non-empty value in the environment, turns off all
// keychain access. It is read early (before the keychain is consulted) so a user
// or a headless daemon can opt out without touching the keychain at all.
const DisableEnvVar = "DIR2MCP_DISABLE_KEYCHAIN"

// managedEnvVars is the set of built-in provider credential env vars the
// keychain backend reads on load. Custom provider profiles using other env vars
// are not auto-read (they can still be set via env/.env.local). It is
// unexported and exposed only via ManagedEnvVars (a copy) so importers cannot
// mutate which credentials are auto-resolved at runtime.
var managedEnvVars = []string{
	"MISTRAL_API_KEY",
	"OPENAI_API_KEY",
	"OPENROUTER_API_KEY",
	"ANTHROPIC_API_KEY",
	"GEMINI_API_KEY",
	"COHERE_API_KEY",
	"ELEVENLABS_API_KEY",
	// Qdrant vector-backend credential (issue #268), used when
	// index.backend=qdrant against a secured/Cloud deployment.
	"QDRANT_API_KEY",
	// S3 corpus-source credentials (issue #244). Stored under the standard AWS
	// env var names so a keychain entry satisfies the same reference the
	// environment/.env.local sources use.
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
}

// ErrNotFound indicates the requested secret is absent from the keychain.
var ErrNotFound = keyring.ErrNotFound

// ManagedEnvVars returns a fresh copy of the built-in provider credential env
// vars the keychain backend reads automatically. Returning a copy keeps the
// canonical list immutable across the process lifetime.
func ManagedEnvVars() []string {
	out := make([]string, len(managedEnvVars))
	copy(out, managedEnvVars)
	return out
}

// IsManaged reports whether key is one of the built-in provider credentials the
// keychain backend reads automatically.
func IsManaged(key string) bool {
	for _, k := range managedEnvVars {
		if k == key {
			return true
		}
	}
	return false
}

// Get returns the secret stored under (service, key). A missing entry returns
// ("", ErrNotFound); any other error is the backend's (locked/unsupported).
func Get(service, key string) (string, error) {
	return keyring.Get(service, key)
}

// Set stores value under (service, key), creating or replacing the entry.
func Set(service, key, value string) error {
	return keyring.Set(service, key, value)
}

// Delete removes the entry under (service, key). A missing entry returns
// ErrNotFound.
func Delete(service, key string) error {
	return keyring.Delete(service, key)
}

// Has reports whether a non-empty secret exists under (service, key). Backend
// errors other than "not found" are treated as absent (fail-open: callers fall
// back to env/.env.local rather than hard-failing).
func Has(service, key string) bool {
	v, err := Get(service, key)
	if err != nil {
		return false
	}
	return v != ""
}

// IsNotFound reports whether err means the secret was absent (vs. a backend
// failure), so callers can distinguish "no entry" from "keychain unavailable".
func IsNotFound(err error) bool {
	return errors.Is(err, keyring.ErrNotFound)
}
