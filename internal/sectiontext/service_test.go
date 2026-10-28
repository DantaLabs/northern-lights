package sectiontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
	"github.com/dantalabs/northern-lights/internal/workiva"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

type fakeRecipe struct {
	facts    []recipe.Fact
	verbatim []recipe.VerbatimBlock
	err      error
}

func (f *fakeRecipe) LatestFacts(_ context.Context, actor, section string) ([]recipe.Fact, []recipe.VerbatimBlock, error) {
	if actor != "analyst" || section != "environmental" {
		return nil, nil, nil
	}
	return f.facts, f.verbatim, f.err
}
func (f *fakeRecipe) Prepare(context.Context, string, string) (recipe.Prepared, error) {
	return recipe.Prepared{}, errors.New("not used")
}
func (f *fakeRecipe) Sections() []recipe.SectionInfo {
	return []recipe.SectionInfo{{ID: "environmental", Title: "Environmental disclosures", DocumentID: "demo-doc-annual-report", Period: "2025", PriorPeriod: "2024"}}
}
func newService(t *testing.T) (*service, *fakeRecipe) {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log, err := audit.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRecipe{facts: []recipe.Fact{{Label: "Scope 1 energy", Display: []string{"8.1%", "1.24 GWh", "18,400 m3", "-2.5%", "+10"}}}, verbatim: []recipe.VerbatimBlock{{ID: "policy", Text: "Approved policy.\nUnchanged."}}}
	return New(demoenv.Env{DB: db, Audit: log, Cfg: &config.Config{AuthMode: config.AuthModeAPIKey}}, f, f).(*service), f
}
func narrative(text string) []Block { return []Block{{Kind: "narrative", Text: text}} }
func stage(t *testing.T, s *service) Preview {
	t.Helper()
	p, err := s.Stage(context.Background(), "analyst", "environmental", narrative("Energy consumption fell 8.1% to 1.24 GWh in 2025"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestPrepareRequired(t *testing.T) {
	for _, mode := range []string{"none", "error", "actor", "section"} {
		t.Run(mode, func(t *testing.T) {
			s, f := newService(t)
			actor, section := "analyst", "environmental"
			switch mode {
			case "none":
				f.facts = nil
			case "error":
				f.err = errors.New("not prepared")
			case "actor":
				actor = "other"
			case "section":
				section = "other"
			}
			_, err := s.Stage(context.Background(), actor, section, narrative("Text"))
			if err == nil || err.Error() != "Prepare the section first." {
				t.Fatalf("got %v", err)
			}
		})
	}
}
func TestBlockBounds(t *testing.T) {
	s, _ := newService(t)
	for _, blocks := range [][]Block{nil, make([]Block, 21), narrative(strings.Repeat("é", 4001)), {{Kind: "unknown", Text: "text"}}} {
		if _, err := s.Stage(context.Background(), "analyst", "environmental", blocks); err == nil {
			t.Fatal("accepted invalid blocks")
		}
	}
	for _, blocks := range [][]Block{narrative(strings.Repeat("é", 4000)), func() []Block {
		b := make([]Block, 20)
		for i := range b {
			b[i] = Block{Kind: "narrative", Text: "Text"}
		}
		return b
	}()} {
		if _, err := s.Stage(context.Background(), "analyst", "environmental", blocks); err != nil {
			t.Fatal(err)
		}
	}
}
func TestVerbatimGuard(t *testing.T) {
	s, _ := newService(t)
	for _, text := range []string{"Approved policy.\nUnchanged.", "Approved policy.\r\nUnchanged."} {
		if _, err := s.Stage(context.Background(), "analyst", "environmental", []Block{{Kind: "verbatim", VerbatimID: "policy", Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id, text string
		position int
	}{{"policy", "Approved Policy.\nUnchanged.", 10}, {"missing", "text", 1}, {"policy", "Approved policy.\nUnchanged. ", 28}} {
		_, err := s.Stage(context.Background(), "analyst", "environmental", []Block{{Kind: "verbatim", VerbatimID: tc.id, Text: tc.text}})
		var ve *VerbatimError
		if !errors.As(err, &ve) || ve.Position != tc.position || !strings.HasPrefix(err.Error(), "Verbatim block "+tc.id+" cannot be changed. Use the approved text exactly.") {
			t.Fatalf("got %v, want position %d", err, tc.position)
		}
	}
}
func TestAgentNotesBecomeBookmarks(t *testing.T) {
	s, _ := newService(t)
	base, _ := url.Parse("https://api.eu.wdesk.com")
	transport := workiva.NewDemoTransport()
	httpClient := &http.Client{Transport: transport}
	s.env.Client = workiva.NewClient(base, workiva.NewTokenProvider(base, "demo", "demo", "file:read file:write", httpClient), nil, httpClient)
	for _, note := range []string{"NL AGENT: about 9% higher", strings.Repeat("x", 241), "two\nlines"} {
		if _, err := s.Stage(context.Background(), "analyst", "environmental", []Block{{Kind: "narrative", Text: "Scope 1 emissions", Note: note}}); err == nil {
			t.Fatalf("note %q was staged", note)
		}
	}
	if _, err := s.Stage(context.Background(), "analyst", "environmental", []Block{{Kind: "verbatim", VerbatimID: "policy", Text: "Approved policy.\nUnchanged.", Note: "why"}}); err == nil {
		t.Fatal("note on a verbatim block was staged")
	}
	p, err := s.Stage(context.Background(), "analyst", "environmental", []Block{
		{Kind: "narrative", Text: "Energy consumption fell 8.1% to 1.24 GWh in 2025", Note: "NL AGENT: led with energy, the only material change."},
		{Kind: "narrative", Text: "Scope 1 emissions in 2024"},
		{Kind: "verbatim", VerbatimID: "policy", Text: "Approved policy.\nUnchanged."},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
	if err != nil || result.Status != "completed" || result.Notes != "completed" {
		t.Fatalf("confirm: %+v %v", result, err)
	}
	if got := transport.WrittenCells["document:demo-doc-annual-report"]; !strings.Contains(got, `"Bookmarks":["NL AGENT: led with energy, the only material change."]`) {
		t.Fatalf("bookmarks: %s", got)
	}
	var after string
	if err := s.env.DB.QueryRow(`SELECT after_json FROM audit_log`).Scan(&after); err != nil || !strings.Contains(after, `"agent_notes_status":"completed"`) {
		t.Fatalf("audit: %s %v", after, err)
	}
}

func TestNumberGuard(t *testing.T) {
	s, _ := newService(t)
	for _, text := range []string{"Energy consumption fell 8.1% to 1.24 GWh in 2025", "in 2025 compared with 2024", "Scope 1 emissions", "18,400 m3", "CO2 and m3", "Change -2.5% and +10"} {
		if _, err := s.Stage(context.Background(), "analyst", "environmental", narrative(text)); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
	}
	for _, token := range []string{"9%", "8.10%", "18400", "2", "-10", "81%", "18,4000"} {
		_, err := s.Stage(context.Background(), "analyst", "environmental", narrative("fell "+token))
		want := "The number " + token + " is not in the prepared facts for this section."
		if err == nil || err.Error() != want {
			t.Fatalf("got %v want %q", err, want)
		}
	}
}
func TestPreviewPersistence(t *testing.T) {
	s, _ := newService(t)
	before := time.Now()
	p := stage(t, s)
	if p.ExpiresAt.Before(before.Add(10*time.Minute)) || p.ExpiresAt.After(time.Now().Add(10*time.Minute)) {
		t.Fatal("bad expiry")
	}
	var owner, permission, hash, storedToken string
	if err := s.env.DB.QueryRow(`SELECT actor,permission,text_hash,token_digest FROM section_text_previews WHERE id=?`, p.ID).Scan(&owner, &permission, &hash, &storedToken); err != nil {
		t.Fatal(err)
	}
	if owner != "analyst" || permission != "workiva.write.confirm" || hash != digest(fullText(p.Blocks)) || storedToken == p.Token || storedToken != digest(p.Token) {
		t.Fatal("incorrect preview binding")
	}
}

func TestConfirmDemoAndRefusals(t *testing.T) {
	s, f := newService(t)
	f.verbatim[0].Text = "Approved policy."
	base, _ := url.Parse("https://api.eu.wdesk.com")
	docClient := workiva.NewDemoClient(base)
	s.env.Client = workivaprovider.NewRouter(docClient)
	p, err := s.Stage(context.Background(), "analyst", "environmental", []Block{
		{Kind: "narrative", Text: "Energy consumption fell 8.1% to 1.24 GWh in 2025"},
		{Kind: "narrative", Text: "Scope 1 emissions in 2024"},
		{Kind: "verbatim", VerbatimID: "policy", Text: "Approved policy."},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The preview survives reconstructing the service on the same shared handle.
	s = New(s.env, s.recipes, s.facts).(*service)
	denied := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: identity.LegacyTenantID})
	cases := []struct {
		name, actor, id, token, want string
		ctx                          context.Context
		expire                       bool
	}{
		{"unknown", "analyst", "missing", p.Token, "unknown", context.Background(), false},
		{"wrong token", "analyst", p.ID, "wrong", "unknown", context.Background(), false},
		{"foreign", "other", p.ID, p.Token, "another actor", context.Background(), false},
		{"tenant", "analyst", p.ID, p.Token, "another actor", identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: "foreign", Permissions: []identity.Permission{identity.PermissionWorkivaWriteConfirm}}), false},
		{"permission", "analyst", p.ID, p.Token, "requires workiva.write.confirm", denied, false},
		{"expired", "analyst", p.ID, p.Token, "expired", context.Background(), true},
	}
	errorsSeen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.expire {
				s.now = func() time.Time { return p.ExpiresAt }
			} else {
				s.now = time.Now
			}
			_, err := s.Confirm(tc.ctx, tc.actor, tc.id, tc.token)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
			errorsSeen[err.Error()] = true
			sections, err := docClient.ListDocumentSections(context.Background(), "demo-doc-annual-report")
			if err != nil || len(sections) != 0 {
				t.Fatal("refused confirmation wrote a section")
			}
			var count int
			if err := s.env.DB.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unexpected audit writes: %d %v", count, err)
			}
			var used int
			if err := s.env.DB.QueryRow(`SELECT consumed FROM section_text_previews WHERE id=?`, p.ID).Scan(&used); err != nil || used != 0 {
				t.Fatal("refusal consumed preview")
			}
		})
	}
	s.now = time.Now
	result, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
	if err != nil || result.Status != "completed" || result.AuditID == "" || result.WorkivaSectionID == "" {
		t.Fatalf("confirm: %+v %v", result, err)
	}
	writer, _ := workivaprovider.DocumentWriterFor(s.env.Client)
	actual, err := writer.ReadSectionText(context.Background(), result.DocumentID, result.WorkivaSectionID)
	if err != nil || !equalParagraphs(actual, strings.Split(fullText(p.Blocks), "\n")) {
		t.Fatalf("read back: %q %v", actual, err)
	}
	rows, err := s.env.Audit.Recent(context.Background(), 10, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit: %+v %v", rows, err)
	}
	row := rows[0]
	for _, want := range []string{p.SectionID, result.DocumentID, result.WorkivaSectionID, digest(fullText(p.Blocks)), "policy"} {
		if !strings.Contains(row.AfterJSON, want) {
			t.Fatalf("audit missing %q: %+v", want, row)
		}
	}
	if row.Actor != "analyst" || row.AuditID != result.AuditID {
		t.Fatal("audit attribution differs")
	}
	if err := s.env.Audit.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = s.Confirm(context.Background(), "analyst", p.ID, p.Token)
	if err == nil || !strings.Contains(err.Error(), "already used") || errorsSeen[err.Error()] {
		t.Fatalf("reuse error: %v", err)
	}
}

type outcomeWriter struct {
	*workiva.Client
	ensureCalls, writeCalls, readCalls int
	failAt                             string
	actual                             []string
}

func (w *outcomeWriter) EnsureSection(context.Context, string, string) (string, error) {
	w.ensureCalls++
	if w.failAt == "ensure" {
		return "", errors.New("uncertain creation")
	}
	return "section", nil
}
func (w *outcomeWriter) WriteSectionText(context.Context, string, string, []string) (string, error) {
	w.writeCalls++
	if w.failAt == "write" {
		return "/operations/test", errors.New("uncertain edit")
	}
	return "/operations/test", nil
}
func (w *outcomeWriter) ReadSectionText(context.Context, string, string) ([]string, error) {
	w.readCalls++
	if w.failAt == "read" {
		return nil, errors.New("read failed")
	}
	return w.actual, nil
}
func TestConfirmOutcomesConsumeWithoutRetry(t *testing.T) {
	for _, failure := range []string{"ensure", "write", "read", "mismatch"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := newService(t)
			w := &outcomeWriter{failAt: failure, actual: []string{"Different text"}}
			s.env.Client = w
			p := stage(t, s)
			result, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
			want := "reconciliation_required"
			if failure == "mismatch" {
				want = "mismatch"
			}
			if err != nil || result.Status != want {
				t.Fatalf("got %+v %v", result, err)
			}
			_, err = s.Confirm(context.Background(), "analyst", p.ID, p.Token)
			if err == nil || !strings.Contains(err.Error(), "already used") {
				t.Fatalf("retry: %v", err)
			}
			if w.ensureCalls != 1 || w.writeCalls > 1 || w.readCalls > 1 {
				t.Fatal("retried provider mutation")
			}
			rows, err := s.env.Audit.Recent(context.Background(), 10, "")
			if err != nil || len(rows) != 1 || rows[0].Action != want {
				t.Fatalf("audit: %+v %v", rows, err)
			}
		})
	}
}
func TestConcurrentConfirmWritesOnce(t *testing.T) {
	s, _ := newService(t)
	w := &outcomeWriter{actual: []string{"Different text"}}
	s.env.Client = w
	p := stage(t, s)
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token); results <- err }()
	}
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		} else if !strings.Contains(err.Error(), "already used") {
			t.Fatal(err)
		}
	}
	if successes != 1 || w.ensureCalls != 1 || w.writeCalls != 1 {
		t.Fatalf("duplicate confirmation: successes=%d writes=%d", successes, w.writeCalls)
	}
}
func TestStageHashTamperRefused(t *testing.T) {
	s, _ := newService(t)
	w := &outcomeWriter{}
	s.env.Client = w
	p := stage(t, s)
	if _, err := s.env.DB.Exec(`UPDATE section_text_previews SET blocks='[{"Kind":"narrative","Text":"Tampered"}]' WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatal("tampered preview accepted")
	}
	if w.ensureCalls != 0 {
		t.Fatal("tampered preview wrote to provider")
	}
}

func TestRB02EvidenceAndDocumentDecision(t *testing.T) {
	path := "../../docs/demo/evidence/2026-10-19-RB-02/"
	result, err := os.ReadFile(path + "result.md")
	if err != nil || !strings.Contains(string(result), "**Decision: document.**") {
		t.Fatalf("missing document decision: %v", err)
	}
	for _, name := range []string{"step-2.json", "step-3.json", "step-4.json", "step-5.json", "step-6.json"} {
		content, err := os.ReadFile(path + name)
		if err != nil || !json.Valid(content) {
			t.Fatalf("missing request/response evidence %s: %v", name, err)
		}
		for _, secret := range []string{"Bearer ", "access_token", "client_secret"} {
			if strings.Contains(string(content), secret) {
				t.Fatalf("credential marker %q in %s", secret, name)
			}
		}
	}
}
func TestSheetFallbackScenario9(t *testing.T) {
	t.Skip("RB-02 decided document; R8/scenario 9 is conditional and omitted unless a live call fails")
}

func TestMissingPrincipalCannotConfirmOutsideAPIKeyMode(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {AuthMode: config.AuthModeEntra}, {}} {
		t.Run(fmt.Sprint(cfg), func(t *testing.T) {
			s, _ := newService(t)
			w := &outcomeWriter{}
			s.env.Client = w
			p := stage(t, s)
			s.env.Cfg = cfg
			_, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
			if err == nil || err.Error() != "section text confirmation requires workiva.write.confirm" {
				t.Fatalf("missing principal: %v", err)
			}
			if w.ensureCalls != 0 {
				t.Fatal("missing principal wrote a section")
			}
		})
	}
}

type chartWriter struct {
	outcomeWriter
	images [][]byte
}

func (w *chartWriter) WriteSectionText(_ context.Context, _, _ string, paragraphs []string) (string, error) {
	w.actual = paragraphs
	return "/operations/test", nil
}

func (w *chartWriter) AppendSectionImage(_ context.Context, _, _, _ string, png []byte, width, height int) error {
	if width <= 0 || height <= 0 {
		return errors.New("bad size")
	}
	w.images = append(w.images, png)
	return nil
}

func TestChartBlockIsDrawnAfterTheText(t *testing.T) {
	s, f := newService(t)
	f.facts = append(f.facts, recipe.Fact{Key: "business_travel", Label: "6. Business Travel", Unit: "tCO2e", Current: "165", Prior: "140", ChangePct: "17.9", Material: true})
	for _, bad := range [][]Block{
		{{Kind: "chart", Text: "business_travel"}, {Kind: "narrative", Text: "Scope 1 emissions"}},
		{{Kind: "narrative", Text: "Scope 1 emissions"}, {Kind: "chart", Text: "revenue"}},
	} {
		if _, err := s.Stage(context.Background(), "analyst", "environmental", bad); err == nil {
			t.Fatalf("staged %+v", bad)
		}
	}
	w := &chartWriter{}
	s.env.Client = w
	p, err := s.Stage(context.Background(), "analyst", "environmental", []Block{{Kind: "narrative", Text: "Scope 1 emissions"}, {Kind: "chart", Text: "business_travel, scope 1"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token)
	if err != nil || result.Status != "completed" || result.Chart != "completed" || len(w.images) != 1 {
		t.Fatalf("confirm: %+v %v images=%d", result, err, len(w.images))
	}
	if !reflect.DeepEqual(w.actual, []string{"Scope 1 emissions"}) {
		t.Fatalf("chart keys were written as text: %q", w.actual)
	}
}

func TestChartRowFormatsPreparedFigures(t *testing.T) {
	r := chartRow(recipe.Fact{Label: "Energy", Unit: "MWh", Current: "1978", Prior: "-2030.5", ChangePct: "-2.6"})
	if r.CurrentText != "1,978 MWh" || r.PriorText != "-2,030.5 MWh" || r.Tag != "" {
		t.Fatalf("%+v", r)
	}
	if r := chartRow(recipe.Fact{Label: "Water", Unit: "m3", Prior: "2300"}); r.CurrentText != "missing" || r.Current != 0 {
		t.Fatalf("%+v", r)
	}
}
