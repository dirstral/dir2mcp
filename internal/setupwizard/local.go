package setupwizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/provider"
)

// Route is how the wizard sets up the models: on this machine through Ollama,
// or through one cloud provider's API key.
type Route string

const (
	RouteLocal Route = "local"
	RouteCloud Route = "cloud"
)

const (
	// DefaultOllamaURL is where a stock Ollama install listens.
	DefaultOllamaURL = "http://127.0.0.1:11434"
	// DefaultEmbedModel and DefaultChatModel are the models the README's local
	// quickstart pulls. The wizard proposes them when Ollama has none.
	DefaultEmbedModel = "nomic-embed-text"
	DefaultChatModel  = "qwen2.5:7b"
	// localProviderName is the provider profile the local route writes.
	localProviderName = "local"
	// ollamaProbeTimeout bounds the one request the wizard makes before it
	// shows the form, so a machine with no Ollama does not wait.
	ollamaProbeTimeout = 1500 * time.Millisecond
)

// OllamaURL is the address the wizard probes: OLLAMA_HOST when it is set (the
// variable Ollama itself reads), else DefaultOllamaURL. A bare host:port gets
// http://, and a 0.0.0.0 listen address is reached through 127.0.0.1.
func OllamaURL() string {
	host := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if host == "" {
		return DefaultOllamaURL
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	return strings.TrimRight(strings.Replace(host, "://0.0.0.0", "://127.0.0.1", 1), "/")
}

// LocalSetup is the local route's answer: the Ollama OpenAI-compatible
// endpoint and the two models to bind.
type LocalSetup struct {
	BaseURL    string
	EmbedModel string
	ChatModel  string
}

// OllamaProbe is what the wizard found at the Ollama address before it showed
// the form. Reachable is false when nothing answered; the lists are empty then.
type OllamaProbe struct {
	BaseURL   string
	Reachable bool
	Embed     []string
	Chat      []string
}

// ProbeOllama asks the Ollama server at baseURL which models it has
// (GET /api/tags) and sorts them into embedding and chat models. It never
// fails: a server that does not answer, or answers with something else, gives
// a probe with Reachable false.
func ProbeOllama(ctx context.Context, baseURL string) OllamaProbe {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	probe := OllamaProbe{BaseURL: baseURL}
	ctx, cancel := context.WithTimeout(ctx, ollamaProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return probe
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return probe
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return probe
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&tags); err != nil {
		return probe
	}
	names := make([]string, 0, len(tags.Models))
	for _, m := range tags.Models {
		if n := strings.TrimSpace(m.Name); n != "" {
			names = append(names, n)
		}
	}
	probe.Reachable = true
	probe.Embed, probe.Chat = SplitOllamaModels(names)
	return probe
}

// embedModelMarkers identify embedding models by name. Ollama's tag list does
// not say what a model is for, and its embedding models all carry one of
// these in the name.
var embedModelMarkers = []string{"embed", "bge", "minilm", "e5-", "gte-", "arctic-embed"}

// SplitOllamaModels sorts Ollama model names into embedding models and chat
// models, keeping the server's order within each list.
func SplitOllamaModels(names []string) (embed, chat []string) {
	for _, name := range names {
		lower := strings.ToLower(name)
		isEmbed := false
		for _, marker := range embedModelMarkers {
			if strings.Contains(lower, marker) {
				isEmbed = true
				break
			}
		}
		if isEmbed {
			embed = append(embed, name)
		} else {
			chat = append(chat, name)
		}
	}
	return embed, chat
}

// PreferredModel returns the first model whose name starts with one of the
// preferred prefixes, in the order of the prefixes, else the first model, else
// fallback.
func PreferredModel(models, preferred []string, fallback string) string {
	for _, prefix := range preferred {
		for _, m := range models {
			if strings.HasPrefix(m, prefix) {
				return m
			}
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return fallback
}

// LocalProviderYAML returns the providers and model blocks that bind
// embeddings and chat to the local Ollama models: the same YAML the README's
// local quickstart shows.
func LocalProviderYAML(l LocalSetup) []string {
	return []string{
		"providers:",
		"  " + localProviderName + ":",
		"    kind: openai",
		"    base_url: " + l.BaseURL,
		"    embed_text_model: " + l.EmbedModel,
		"    embed_code_model: " + l.EmbedModel,
		"    chat_model: " + l.ChatModel,
		"model:",
		"  embed: {provider: " + localProviderName + "}",
		"  chat: {provider: " + localProviderName + "}",
	}
}

// ErrConfigExists is returned by WriteNewLocalConfig when the file is there
// already. An existing config file is never rewritten.
var ErrConfigExists = errors.New("config file already exists")

// WriteNewLocalConfig creates the config file at path with the local provider
// binding and any extra lines (a corpus profile), then loads it back and checks
// that embeddings resolve to the local provider. It never replaces a file: an
// existing path gives ErrConfigExists and the file is left as it is.
func WriteNewLocalConfig(path string, l LocalSetup, extra []string) error {
	lines := append(LocalProviderYAML(l), extra...)
	content := []byte(strings.Join(lines, "\n") + "\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return ErrConfigExists
	}
	if err != nil {
		return fmt.Errorf("create config file: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write config file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write config file: %w", err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		return fmt.Errorf("the written config does not load: %w", err)
	}
	prof, err := cfg.Providers().Resolve(provider.CapEmbed)
	if err != nil || prof.Name != localProviderName {
		return fmt.Errorf("the written config does not bind embeddings to %q: %v", localProviderName, err)
	}
	return nil
}
