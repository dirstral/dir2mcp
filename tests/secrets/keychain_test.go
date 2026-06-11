package secrets_test

import (
	"testing"

	keyring "github.com/zalando/go-keyring"

	"github.com/dirstral/dir2mcp/internal/secrets"
)

func TestSetGetDelete(t *testing.T) {
	keyring.MockInit()

	if err := secrets.Set(secrets.DefaultService, "MISTRAL_API_KEY", "sk-test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := secrets.Get(secrets.DefaultService, "MISTRAL_API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-test" {
		t.Fatalf("Get=%q want sk-test", got)
	}
	if !secrets.Has(secrets.DefaultService, "MISTRAL_API_KEY") {
		t.Error("Has should report the stored key present")
	}

	if err := secrets.Delete(secrets.DefaultService, "MISTRAL_API_KEY"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if secrets.Has(secrets.DefaultService, "MISTRAL_API_KEY") {
		t.Error("Has should report the key absent after Delete")
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	keyring.MockInit()
	_, err := secrets.Get(secrets.DefaultService, "NOPE_API_KEY")
	if !secrets.IsNotFound(err) {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestDeleteMissingReturnsNotFound(t *testing.T) {
	keyring.MockInit()
	err := secrets.Delete(secrets.DefaultService, "NOPE_API_KEY")
	if !secrets.IsNotFound(err) {
		t.Fatalf("expected not-found error on delete, got %v", err)
	}
}

func TestHasFailOpenOnMissing(t *testing.T) {
	keyring.MockInit()
	if secrets.Has(secrets.DefaultService, "ABSENT_API_KEY") {
		t.Error("Has must be false for an absent key (fail-open, not a panic/true)")
	}
}

func TestIsManaged(t *testing.T) {
	if !secrets.IsManaged("MISTRAL_API_KEY") {
		t.Error("MISTRAL_API_KEY should be managed")
	}
	if !secrets.IsManaged("COHERE_API_KEY") {
		t.Error("COHERE_API_KEY should be managed")
	}
	if secrets.IsManaged("RANDOM_TOKEN") {
		t.Error("RANDOM_TOKEN should not be managed")
	}
}

func TestManagedEnvVarsIncludeCoreProviders(t *testing.T) {
	want := []string{"MISTRAL_API_KEY", "COHERE_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"}
	for _, w := range want {
		if !secrets.IsManaged(w) {
			t.Errorf("expected %s in ManagedEnvVars", w)
		}
	}
}
