package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/audit"
)

func seedAuditTrail(t *testing.T, env testEnv) {
	t.Helper()
	ctx := context.Background()
	targets := []string{"sp-1/sh-1/B3", "sp-1/sh-1/B4", "sp-1/sh-1/B3"}
	for _, target := range targets {
		if _, err := env.deps.Audit.Append(ctx, audit.Entry{
			Actor: "copilot", Tool: "workiva_update_field", Action: "write",
			Target:     target,
			BeforeJSON: `{"value":"1"}`, AfterJSON: `{"value":"2"}`,
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

func TestAuditTrailReturnsNewestFirstWithLimit(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	seedAuditTrail(t, env)

	result := callTool(t, env.deps, AuditTrail(), map[string]any{"limit": 2})
	if result.IsError {
		t.Fatalf("audit_trail returned error: %+v", result.Content)
	}
	sc := structuredContent(t, result)
	if sc["count"] != float64(2) {
		t.Errorf("count = %v, want 2", sc["count"])
	}
	entries, _ := sc["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	first, _ := entries[0].(map[string]any)
	second, _ := entries[1].(map[string]any)
	if first["hash_version"] != float64(2) || second["hash_version"] != float64(2) {
		t.Fatalf("MCP audit entries omit v2 hash_version: first=%v second=%v", first, second)
	}
	if first["seq"].(float64) <= second["seq"].(float64) {
		t.Errorf("entries not newest first: seq %v then %v", first["seq"], second["seq"])
	}
}

func TestAuditTrailFiltersByTarget(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	seedAuditTrail(t, env)

	result := callTool(t, env.deps, AuditTrail(), map[string]any{"target": "sp-1/sh-1/B4"})
	if result.IsError {
		t.Fatalf("audit_trail returned error: %+v", result.Content)
	}
	sc := structuredContent(t, result)

	// The seeded write plus this very call (the server middleware logs
	// every tool call, and the target argument is used as the entry
	// target), both matching the filter.
	if sc["count"] != float64(2) {
		t.Fatalf("count = %v, want 2", sc["count"])
	}
	entries, _ := sc["entries"].([]any)
	var sawWrite bool
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		if e["target"] != "sp-1/sh-1/B4" {
			t.Errorf("entry target = %v, want sp-1/sh-1/B4", e["target"])
		}
		if e["action"] == "write" {
			sawWrite = true
		}
	}
	if !sawWrite {
		t.Error("filtered trail misses the seeded write entry")
	}
}

func TestAuditTrailFiltersDeniedAndAmbiguousHistoricalEntriesBeforeLimit(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	env.deps.Cfg.AllowedResources = map[string][]string{"sp-1": {"sh-1"}}
	ctx := context.Background()
	entries := []audit.Entry{
		{Tool: "workiva_update_field", Action: "write", Target: "sp-1/sh-1/B3", BeforeJSON: `{"value":"safe-old"}`, AfterJSON: `{"value":"safe-new"}`, WorkivaOpURL: "/operations/safe"},
		{Tool: "workiva_update_field", Action: "write", Target: "denied/sh-x/C9", BeforeJSON: `{"value":"secret-old"}`, AfterJSON: `{"value":"secret-new"}`, WorkivaOpURL: "/operations/secret"},
		{Tool: "legacy", Target: "ambiguous-secret", AfterJSON: `{"field":"secret"}`},
		{Tool: "health", Action: "ready"},
	}
	for _, entry := range entries {
		if _, err := env.deps.Audit.Append(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}

	result := callTool(t, env.deps, AuditTrail(), map[string]any{"limit": 2})
	if result.IsError {
		t.Fatalf("audit_trail error: %+v", result.Content)
	}
	content := structuredContent(t, result)
	got := content["entries"].([]any)
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2 after authorization filtering", len(got))
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "denied") {
		t.Fatalf("denied data leaked: %s", encoded)
	}
}

func TestAuditTrailExplicitDeniedOrAmbiguousTargetReturnsNoEntries(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	env.deps.Cfg.AllowedResources = map[string][]string{"sp-1": {"sh-1"}}
	for _, target := range []string{"denied/sh-x/C9", "ambiguous-secret"} {
		if _, err := env.deps.Audit.Append(context.Background(), audit.Entry{Target: target, AfterJSON: `{"value":"secret"}`}); err != nil {
			t.Fatal(err)
		}
		result := callTool(t, env.deps, AuditTrail(), map[string]any{"target": target})
		if result.IsError {
			t.Fatalf("audit_trail error: %+v", result.Content)
		}
		if got := structuredContent(t, result)["count"]; got != float64(0) {
			t.Fatalf("target %q count = %v, want 0", target, got)
		}
	}
}

func TestAuditTrailRejectsNonCanonicalRichEntries(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	env.deps.Cfg.AllowedResources = map[string][]string{"sp-1": {"sh-1"}}
	cases := []audit.Entry{
		{Tool: "workiva_sync_mapping", Action: "sync", Target: "sp-1/sh-1/extra", AfterJSON: `{"value":"leak-sync-extra"}`},
		{Tool: "workiva_read_range", Action: "read", Target: "sp-1/sh-1/B3/extra", AfterJSON: `{"value":"leak-read-extra"}`},
		{Tool: "workiva_update_field", Action: "write", Target: "sp-1/sh-1/B3/extra", BeforeJSON: `{"value":"leak-write-before"}`, AfterJSON: `{"value":"leak-write-after"}`, WorkivaOpURL: "/operations/leak-write"},
		{Tool: "workiva_sync_mapping", Action: "read", Target: "sp-1/sh-1", AfterJSON: `{"value":"leak-sync-action"}`},
		{Tool: "workiva_read_range", Action: "write", Target: "sp-1/sh-1/B3", AfterJSON: `{"value":"leak-read-action"}`},
		{Tool: "workiva_update_field", Action: "sync", Target: "sp-1/sh-1/B3", AfterJSON: `{"value":"leak-write-action"}`},
		{Tool: "historical_tool", Action: "historical_action", Target: "sp-1/sh-1/B3", AfterJSON: `{"value":"leak-unknown"}`},
		{Tool: "workiva_update_field", Action: "write", Target: "sp-1//B3", AfterJSON: `{"value":"leak-empty"}`},
	}
	for _, entry := range cases {
		if _, err := env.deps.Audit.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.deps.Audit.Append(context.Background(), audit.Entry{
		Tool: "workiva_update_field", Action: "write", Target: "sp-1/sh-1/B4", AfterJSON: `{"value":"visible"}`,
	}); err != nil {
		t.Fatal(err)
	}

	result := callTool(t, env.deps, AuditTrail(), map[string]any{})
	if result.IsError {
		t.Fatalf("audit_trail error: %+v", result.Content)
	}
	encoded, _ := json.Marshal(structuredContent(t, result)["entries"])
	if strings.Contains(string(encoded), "leak-") {
		t.Fatalf("non-canonical audit data leaked through unrestricted query: %s", encoded)
	}
	if !strings.Contains(string(encoded), "visible") {
		t.Fatalf("canonical authorized audit entry missing: %s", encoded)
	}

	for _, entry := range cases {
		result := callTool(t, env.deps, AuditTrail(), map[string]any{"target": entry.Target})
		if result.IsError {
			t.Fatalf("audit_trail target %q error: %+v", entry.Target, result.Content)
		}
		encoded, _ := json.Marshal(structuredContent(t, result)["entries"])
		if strings.Contains(string(encoded), "leak-") {
			t.Fatalf("non-canonical audit data leaked through explicit target %q: %s", entry.Target, encoded)
		}
	}
}

func TestAuditTrailDefaultLimit(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	// Seed more entries than the default limit of 20.
	for i := 0; i < 25; i++ {
		if _, err := env.deps.Audit.Append(context.Background(), audit.Entry{
			Actor: "copilot", Tool: "workiva_get_field", Action: "read",
			Target: "sp-1/sh-1/B3",
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	result := callTool(t, env.deps, AuditTrail(), map[string]any{})
	if result.IsError {
		t.Fatalf("audit_trail returned error: %+v", result.Content)
	}
	sc := structuredContent(t, result)
	if sc["count"] != float64(20) {
		t.Errorf("count = %v, want 20 (default limit)", sc["count"])
	}
}

func TestAuditTrailCapsLimitAtOneHundred(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	for i := 0; i < 5; i++ {
		if _, err := env.deps.Audit.Append(context.Background(), audit.Entry{
			Actor: "copilot", Tool: "workiva_get_field", Action: "read",
			Target: "sp-1/sh-1/B3",
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	result := callTool(t, env.deps, AuditTrail(), map[string]any{"limit": 500})
	if result.IsError {
		t.Fatalf("audit_trail returned error: %+v", result.Content)
	}
	sc := structuredContent(t, result)
	// 5 seeded entries plus the call entry logged by the server
	// middleware; the 500 limit is capped at 100 so everything shows.
	if sc["count"] != float64(6) {
		t.Errorf("count = %v, want 6 (5 seeded plus the logged call)", sc["count"])
	}
}

func TestAuditTrailFindsAllowedBeyondTenThousandDeniedRows(t *testing.T) {
	env := newTestEnv(t, tokenHandler(t, http.NotFound))
	env.deps.Cfg.AllowedResources = map[string][]string{"sp-1": {"sh-1"}}
	ctx := context.Background()
	if _, err := env.deps.Audit.Append(ctx, audit.Entry{Tool: "workiva_read_range", Action: "read", Target: "sp-1/sh-1/B3", Actor: "older-allowed"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10001; i++ {
		e := audit.Entry{Tool: "workiva_read_range", Action: "read", Target: "denied/sh/B3", AfterJSON: "secret"}
		if i%2 == 0 {
			e.Target = "sp-1/sh-1/B3"
			e.Action = "ambiguous"
		}
		if _, err := env.deps.Audit.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"", "sp-1/sh-1/B3"} {
		res := callTool(t, env.deps, AuditTrail(), map[string]any{"limit": 100, "target": target})
		if res.IsError {
			t.Fatal(res.Content)
		}
		data, _ := json.Marshal(structuredContent(t, res)["entries"])
		if !strings.Contains(string(data), "older-allowed") || strings.Contains(string(data), "secret") || strings.Contains(string(data), "denied") {
			t.Fatalf("results=%s", data)
		}
	}
}
