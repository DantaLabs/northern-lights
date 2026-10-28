package demoe2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/bootstrap"
	"github.com/dantalabs/northern-lights/internal/changeset"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/mcpserver/tools"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/sectiontext"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSocialDemoPipeline exercises the repeat act through the MCP transport, using
// the real lane services, committed recipe and offline Workiva demo backend.
func TestSocialDemoPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	abs := func(rel string) string {
		t.Helper()
		p, err := filepath.Abs(filepath.Join("..", "..", "configs", "demo", rel))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("NL_DEMO_MODE", "true")
	t.Setenv("NL_DEMO_PIPELINE", "true")
	t.Setenv("NL_AUTH_MODE", "api_key")
	t.Setenv("NL_DEMO_ESG_DATA", abs("data"))
	t.Setenv("NL_DEMO_SECTIONS_DIR", abs("sections"))
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DemoMode || !cfg.DemoPipelineEnabled {
		t.Fatal("demo mode and pipeline must be enabled")
	}
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := mapping.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.LoadDemoMappings(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := bootstrap.LoadMappings(ctx, abs("mappings-demo.yaml"), store); err != nil {
		t.Fatal(err)
	}
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("https://demo.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client := workiva.NewDemoClient(base)
	backend := workivaprovider.NewRouter(client)
	env := demoenv.Env{DB: db, Store: store, Audit: log, Client: backend, Cfg: cfg}
	changes := changeset.New(env)
	recipes, facts, err := recipe.New(env, changes)
	if err != nil {
		t.Fatal(err)
	}
	deps := mcpserver.Deps{Client: backend, Store: store, Audit: log, Cfg: cfg,
		ChangeSets: changes, Recipes: recipes, Facts: facts, SectionText: sectiontext.New(env, recipes, facts)}
	reg := mcpserver.NewRegistry()
	for _, tool := range tools.All() {
		reg.Register(tool)
	}
	if cfg.DemoPipelineEnabled {
		for _, tool := range tools.Demo() {
			reg.Register(tool)
		}
	}
	handler, err := mcpserver.New(deps, reg, &mcpserver.Options{APIToken: "demo-e2e-token"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "demo-e2e", Version: "test"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: actorTransport{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 20 {
		t.Fatalf("listed %d tools, want 20", len(listed.Tools))
	}
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}

	var prepared preparedOutput
	decode(t, call("workiva_prepare_section", map[string]any{"section_id": "esg-social"}), &prepared)
	if prepared.SectionID != "esg-social" {
		t.Fatalf("section = %q", prepared.SectionID)
	}
	wantFindings := map[string]string{
		"Number of annual training hours per employee - Female": "material_change",
	}
	if len(prepared.Findings) != len(wantFindings) {
		t.Fatalf("findings = %+v", prepared.Findings)
	}
	for _, finding := range prepared.Findings {
		if wantFindings[finding.Field] != finding.Kind || finding.Severity == "" || finding.Message == "" {
			t.Fatalf("unexpected finding: %+v", finding)
		}
		delete(wantFindings, finding.Field)
	}
	// Independent expected current-year values from the committed synthetic data.
	wantValues := map[string]string{
		"employees_total": "428", "employees_permanent": "402", "employees_male": "238",
		"employees_female": "183", "employees_other": "5", "employees_not_reported": "2",
		"employees_left": "41", "employees_start": "412", "employees_end": "428",
		"accidents_recordable": "11", "fatalities_work_related": "0",
		"employees_collective_bargaining": "281", "training_hours_male": "19",
		"training_hours_female": "25.5", "managers_male": "32", "managers_female": "15",
	}
	if len(prepared.Facts) != len(wantValues) {
		t.Fatalf("facts = %d, want %d", len(prepared.Facts), len(wantValues))
	}
	factsByKey := map[string]factOutput{}
	for _, fact := range prepared.Facts {
		if wantValues[fact.Key] != fact.Current || len(fact.Display) == 0 || fact.Prior == "" || fact.Direction == "" {
			t.Fatalf("unexpected fact: %+v", fact)
		}
		if _, duplicate := factsByKey[fact.Key]; duplicate {
			t.Fatalf("duplicate fact %s", fact.Key)
		}
		factsByKey[fact.Key] = fact
	}
	training := factsByKey["training_hours_female"]
	if training.Prior != "22" || training.Change != "3.5" || training.Direction != "up" || !training.Material ||
		!slices.Contains(training.Display, "15.9%") || !slices.Contains(training.Display, "16%") || !slices.Contains(training.Display, "25.5 hours") {
		t.Fatalf("female training fact = %+v", training)
	}
	employees := factsByKey["employees_total"]
	if employees.Prior != "412" || employees.Change != "16" || employees.Material || employees.Direction != "up" {
		t.Fatalf("employees fact = %+v", employees)
	}
	preview := prepared.ChangeSet
	if preview.ID == "" || preview.Token == "" || len(preview.Items) != len(wantValues) {
		t.Fatal("missing change set approval or wrong item count")
	}
	expiry, err := time.Parse(time.RFC3339, preview.ExpiresAt)
	if err != nil || !expiry.After(time.Now()) {
		t.Fatalf("change set expiry = %q: %v", preview.ExpiresAt, err)
	}
	byField := map[string]previewItem{}
	for _, item := range preview.Items {
		key := strings.TrimSuffix(strings.TrimPrefix(item.FieldName, "rpt_soc_"), "_2025")
		field, err := store.GetField(ctx, item.FieldName)
		if err != nil || field == nil {
			t.Fatalf("mapping %s: %v", item.FieldName, err)
		}
		if wantValues[key] == "" || item.After != wantValues[key] || item.Before != "" || item.Source != "src_soc_"+key ||
			item.SpreadsheetID != field.SpreadsheetID || item.SheetID != field.SheetID || item.Range != field.CellRange {
			t.Fatalf("unexpected preview: %+v", item)
		}
		if _, duplicate := byField[item.FieldName]; duplicate {
			t.Fatalf("duplicate preview field %s", item.FieldName)
		}
		byField[item.FieldName] = item
		assertCell(t, ctx, client, item, "")
		if !strings.Contains(prepared.Summary, item.FieldName+":  -> "+item.After+" (awaiting_confirmation)") {
			t.Fatalf("summary missing preview for %s", item.FieldName)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE action='write'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("prepare wrote audit rows: count=%d err=%v", count, err)
	}

	var confirmed confirmedOutput
	decode(t, call("workiva_confirm_change_set", map[string]any{"change_set_id": preview.ID, "confirm_token": preview.Token}), &confirmed)
	if confirmed.ID != preview.ID || confirmed.Status != "completed" || len(confirmed.Items) != len(byField) {
		t.Fatalf("confirm = %+v", confirmed)
	}
	seen := map[string]bool{}
	for _, item := range confirmed.Items {
		staged, ok := byField[item.FieldName]
		if !ok || seen[item.FieldName] || item.Status != "completed" || item.ReadBack != staged.After ||
			item.Before != staged.Before || item.After != staged.After || item.AuditID == "" || item.OperationURL == "" {
			t.Fatalf("written field = %+v; preview = %+v", item, staged)
		}
		seen[item.FieldName] = true
		assertCell(t, ctx, client, staged, staged.After)
		// Filter out the MCP middleware's per-call rows; these are the per-field writes.
		var actor, auditID, beforeJSON, afterJSON, op string
		target := staged.SpreadsheetID + "/" + staged.SheetID + "/" + staged.Range
		err := db.QueryRowContext(ctx, `SELECT actor,audit_id,before_json,after_json,workiva_op_url FROM audit_log WHERE tool='workiva_confirm_change_set' AND action='write' AND target=?`, target).
			Scan(&actor, &auditID, &beforeJSON, &afterJSON, &op)
		if err != nil {
			t.Fatalf("audit for %s: %v", item.FieldName, err)
		}
		var before, after map[string]string
		if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(afterJSON), &after); err != nil {
			t.Fatal(err)
		}
		if actor != "analyst@example.com" || auditID != item.AuditID || op != item.OperationURL ||
			before["change_set_id"] != preview.ID || before["field"] != item.FieldName || before["value"] != staged.Before ||
			after["change_set_id"] != preview.ID || after["field"] != item.FieldName || after["value"] != staged.After || after["intended"] != staged.After || after["status"] != "completed" {
			t.Fatalf("audit mismatch for %s: before=%s after=%s", item.FieldName, beforeJSON, afterJSON)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE tool='workiva_confirm_change_set' AND action='write'`).Scan(&count); err != nil || count != len(wantValues) {
		t.Fatalf("per-field audit count=%d, want %d: %v", count, len(wantValues), err)
	}

	approved, err := os.ReadFile(abs("sections/verbatim/social-policy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Verbatim) != 1 || prepared.Verbatim[0].ID != "social-policy" || prepared.Verbatim[0].Text != string(approved) {
		t.Fatalf("verbatim does not match approved file: %+v", prepared.Verbatim)
	}
	// Every numeric expression is copied from Display, including its unit.
	narrative := "Annual training hours per female employee rose 16% to 25.5 hours. Other workforce and safety indicators remained broadly stable."
	blocks := []textBlock{{Kind: "narrative", Text: narrative}, {Kind: "verbatim", VerbatimID: "social-policy", Text: prepared.Verbatim[0].Text}}
	var stagedText textPreview
	decode(t, call("workiva_stage_section_text", map[string]any{"section_id": prepared.SectionID, "blocks": blocks}), &stagedText)
	if stagedText.ID == "" || stagedText.Token == "" || stagedText.SectionID != prepared.SectionID || stagedText.Title != "3.1 Social performance" || !reflect.DeepEqual(stagedText.Blocks, blocks) {
		t.Fatalf("text preview = %+v", stagedText)
	}
	var writtenText textResult
	decode(t, call("workiva_confirm_section_text", map[string]any{"text_id": stagedText.ID, "confirm_token": stagedText.Token}), &writtenText)
	if writtenText.Status != "completed" || writtenText.SectionID != prepared.SectionID || writtenText.DocumentID != "demo-doc-annual-report" || writtenText.WorkivaSectionID == "" || writtenText.AuditID == "" {
		t.Fatalf("confirmed text = %+v", writtenText)
	}
	wantParagraphs := strings.Split(narrative+"\n"+string(approved), "\n")
	assertText := func(t *testing.T) {
		t.Helper()
		paragraphs, err := client.ReadSectionText(ctx, writtenText.DocumentID, writtenText.WorkivaSectionID)
		if err != nil || !reflect.DeepEqual(paragraphs, wantParagraphs) {
			t.Fatalf("section text = %q, want %q: %v", paragraphs, wantParagraphs, err)
		}
	}
	assertText(t)
	for _, tc := range []struct {
		name, refusal string
		blocks        []textBlock
	}{
		{"edited verbatim", "Verbatim block social-policy cannot be changed. Use the approved text exactly.", []textBlock{blocks[0], {Kind: "verbatim", VerbatimID: "social-policy", Text: "Revised policy. " + string(approved)}}},
		{"invented number", "The number 987654321 is not in the prepared facts for this section.", []textBlock{{Kind: "narrative", Text: narrative + " Training will reach 987654321 hours."}, blocks[1]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := call("workiva_stage_section_text", map[string]any{"section_id": prepared.SectionID, "blocks": tc.blocks})
			if !result.IsError || !strings.Contains(resultText(result), tc.refusal) {
				t.Fatalf("refusal = %+v, want %q", result, tc.refusal)
			}
			assertText(t)
		})
	}
	if err := log.Verify(ctx); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
}
