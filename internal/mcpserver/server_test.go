package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
)

func TestNewRequiresAPIToken(t *testing.T) {
	if _, err := New(Deps{}, NewRegistry(), nil); err == nil {
		t.Fatal("New without APIToken should fail")
	}
	if _, err := New(Deps{}, NewRegistry(), &Options{}); err == nil {
		t.Fatal("New with empty APIToken should fail")
	}
}

func TestRequestWithoutBearerTokenRejected(t *testing.T) {
	deps := testDeps(t)
	handler, err := New(deps, NewRegistry(), &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong token", "Bearer wrong"},
		{"not bearer scheme", "Basic dGVzdA=="},
		{"bare token without scheme", "test-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader([]byte("{}")))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			t.Cleanup(func() {
				if err := resp.Body.Close(); err != nil {
					t.Errorf("close unauthorized response: %v", err)
				}
			})
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("read unauthorized response: %v", err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestServerAuditsToolCalls connects with the SDK client, calls the echo
// tool, and asserts a matching audit row was appended with the default
// actor and the tool name as target.
func TestServerAuditsToolCalls(t *testing.T) {
	deps := testDeps(t)

	reg := NewRegistry()
	reg.Register(echoTool{})

	handler, err := New(deps, reg, &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: bearerClient("test-token"),
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "audited"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	entries := exportEntries(t, deps.Audit)
	if len(entries) == 0 {
		t.Fatal("no audit entries appended")
	}
	last := entries[len(entries)-1]
	if last.Tool != "echo" {
		t.Fatalf("audited tool = %q, want echo", last.Tool)
	}
	if last.Action != "call" {
		t.Fatalf("audited action = %q, want call", last.Action)
	}
	if last.Actor != DefaultActor {
		t.Fatalf("audited actor = %q, want %q", last.Actor, DefaultActor)
	}
	if err := deps.Audit.Verify(ctx); err != nil {
		t.Fatalf("audit chain invalid after tool call: %v", err)
	}
}

// TestServerAuditUsesActorHeader verifies the actor header overrides the
// default actor.
func TestServerAuditUsesActorHeader(t *testing.T) {
	deps := testDeps(t)

	reg := NewRegistry()
	reg.Register(echoTool{})

	handler, err := New(deps, reg, &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	httpClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set(DefaultActorHeader, "user@example.com")
			return http.DefaultTransport.RoundTrip(req)
		}),
	}

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "dev"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "x"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	entries := exportEntries(t, deps.Audit)
	last := entries[len(entries)-1]
	if last.Actor != "user@example.com" {
		t.Fatalf("audited actor = %q, want user@example.com", last.Actor)
	}
}

func exportEntries(t *testing.T, log *audit.Log) []audit.Entry {
	t.Helper()
	var buf bytes.Buffer
	if err := log.Export(&buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var entries []audit.Entry
	for line := range bytes.Lines(buf.Bytes()) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e audit.Entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("unmarshal audit entry %q: %v", line, err)
		}
		entries = append(entries, e)
	}
	return entries
}
