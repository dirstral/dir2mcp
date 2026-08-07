package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
)

// The recognition entity/event filter's TOOL surface (design 0004 §7). The
// filter semantics live in tests/retrieval; these pin that the arguments are
// accepted, forwarded verbatim, advertised to clients, and validated.

func entityFilterQuery(t *testing.T, toolName, arguments string) (model.SearchQuery, int, string) {
	t.Helper()
	cfg := config.Default()
	cfg.AuthMode = "none"

	var got model.SearchQuery
	retriever := &askAudioRetrieverStub{
		indexingComplete: true,
		OnSearch: func(q model.SearchQuery) ([]model.SearchHit, error) {
			got = q
			return []model.SearchHit{}, nil
		},
		OnAskQuery: func(q model.SearchQuery) { got = q },
	}
	server := httptest.NewServer(mcp.NewServer(cfg, retriever).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+toolName+`","arguments":`+arguments+`}}`)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return got, resp.StatusCode, string(body)
}

func TestSearchForwardsTheEntityAndEventFilters(t *testing.T) {
	q, status, body := entityFilterQuery(t, "dir2mcp_search",
		`{"query":"q","entities":["team:sf","player:x"],"events":["at_bat"]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if strings.Join(q.Entities, ",") != "team:sf,player:x" {
		t.Fatalf("entities not forwarded: %v", q.Entities)
	}
	if strings.Join(q.Events, ",") != "at_bat" {
		t.Fatalf("events not forwarded: %v", q.Events)
	}
}

// The ask path gets them too: "what did the Giants do in the 7th" is an ask,
// not a search, and it is the query the whole feature exists for.
func TestAskForwardsTheEntityAndEventFilters(t *testing.T) {
	q, status, body := entityFilterQuery(t, "dir2mcp_ask",
		`{"question":"what happened","entities":["team:sf"],"events":["pitch"]}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if strings.Join(q.Entities, ",") != "team:sf" {
		t.Fatalf("entities not forwarded to ask: %v", q.Entities)
	}
	if strings.Join(q.Events, ",") != "pitch" {
		t.Fatalf("events not forwarded to ask: %v", q.Events)
	}
}

func TestOmittingTheFiltersLeavesThemUnset(t *testing.T) {
	q, status, body := entityFilterQuery(t, "dir2mcp_search", `{"query":"q"}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if len(q.Entities) != 0 || len(q.Events) != 0 {
		t.Fatalf("absent filters materialised: %+v / %+v", q.Entities, q.Events)
	}
}

// TestAnEmptyFilterValueIsRejected: silently ignoring a blank id would turn a
// caller's bug into "no filtering at all", which is the worst possible failure
// for a filter — strictly more results than asked for, with no signal.
func TestAnEmptyFilterValueIsRejected(t *testing.T) {
	for _, args := range []string{
		`{"query":"q","entities":["  "]}`,
		`{"query":"q","events":[""]}`,
	} {
		_, _, body := entityFilterQuery(t, "dir2mcp_search", args)
		if !strings.Contains(body, "INVALID_FIELD") {
			t.Fatalf("%s was accepted: %s", args, body)
		}
	}
}

// TestTheFiltersAreAdvertisedInTheToolSchema: a client cannot use an argument it
// cannot discover, and the schema is how discovery happens.
func TestTheFiltersAreAdvertisedInTheToolSchema(t *testing.T) {
	cfg := config.Default()
	cfg.AuthMode = "none"
	server := httptest.NewServer(mcp.NewServer(cfg, &askAudioRetrieverStub{indexingComplete: true}).Handler())
	defer server.Close()

	sessionID := initializeSession(t, server.URL+cfg.MCPPath)
	resp := postRPC(t, server.URL+cfg.MCPPath, sessionID,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	defer func() { _ = resp.Body.Close() }()

	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	for _, want := range []string{"dir2mcp_search", "dir2mcp_ask"} {
		found := false
		for _, tool := range envelope.Result.Tools {
			if tool.Name != want {
				continue
			}
			found = true
			for _, field := range []string{"entities", "events"} {
				if _, ok := tool.InputSchema.Properties[field]; !ok {
					t.Fatalf("%s does not advertise %q", want, field)
				}
			}
		}
		if !found {
			t.Fatalf("tool %s missing from tools/list", want)
		}
	}
}
