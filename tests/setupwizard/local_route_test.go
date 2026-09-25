package setupwizard_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
	"github.com/dirstral/dir2mcp/internal/setupwizard"
)

// The setup wizard's local route: a first run with Ollama needs no account and
// no key. Before it, the wizard's only path demanded a Mistral key, which
// contradicted the README's "fully local" quickstart on the first screen.

func TestSplitOllamaModels(t *testing.T) {
	embed, chat := setupwizard.SplitOllamaModels([]string{
		"qwen2.5:7b-instruct-q4_K_M", "nomic-embed-text:latest", "llama3.1:8b",
		"mxbai-embed-large", "bge-m3", "all-minilm:l6-v2", "snowflake-arctic-embed:m",
	})
	wantEmbed := []string{"nomic-embed-text:latest", "mxbai-embed-large", "bge-m3", "all-minilm:l6-v2", "snowflake-arctic-embed:m"}
	wantChat := []string{"qwen2.5:7b-instruct-q4_K_M", "llama3.1:8b"}
	if !reflect.DeepEqual(embed, wantEmbed) || !reflect.DeepEqual(chat, wantChat) {
		t.Errorf("embed=%v chat=%v, want %v and %v", embed, chat, wantEmbed, wantChat)
	}
}

func TestPreferredModel(t *testing.T) {
	if got := setupwizard.PreferredModel([]string{"mxbai-embed-large", "nomic-embed-text:latest"}, []string{"nomic-embed-text"}, "x"); got != "nomic-embed-text:latest" {
		t.Errorf("preferred prefix: got %q", got)
	}
	if got := setupwizard.PreferredModel([]string{"phi3"}, []string{"qwen2.5"}, "x"); got != "phi3" {
		t.Errorf("no preferred match: got %q, want the first model", got)
	}
	if got := setupwizard.PreferredModel(nil, []string{"qwen2.5"}, "qwen2.5:7b"); got != "qwen2.5:7b" {
		t.Errorf("no models: got %q, want the fallback", got)
	}
}

func TestProbeOllama(t *testing.T) {
	tags := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen2.5:7b"},{"name":"nomic-embed-text:latest"}]}`))
	}))
	defer tags.Close()
	got := setupwizard.ProbeOllama(context.Background(), tags.URL+"/")
	if !got.Reachable || got.BaseURL != tags.URL ||
		!reflect.DeepEqual(got.Embed, []string{"nomic-embed-text:latest"}) || !reflect.DeepEqual(got.Chat, []string{"qwen2.5:7b"}) {
		t.Errorf("probe = %+v", got)
	}

	notOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not ollama</html>"))
	}))
	defer notOllama.Close()
	if p := setupwizard.ProbeOllama(context.Background(), notOllama.URL); p.Reachable {
		t.Errorf("a server that is not Ollama was reported reachable: %+v", p)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + ln.Addr().String()
	_ = ln.Close()
	start := time.Now()
	if p := setupwizard.ProbeOllama(context.Background(), closed); p.Reachable {
		t.Errorf("a closed port was reported reachable: %+v", p)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("probing a closed port took %s; the wizard must not wait", d)
	}
}

func TestOllamaURL(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", setupwizard.DefaultOllamaURL},
		{"0.0.0.0:11434", "http://127.0.0.1:11434"},
		{"0.0.0.0", "http://127.0.0.1:11434"},
		{"gpu-box", "http://gpu-box:11434"},
		{"http://gpu-box:11434/", "http://gpu-box:11434"},
		{"https://ollama.example.com", "https://ollama.example.com"},
		{"http://0.0.0.0", "http://127.0.0.1"},
	} {
		t.Setenv("OLLAMA_HOST", tc.env)
		if got := setupwizard.OllamaURL(); got != tc.want {
			t.Errorf("OLLAMA_HOST=%q: got %q, want %q", tc.env, got, tc.want)
		}
	}
}

// TestWriteNewLocalConfig_LoadsAndBindsTheLocalModels pins that the file the
// local route writes is the README's quickstart YAML in effect: it loads, and
// embeddings and chat both resolve to the local Ollama models.
func TestWriteNewLocalConfig_LoadsAndBindsTheLocalModels(t *testing.T) {
	for _, env := range []string{"MISTRAL_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY"} {
		t.Setenv(env, "")
	}
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	local := setupwizard.LocalSetup{BaseURL: "http://127.0.0.1:11434/v1", EmbedModel: "nomic-embed-text", ChatModel: "qwen2.5:7b"}
	before := config.Default()
	after := before
	setupwizard.ApplyCorpusProfile(&after, setupwizard.ProfileCode)
	if err := setupwizard.WriteNewLocalConfig(path, local, setupwizard.ProfileSettingsYAML(before, after)); err != nil {
		t.Fatalf("WriteNewLocalConfig: %v", err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	embed, err := cfg.Providers().Resolve(provider.CapEmbed)
	if err != nil || embed.Name != "local" || embed.BaseURL != local.BaseURL || embed.EmbedTextModel != local.EmbedModel {
		t.Errorf("embed resolves to %+v (err %v), want the local profile", embed, err)
	}
	chat, err := cfg.Providers().Resolve(provider.CapChat)
	if err != nil || chat.Name != "local" || chat.ChatModel != local.ChatModel {
		t.Errorf("chat resolves to %+v (err %v), want the local profile", chat, err)
	}
	if cfg.RAGKDefault != after.RAGKDefault {
		t.Errorf("the profile line did not load: k_default=%d, want %d", cfg.RAGKDefault, after.RAGKDefault)
	}
}

func TestWriteNewLocalConfig_NeverReplacesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	const existing = "# mine\nroot_dir: /data\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	err := setupwizard.WriteNewLocalConfig(path, setupwizard.LocalSetup{BaseURL: "http://127.0.0.1:11434/v1", EmbedModel: "e", ChatModel: "c"}, nil)
	if !errors.Is(err, setupwizard.ErrConfigExists) {
		t.Fatalf("err = %v, want ErrConfigExists", err)
	}
	if got, _ := os.ReadFile(path); string(got) != existing {
		t.Errorf("the existing file changed:\n%s", got)
	}
}

func TestNewFormState_PicksTheRoute(t *testing.T) {
	found := setupwizard.OllamaProbe{BaseURL: setupwizard.DefaultOllamaURL, Reachable: true,
		Embed: []string{"mxbai-embed-large", "nomic-embed-text:latest"}, Chat: []string{"phi3", "qwen2.5:7b-instruct"}}
	st := setupwizard.NewFormState(setupwizard.Input{Ollama: found, ExistingKeys: map[string]bool{"MISTRAL_API_KEY": true}})
	if st.Route != string(setupwizard.RouteLocal) || st.EmbedModel != "nomic-embed-text:latest" || st.ChatModel != "qwen2.5:7b-instruct" {
		t.Errorf("Ollama found: route=%q embed=%q chat=%q, want local with the preferred models", st.Route, st.EmbedModel, st.ChatModel)
	}

	none := setupwizard.OllamaProbe{BaseURL: setupwizard.DefaultOllamaURL}
	st = setupwizard.NewFormState(setupwizard.Input{Ollama: none, ExistingKeys: map[string]bool{"OPENAI_API_KEY": true}})
	if st.Route != string(setupwizard.RouteCloud) || st.CloudProvider != "OPENAI_API_KEY" {
		t.Errorf("no Ollama, OpenAI key set: route=%q provider=%q, want cloud/OpenAI", st.Route, st.CloudProvider)
	}

	st = setupwizard.NewFormState(setupwizard.Input{Ollama: none})
	if st.Route != string(setupwizard.RouteLocal) || st.EmbedModel != setupwizard.DefaultEmbedModel || st.ChatModel != setupwizard.DefaultChatModel {
		t.Errorf("nothing found: route=%q embed=%q chat=%q, want local with the README defaults", st.Route, st.EmbedModel, st.ChatModel)
	}
	for _, spec := range setupwizard.ProviderKeys {
		if st.Keys[spec.EnvVar] == nil {
			t.Errorf("no field for %s", spec.EnvVar)
		}
	}
}

func TestFormState_Result(t *testing.T) {
	in := setupwizard.Input{Ollama: setupwizard.OllamaProbe{BaseURL: "http://127.0.0.1:11434"}}

	st := setupwizard.NewFormState(in)
	*st.Keys["MISTRAL_API_KEY"] = "typed-then-switched"
	res, err := st.Result(in)
	if err != nil || res.Route != setupwizard.RouteLocal || len(res.Keys) != 0 ||
		res.Local != (setupwizard.LocalSetup{BaseURL: "http://127.0.0.1:11434/v1", EmbedModel: setupwizard.DefaultEmbedModel, ChatModel: setupwizard.DefaultChatModel}) {
		t.Errorf("local route: %+v err=%v, want the local binding and no keys", res, err)
	}

	st = setupwizard.NewFormState(in)
	st.Route, st.CloudProvider = string(setupwizard.RouteCloud), "OPENAI_API_KEY"
	*st.Keys["OPENAI_API_KEY"] = " sk-openai "
	*st.Keys["MISTRAL_API_KEY"] = "not-chosen"
	*st.Keys["COHERE_API_KEY"] = "optional-but-not-asked"
	res, _ = st.Result(in)
	if !reflect.DeepEqual(res.Keys, map[string]string{"OPENAI_API_KEY": "sk-openai"}) {
		t.Errorf("cloud route keys = %v, want only the chosen provider's key", res.Keys)
	}
	st.ConfigureMore = true
	res, _ = st.Result(in)
	if res.Keys["COHERE_API_KEY"] != "optional-but-not-asked" || res.Keys["MISTRAL_API_KEY"] != "" {
		t.Errorf("with optional providers: keys = %v", res.Keys)
	}

	st.Save = false
	if _, err := st.Result(in); !errors.Is(err, huh.ErrUserAborted) {
		t.Errorf("declined save: err = %v, want huh.ErrUserAborted", err)
	}
}

// TestProviderKeys_NoCloudKeyIsRequired pins that no key description claims to
// be required: the local route needs none.
func TestProviderKeys_NoCloudKeyIsRequired(t *testing.T) {
	for _, spec := range setupwizard.ProviderKeys {
		if strings.Contains(strings.ToLower(spec.Description), "required") {
			t.Errorf("%s: description %q still says required", spec.EnvVar, spec.Description)
		}
	}
}

// TestWriteNewLocalConfig_RemovesAFileThatFailsValidation pins that a written
// config that does not load is removed again, so the next run can retry
// instead of stopping at ErrConfigExists.
func TestWriteNewLocalConfig_RemovesAFileThatFailsValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dir2mcp.yaml")
	broken := setupwizard.LocalSetup{BaseURL: "http://127.0.0.1:11434/v1", EmbedModel: "e\n  : [", ChatModel: "c"}
	err := setupwizard.WriteNewLocalConfig(path, broken, nil)
	if err == nil || errors.Is(err, setupwizard.ErrConfigExists) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if strings.Contains(err.Error(), "<nil>") {
		t.Errorf("error text prints a nil error: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("the file that failed validation is still there (stat err %v)", statErr)
	}
	good := setupwizard.LocalSetup{BaseURL: "http://127.0.0.1:11434/v1", EmbedModel: "nomic-embed-text", ChatModel: "qwen2.5:7b"}
	if err := setupwizard.WriteNewLocalConfig(path, good, nil); err != nil {
		t.Fatalf("retry after a failed validation: %v", err)
	}
}
