package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mapping"
)

// echoInput is the JSON schema for the fake echo tool.
type echoInput struct {
	Message string `json:"message" jsonschema:"message to echo back"`
}

// echoOutput is the structured output of the fake echo tool.
type echoOutput struct {
	Echo string `json:"echo"`
}

// echoTool is a fake Tool used to exercise the registry without touching
// the Workiva API.
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "Echoes a message back to the caller" }

func (echoTool) RegisterSDK(s *mcp.Server, deps Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "echo",
		Description: "Echoes a message back to the caller",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
		return &mcp.CallToolResult{}, echoOutput{Echo: in.Message}, nil
	})
}

// testDeps returns Deps backed by ephemeral stores, with no Workiva client.
func testDeps(t *testing.T) Deps {
	t.Helper()
	store, err := mapping.Open(":memory:")
	if err != nil {
		t.Fatalf("mapping.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	log, err := audit.Open(":memory:")
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	t.Cleanup(func() { log.Close() })

	return Deps{
		Store: store,
		Audit: log,
		Cfg:   &config.Config{Region: "eu"},
	}
}

func TestRegistryRegisterAndNames(t *testing.T) {
	reg := NewRegistry()
	reg.Register(echoTool{})

	names := reg.Names()
	if len(names) != 1 || names[0] != "echo" {
		t.Fatalf("Names() = %v, want [echo]", names)
	}
}

func TestRegistryBindDuplicateFails(t *testing.T) {
	reg := NewRegistry()
	reg.Register(echoTool{})
	reg.Register(echoTool{})

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "dev"}, nil)
	if err := reg.Bind(srv, Deps{}); err == nil {
		t.Fatal("Bind with duplicate tool names should fail")
	}
}

// TestRegistryListAndCall registers the fake echo tool, serves it over
// streamable HTTP, connects with the SDK client, and verifies the tool is
// listed and callable end to end.
func TestRegistryListAndCall(t *testing.T) {
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
	t.Cleanup(func() { session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
		t.Fatalf("ListTools returned %d tools, want exactly [echo]", len(tools.Tools))
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"message": "hello"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	content, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map", result.StructuredContent)
	}
	if content["echo"] != "hello" {
		t.Fatalf("echo result = %v, want hello", content["echo"])
	}
}

// bearerClient returns an HTTP client that attaches the bearer token to
// every request, as an MCP client configured with static API-key auth would.
func bearerClient(token string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.Header.Set("Authorization", "Bearer "+token)
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
