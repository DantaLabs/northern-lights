package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/sectiontext"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sectionTextRecipe struct{ prepared bool }

func (r *sectionTextRecipe) Prepare(context.Context, string, string) (recipe.Prepared, error) {
	return recipe.Prepared{}, errors.New("fake recipe")
}
func (r *sectionTextRecipe) Sections() []recipe.SectionInfo {
	return []recipe.SectionInfo{{ID: "environmental", Title: "Environmental disclosures", DocumentID: "demo-doc-annual-report", Period: "2025", PriorPeriod: "2024"}}
}
func (r *sectionTextRecipe) LatestFacts(_ context.Context, actor, section string) ([]recipe.Fact, []recipe.VerbatimBlock, error) {
	if !r.prepared || actor != "tester@example.com" || section != "environmental" {
		return nil, nil, nil
	}
	return []recipe.Fact{{Label: "Energy", Display: []string{"8.1%", "1.24 GWh"}}}, []recipe.VerbatimBlock{{ID: "policy", Text: "Approved policy."}}, nil
}
func TestSectionTextToolsFlowAndExactRefusals(t *testing.T) {
	env := newTestEnv(t, http.NotFound)
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	env.deps.Audit = log
	base, _ := url.Parse("https://api.eu.wdesk.com")
	client := workiva.NewDemoClient(base)
	r := &sectionTextRecipe{}
	env.deps.SectionText = sectiontext.New(demoenv.Env{DB: db, Audit: log, Client: client, Cfg: &config.Config{AuthMode: config.AuthModeAPIKey}}, r, r)
	session := serveDemoTools(t, env.deps)
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	draft := map[string]any{"section_id": "environmental", "blocks": []map[string]any{{"kind": "narrative", "text": "Energy consumption fell 8.1% to 1.24 GWh in 2025"}, {"kind": "narrative", "text": "In 2025 compared with 2024."}, {"kind": "verbatim", "verbatim_id": "policy", "text": "Approved policy."}}}
	res := call(stageSectionTextName, draft)
	if !res.IsError || !strings.Contains(resultText(res), "Prepare the section first.") {
		t.Fatalf("prepare gate: %+v", res)
	}
	r.prepared = true
	res = call(stageSectionTextName, map[string]any{"section_id": "environmental", "blocks": []map[string]any{{"kind": "narrative", "text": "fell 9%"}}})
	if !res.IsError || !strings.Contains(resultText(res), "The number 9% is not in the prepared facts for this section.") {
		t.Fatalf("number refusal: %+v", res)
	}
	res = call(stageSectionTextName, map[string]any{"section_id": "environmental", "blocks": []map[string]any{{"kind": "verbatim", "verbatim_id": "policy", "text": "Approved Policy."}}})
	if !res.IsError || !strings.Contains(resultText(res), "Verbatim block policy cannot be changed. Use the approved text exactly.") || !strings.Contains(resultText(res), "position: 10") {
		t.Fatalf("verbatim refusal: %+v", res)
	}
	res = call(stageSectionTextName, draft)
	if res.IsError {
		t.Fatal(resultText(res))
	}
	out := structuredContent(t, res)
	summary, _ := out["summary"].(string)
	if strings.Count(summary, "[narrative]") != 2 || strings.Count(summary, "[verbatim]") != 1 || !strings.Contains(summary, "Approved policy.") {
		t.Fatalf("preview: %s", summary)
	}
	res = call(confirmSectionTextName, map[string]any{"text_id": out["text_id"], "confirm_token": out["confirm_token"]})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	confirmed := structuredContent(t, res)
	if confirmed["status"] != "completed" || confirmed["document_id"] != "demo-doc-annual-report" || confirmed["audit_id"] == "" {
		t.Fatalf("confirm: %+v", confirmed)
	}
	paragraphs, err := client.ReadSectionText(context.Background(), "demo-doc-annual-report", confirmed["workiva_section_id"].(string))
	if err != nil || len(paragraphs) != 3 {
		t.Fatalf("paragraphs: %q %v", paragraphs, err)
	}
}

func TestSectionTextToolsRequireActor(t *testing.T) {
	env := newTestEnv(t, http.NotFound)
	env.deps.SectionText = sectiontext.New(demoenv.Env{}, nil, nil)
	for _, tc := range []struct {
		tool mcpserver.Tool
		args map[string]any
	}{{StageSectionText(), map[string]any{"section_id": "section", "blocks": []map[string]any{{"kind": "narrative", "text": "Text"}}}}, {ConfirmSectionText(), map[string]any{"text_id": "text", "confirm_token": "token"}}} {
		res := callToolWithActor(t, env.deps, tc.tool, "", tc.args)
		if !res.IsError || !strings.Contains(resultText(res), "authenticated actor") {
			t.Fatalf("anonymous call accepted: %+v", res)
		}
	}
}

func newSectionTextJSONTest(t *testing.T) (*mcp.ClientSession, *sectionTextRecipe, *sql.DB, *workiva.Client) {
	t.Helper()
	env := newTestEnv(t, http.NotFound)
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	env.deps.Audit = log
	base, _ := url.Parse("https://api.eu.wdesk.com")
	client := workiva.NewDemoClient(base)
	r := &sectionTextRecipe{}
	env.deps.SectionText = sectiontext.New(demoenv.Env{DB: db, Audit: log, Client: client, Cfg: &config.Config{AuthMode: config.AuthModeAPIKey}}, r, r)
	return serveDemoTools(t, env.deps), r, db, client
}

func TestSectionTextJSONFlowAndGuards(t *testing.T) {
	session, recipe, db, client := newSectionTextJSONTest(t)
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	draft := `[{"kind":"narrative","text":"Energy consumption fell 8.1% to 1.24 GWh in 2025"},{"kind":"narrative","text":"In 2025 compared with 2024."},{"kind":"verbatim","verbatim_id":"policy","text":"Approved policy."}]`
	args := map[string]any{"section_id": "environmental", "blocks_json": draft}
	res := call(stageSectionTextName, args)
	if !res.IsError || !strings.Contains(resultText(res), "Prepare the section first.") {
		t.Fatalf("prepare gate: %+v", res)
	}
	recipe.prepared = true
	for _, tc := range []struct{ draft, want string }{
		{`[{"kind":"narrative","text":"fell 9%"}]`, "The number 9% is not in the prepared facts for this section."},
		{`[{"kind":"verbatim","verbatim_id":"policy","text":"Approved Policy."}]`, "Verbatim block policy cannot be changed. Use the approved text exactly."},
		{`[{"kind":"verbatim","text":"Approved policy."}]`, "cannot be changed"},
		{`[{"kind":"verbatim","verbatim_id":"unknown","text":"Approved policy."}]`, "cannot be changed"},
	} {
		res := call(stageSectionTextName, map[string]any{"section_id": "environmental", "blocks_json": tc.draft})
		if !res.IsError || !strings.Contains(resultText(res), tc.want) {
			t.Fatalf("guard for %s: %+v", tc.draft, res)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM section_text_previews").Scan(&count); err != nil || count != 0 {
		t.Fatalf("refusals staged %d previews: %v", count, err)
	}
	res = call(stageSectionTextName, args)
	if res.IsError {
		t.Fatal(resultText(res))
	}
	out := structuredContent(t, res)
	summary, _ := out["summary"].(string)
	if strings.Count(summary, "[narrative]") != 2 || strings.Count(summary, "[verbatim]") != 1 || !strings.Contains(summary, "Approved policy.") {
		t.Fatalf("preview: %s", summary)
	}
	res = call(confirmSectionTextName, map[string]any{"text_id": out["text_id"], "confirm_token": out["confirm_token"]})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	confirmed := structuredContent(t, res)
	auditID, auditIDOK := confirmed["audit_id"].(string)
	if confirmed["status"] != "completed" || confirmed["document_id"] != "demo-doc-annual-report" || !auditIDOK || auditID == "" {
		t.Fatalf("confirm: %+v", confirmed)
	}
	paragraphs, err := client.ReadSectionText(context.Background(), "demo-doc-annual-report", confirmed["workiva_section_id"].(string))
	if err != nil || len(paragraphs) != 3 || paragraphs[2] != "Approved policy." {
		t.Fatalf("paragraphs: %q %v", paragraphs, err)
	}
}

func TestSectionTextJSONRequiresActor(t *testing.T) {
	env := newTestEnv(t, http.NotFound)
	env.deps.SectionText = sectiontext.New(demoenv.Env{}, nil, nil)
	res := callToolWithActor(t, env.deps, StageSectionText(), "", map[string]any{"section_id": "environmental", "blocks_json": `[{"kind":"narrative","text":"Text"}]`})
	if !res.IsError || !strings.Contains(resultText(res), "authenticated actor") {
		t.Fatalf("anonymous JSON call accepted: %+v", res)
	}
}

func TestSectionTextJSONRejectsInvalidInputsWithoutStaging(t *testing.T) {
	session, recipe, db, _ := newSectionTextJSONTest(t)
	recipe.prepared = true
	encode := func(value any) string {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tooMany := make([]map[string]any, 21)
	for i := range tooMany {
		tooMany[i] = map[string]any{"kind": "narrative", "text": "Text"}
	}
	valid := `[{"kind":"narrative","text":"Text"}]`
	cases := []struct {
		name string
		args map[string]any
	}{
		{"neither", map[string]any{}},
		{"both", map[string]any{"blocks": []map[string]any{{"kind": "narrative", "text": "Text"}}, "blocks_json": valid}},
		{"both with null array", map[string]any{"blocks": nil, "blocks_json": valid}},
		{"both with null string", map[string]any{"blocks": []map[string]any{{"kind": "narrative", "text": "Text"}}, "blocks_json": nil}},
		{"null array", map[string]any{"blocks": nil}},
		{"null string", map[string]any{"blocks_json": nil}},
		{"nonstring", map[string]any{"blocks_json": []any{}}},
		{"empty string", map[string]any{"blocks_json": ""}},
		{"malformed", map[string]any{"blocks_json": "[{"}},
		{"trailing value", map[string]any{"blocks_json": valid + " []"}},
		{"object", map[string]any{"blocks_json": `{}`}},
		{"scalar", map[string]any{"blocks_json": `"Text"`}},
		{"encoded null", map[string]any{"blocks_json": `null`}},
		{"empty array", map[string]any{"blocks_json": `[]`}},
		{"missing kind", map[string]any{"blocks_json": `[{"text":"Text"}]`}},
		{"missing text", map[string]any{"blocks_json": `[{"kind":"narrative"}]`}},
		{"extra key", map[string]any{"blocks_json": `[{"kind":"narrative","text":"Text","extra":true}]`}},
		{"case variant key", map[string]any{"blocks_json": `[{"kind":"narrative","text":"Text","Text":"Other"}]`}},
		{"numeric text", map[string]any{"blocks_json": `[{"kind":"narrative","text":12}]`}},
		{"null text", map[string]any{"blocks_json": `[{"kind":"narrative","text":null}]`}},
		{"invalid kind", map[string]any{"blocks_json": `[{"kind":"other","text":"Text"}]`}},
		{"null block", map[string]any{"blocks_json": `[null]`}},
		{"nonobject block", map[string]any{"blocks_json": `["Text"]`}},
		{"numeric id", map[string]any{"blocks_json": `[{"kind":"narrative","text":"Text","verbatim_id":1}]`}},
		{"long id", map[string]any{"blocks_json": encode([]map[string]any{{"kind": "narrative", "text": "Text", "verbatim_id": strings.Repeat("x", 129)}})}},
		{"long text", map[string]any{"blocks_json": encode([]map[string]any{{"kind": "narrative", "text": strings.Repeat("x", 4001)}})}},
		{"too many blocks", map[string]any{"blocks_json": encode(tooMany)}},
		{"long JSON string", map[string]any{"blocks_json": valid + strings.Repeat(" ", 100000)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["section_id"] = "environmental"
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: stageSectionTextName, Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Fatalf("invalid input accepted: %+v", res)
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM section_text_previews").Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid input staged %d previews: %v", count, err)
			}
		})
	}
}

func TestSectionTextJSONAcceptsArrayBounds(t *testing.T) {
	session, recipe, _, _ := newSectionTextJSONTest(t)
	recipe.prepared = true
	blocks := make([]map[string]any, 20)
	for i := range blocks {
		blocks[i] = map[string]any{"kind": "narrative", "text": strings.Repeat("x", 4000), "verbatim_id": strings.Repeat("x", 128)}
	}
	for _, count := range []int{1, 20} {
		encoded, err := json.Marshal(blocks[:count])
		if err != nil {
			t.Fatal(err)
		}
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: stageSectionTextName, Arguments: map[string]any{"section_id": "environmental", "blocks_json": string(encoded)}})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatal(resultText(res))
		}
	}
}
