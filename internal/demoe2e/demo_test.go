package demoe2e_test

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestEnvironmentalDemoPipeline covers R10 through the MCP transport, using
// the real lane services, committed recipe and offline Workiva demo backend.
func TestEnvironmentalDemoPipeline(t *testing.T) {
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
	if len(listed.Tools) != 19 {
		t.Fatalf("listed %d tools, want 19", len(listed.Tools))
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
	decode(t, call("workiva_prepare_section", map[string]any{"section_id": "esg-environmental"}), &prepared)
	if prepared.SectionID != "esg-environmental" {
		t.Fatalf("section = %q", prepared.SectionID)
	}
	wantFindings := map[string]string{
		"Water discharge from undertaking production processes (m³)": "missing",
		"6. Business Travel": "material_change",
		"Electricity (as reflected in utility billings) - Renewable": "out_of_range",
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
		"energy_total": "1978", "electricity_renewable": "490", "electricity_non_renewable": "1280",
		"self_generated_electricity_renewable": "150", "self_generated_electricity_non_renewable": "58",
		"ghg_scope1": "812.5", "ghg_scope2_location": "585", "ghg_scope2_market": "520",
		"ghg_scope3_purchased_goods": "4050", "ghg_scope3_capital_goods": "820", "ghg_scope3_fuel_energy": "298",
		"ghg_scope3_upstream_transport": "545", "ghg_scope3_waste": "92", "ghg_scope3_business_travel": "165",
		"ghg_scope3_commuting": "224", "land_sealed_area": "12.6", "land_nature_onsite": "3.2",
		"land_nature_offsite": "1.6", "land_use_total": "17.4", "water_withdrawn_total": "18100",
		"water_withdrawn_high_stress": "2300", "waste_recycled_type1": "5400", "waste_disposed_type1": "2000",
		"waste_recycled_type2": "910", "waste_disposed_type2": "415",
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
	energy := factsByKey["energy_total"]
	if energy.Prior != "2030" || energy.Change != "-52" || energy.Direction != "down" || energy.Material ||
		!slices.Contains(energy.Display, "1.98 GWh") || !slices.Contains(energy.Display, "2.03 GWh") {
		t.Fatalf("energy fact = %+v", energy)
	}
	travel := factsByKey["ghg_scope3_business_travel"]
	if travel.Prior != "140" || travel.Change != "25" || !slices.Contains(travel.Display, "17.9%") || !travel.Material || travel.Direction != "up" {
		t.Fatalf("business travel fact = %+v", travel)
	}
	// A read-only comparison quotes the same server-computed facts as the narrative.
	var compared struct {
		SectionTitle   string `json:"section_title"`
		MaterialityPct string `json:"materiality_pct"`
		Items          []struct {
			factOutput
			CurrentCell struct {
				Range string `json:"range"`
			} `json:"current_cell"`
		} `json:"items"`
	}
	decode(t, call("workiva_compare_fields", map[string]any{"section_id": "esg-environmental", "fields": []any{"energy_total", "business travel"}}), &compared)
	if compared.SectionTitle != "Environmental disclosures" || compared.MaterialityPct != "10" || len(compared.Items) != 2 {
		t.Fatalf("comparison = %+v", compared)
	}
	for _, item := range compared.Items {
		want := factsByKey[item.Key]
		if item.ChangePct != want.ChangePct || item.Material != want.Material || !slices.Equal(item.Display, want.Display) || item.CurrentCell.Range == "" {
			t.Fatalf("compared %+v, prepared %+v", item, want)
		}
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
		key := strings.TrimSuffix(strings.TrimPrefix(item.FieldName, "rpt_env_"), "_2025")
		field, err := store.GetField(ctx, item.FieldName)
		if err != nil || field == nil {
			t.Fatalf("mapping %s: %v", item.FieldName, err)
		}
		if wantValues[key] == "" || item.After != wantValues[key] || item.Before != "" || item.Source != "src_env_"+key ||
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

	approved, err := os.ReadFile(abs("sections/verbatim/env-policy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Verbatim) != 1 || prepared.Verbatim[0].ID != "env-policy" || prepared.Verbatim[0].Text != string(approved) {
		t.Fatalf("verbatim does not match approved file: %+v", prepared.Verbatim)
	}
	// Every numeric expression is copied from Display, including its unit.
	narrative := "Total energy consumption was " + energy.Display[0] + ", compared with " + energy.Display[1] + " in the prior period."
	blocks := []textBlock{{Kind: "narrative", Text: narrative}, {Kind: "verbatim", VerbatimID: "env-policy", Text: prepared.Verbatim[0].Text}}
	var stagedText textPreview
	decode(t, call("workiva_stage_section_text", map[string]any{"section_id": prepared.SectionID, "blocks": blocks}), &stagedText)
	if stagedText.ID == "" || stagedText.Token == "" || stagedText.SectionID != prepared.SectionID || stagedText.Title != "Environmental disclosures" || !reflect.DeepEqual(stagedText.Blocks, blocks) {
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
		{"edited verbatim", "Verbatim block env-policy cannot be changed. Use the approved text exactly.", []textBlock{blocks[0], {Kind: "verbatim", VerbatimID: "env-policy", Text: "Revised policy. " + string(approved)}}},
		{"invented number", "The number 987654321 is not in the prepared facts for this section.", []textBlock{{Kind: "narrative", Text: narrative + " Energy consumption will reach 987654321 GWh."}, blocks[1]}},
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

type actorTransport struct{}

func (actorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer demo-e2e-token")
	req.Header.Set(mcpserver.ActorHeader, "analyst@example.com")
	return http.DefaultTransport.RoundTrip(req)
}

func resultText(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}
func decode(t *testing.T, result *mcp.CallToolResult, dst any) {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool refused: %s", resultText(result))
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatalf("decode tool output: %v", err)
	}
}
func assertCell(t *testing.T, ctx context.Context, client *workiva.Client, item previewItem, want string) {
	t.Helper()
	data, err := client.GetSheetData(ctx, item.SpreadsheetID, item.SheetID, item.Range, []string{"cells.value"})
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || len(data.Cells) != 1 || len(data.Cells[0]) != 1 {
		t.Fatalf("unexpected cells for %s: %+v", item.FieldName, data)
	}
	got := ""
	if value := data.Cells[0][0].Value; value != nil {
		got = fmt.Sprint(value)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", item.FieldName, got, want)
	}
}

type preparedOutput struct {
	SectionID string                 `json:"section_id"`
	Findings  []recipe.Finding       `json:"findings"`
	Facts     []factOutput           `json:"facts"`
	Verbatim  []recipe.VerbatimBlock `json:"verbatim"`
	ChangeSet struct {
		ID        string        `json:"change_set_id"`
		Token     string        `json:"confirm_token"`
		ExpiresAt string        `json:"expires_at"`
		Items     []previewItem `json:"items"`
	} `json:"change_set"`
	Summary string `json:"summary"`
}
type factOutput struct {
	Key, Current, Prior, Change, Direction string
	ChangePct                              string `json:"change_pct"`
	Material                               bool
	Display                                []string
}
type previewItem struct {
	FieldName                    string `json:"field_name"`
	SpreadsheetID                string `json:"spreadsheet_id"`
	SheetID                      string `json:"sheet_id"`
	Range, Before, After, Source string
}
type confirmedOutput struct {
	ID     string `json:"change_set_id"`
	Status string
	Items  []struct {
		FieldName             string `json:"field_name"`
		ReadBack              string `json:"read_back"`
		AuditID               string `json:"audit_id"`
		OperationURL          string `json:"operation_url"`
		Status, Before, After string
	}
}
type textBlock struct {
	Kind       string `json:"kind"`
	Text       string `json:"text"`
	VerbatimID string `json:"verbatim_id,omitempty"`
}
type textPreview struct {
	ID        string `json:"text_id"`
	Token     string `json:"confirm_token"`
	SectionID string `json:"section_id"`
	Title     string `json:"section_title"`
	Blocks    []textBlock
}
type textResult struct {
	Status           string
	SectionID        string `json:"section_id"`
	DocumentID       string `json:"document_id"`
	WorkivaSectionID string `json:"workiva_section_id"`
	AuditID          string `json:"audit_id"`
}
