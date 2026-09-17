package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHealthEndpointsDoNotRequireBearerToken(t *testing.T) {
	deps := testDeps(t)
	handler, err := New(deps, NewRegistry(), &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		path string
		body string
	}{
		{path: "/healthz", body: "ok\n"},
		{path: "/readyz", body: "ready\n"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.Code)
			}
			if res.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q", res.Body.String(), tc.body)
			}
			if got := res.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestReadinessFailsWithoutStores(t *testing.T) {
	handler, err := New(Deps{}, NewRegistry(), &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Code)
	}
	if res.Body.String() != "not ready\n" {
		t.Fatalf("body = %q, want generic readiness failure", res.Body.String())
	}
}

func TestReadinessFailsWithClosedStores(t *testing.T) {
	deps := testDeps(t)
	if err := deps.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := deps.Audit.Close(); err != nil {
		t.Fatal(err)
	}
	handler, err := New(deps, NewRegistry(), &Options{APIToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Code)
	}
}

func TestLocalhostProtectionCanOnlyBeDisabledExplicitly(t *testing.T) {
	for _, tc := range []struct {
		name          string
		disabled      bool
		wantForbidden bool
	}{
		{name: "protected", wantForbidden: true},
		{name: "explicitly disabled", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := New(testDeps(t), NewRegistry(), &Options{APIToken: "test-token", DisableLocalhostProtection: tc.disabled})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "http://public.example/mcp", strings.NewReader(`{}`))
			req.Host = "public.example"
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}))
			req.Header.Set("Authorization", "Bearer test-token")
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if tc.wantForbidden && res.Code != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", res.Code)
			}
			if !tc.wantForbidden && res.Code == http.StatusForbidden {
				t.Fatalf("status=%d, protection still enabled", res.Code)
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

func TestReadinessChecksEachLocalDependency(t *testing.T) {
	for _, state := range []string{"nil mapping", "nil audit", "closed mapping", "closed audit"} {
		t.Run(state, func(t *testing.T) {
			deps := testDeps(t)
			switch state {
			case "nil mapping":
				deps.Store = nil
			case "nil audit":
				deps.Audit = nil
			case "closed mapping":
				if err := deps.Store.Close(); err != nil {
					t.Fatal(err)
				}
			case "closed audit":
				if err := deps.Audit.Close(); err != nil {
					t.Fatal(err)
				}
			}
			handler, err := New(deps, NewRegistry(), &Options{APIToken: "test-token"})
			if err != nil {
				t.Fatal(err)
			}
			for path, want := range map[string]int{"/readyz": 503, "/healthz": 200} {
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
				if res.Code != want {
					t.Fatalf("%s status=%d, want %d", path, res.Code, want)
				}
			}
		})
	}
}

// TestDebugHeaderLoggerRedactsAuthorization checks the diagnostic line
// carries header names, actor and tracing values, and the Authorization
// prefix/length, but never the key itself.
func TestDebugHeaderLoggerRedactsAuthorization(t *testing.T) {
	var buf bytes.Buffer
	h := debugHeaderLogger(log.New(&buf, "", log.LstdFlags), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	req.Header.Set("nl-actor", "user@example.com")
	req.Header.Set("traceparent", "00-trace-span-01")
	req.Header.Set("x-ms-correlation-id", "corr-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := buf.String()
	if strings.Contains(line, "secret123") {
		t.Fatalf("log line leaks the key: %s", line)
	}
	for _, want := range []string{"request_id=", `prefix="Bearer "`, "len=16", "Authorization", "Traceparent", "nl-actor=\"user@example.com\"", "traceparent=\"00-trace-span-01\"", "x-ms-correlation-id=\"corr-1\""} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %q: %s", want, line)
		}
	}
}

// TestDebugHeaderLoggingOnlyWithFlag verifies New wires the logger only
// when NL_DEBUG_HEADERS=1, and that the wired logger stays redacted.
func TestDebugHeaderLoggingOnlyWithFlag(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	for _, tc := range []struct {
		flag    string
		wantLog bool
	}{
		{flag: "", wantLog: false},
		{flag: "1", wantLog: true},
	} {
		t.Run("flag="+tc.flag, func(t *testing.T) {
			buf.Reset()
			t.Setenv(debugHeadersEnv, tc.flag)
			handler, err := New(testDeps(t), NewRegistry(), &Options{APIToken: "test-token"})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer secret123")
			handler.ServeHTTP(httptest.NewRecorder(), req)
			got := buf.String()
			if strings.Contains(got, "nl-debug-headers") != tc.wantLog {
				t.Fatalf("debug line logged=%v, want %v: %q", !tc.wantLog, tc.wantLog, got)
			}
			if strings.Contains(got, "secret123") {
				t.Fatalf("log leaks the key: %q", got)
			}
		})
	}
}
