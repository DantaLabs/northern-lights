package tools

import (
	"context"
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
