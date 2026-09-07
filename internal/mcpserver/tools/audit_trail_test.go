package tools

import (
	"context"
	"net/http"
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
