package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/changeset"
)

type changeSetFake struct {
	actor, id, token string
	err              error
}

func (*changeSetFake) Stage(context.Context, string, []changeset.Item) (changeset.Preview, error) {
	return changeset.Preview{}, errors.New("unused")
}
func (f *changeSetFake) Confirm(_ context.Context, actor, id, token string) (changeset.Result, error) {
	f.actor, f.id, f.token = actor, id, token
	return changeset.Result{ID: id, Status: "completed", Summary: "energy: 1 -> 2 (completed)\nwater: 3 -> 4 (completed)", Items: []changeset.ItemResult{
		{FieldName: "energy", Before: "1", After: "2", ReadBack: "2", Status: "completed", AuditID: "audit-energy", OperationURL: "https://provider.invalid/op"},
		{FieldName: "water", Before: "3", After: "4", ReadBack: "4", Status: "completed", AuditID: "audit-water"},
	}}, f.err
}

func TestChangeSet_S9_MCPResultAndSummary(t *testing.T) {
	deps := newTestEnv(t, http.NotFound).deps
	fake := &changeSetFake{}
	deps.ChangeSets = fake
	result := callTool(t, deps, ConfirmChangeSet(), map[string]any{"change_set_id": "cs-1", "confirm_token": "token"})
	if result.IsError {
		t.Fatalf("error: %s", resultText(result))
	}
	var out confirmChangeSetOutput
	if err := json.Unmarshal([]byte(resultText(result)), &out); err != nil {
		t.Fatal(err)
	}
	if out.ChangeSetID != "cs-1" || out.Status != "completed" || out.NLAuditID == "" || len(out.Items) != 2 || out.Items[0].AuditID != "audit-energy" {
		t.Fatalf("output=%+v", out)
	}
	text := out.Summary
	for _, want := range []string{"energy: 1 -> 2 (completed)", "water: 3 -> 4 (completed)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if fake.id != "cs-1" || fake.token != "token" || fake.actor == "" || fake.actor == "unknown" {
		t.Fatalf("bindings=%+v", fake)
	}
}

func TestChangeSet_MCPConfirmationError(t *testing.T) {
	deps := newTestEnv(t, http.NotFound).deps
	deps.ChangeSets = &changeSetFake{err: changeset.ErrExpiredToken}
	session := serveDemoTools(t, deps)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: confirmChangeSetName, Arguments: map[string]any{"change_set_id": "cs-1", "confirm_token": "expired"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(resultText(result), "expired") {
		t.Fatalf("result=%+v", result)
	}
}
