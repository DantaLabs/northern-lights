package sectiontext

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
	"github.com/dantalabs/northern-lights/internal/recipe"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

type liveRecipe struct {
	*fakeRecipe
	info recipe.SectionInfo
}

func (r liveRecipe) Sections() []recipe.SectionInfo { return []recipe.SectionInfo{r.info} }

// TestLiveSandboxSectionText is acceptance scenario 10. All mutations are
// confined to sections created here whose titles begin "NL test ". The parent
// is read only. Credentials are supplied by the test runner, never logged.
func TestLiveSandboxSectionText(t *testing.T) {
	if os.Getenv("NL_LIVE_SANDBOX") != "1" {
		t.Skip("set NL_LIVE_SANDBOX=1 for sandbox section text writes")
	}
	documentID := os.Getenv("NL_LIVE_DOCUMENT_ID")
	if documentID == "" {
		t.Fatal("NL_LIVE_DOCUMENT_ID must name the sandbox report document")
	}
	baseURL := map[string]string{"eu": "https://api.eu.wdesk.com", "us": "https://api.app.wdesk.com", "apac": "https://api.apac.wdesk.com"}[os.Getenv("NL_REGION")]
	if baseURL == "" {
		t.Fatal("NL_REGION must be eu, us or apac")
	}
	base, _ := url.Parse(baseURL)
	httpClient := &http.Client{Timeout: 30 * time.Second}
	tokens := workiva.NewTokenProvider(base, os.Getenv("NL_WORKIVA_CLIENT_ID"), os.Getenv("NL_WORKIVA_CLIENT_SECRET"), "file:read file:write task:read task:write", httpClient)
	client := workiva.NewClient(base, tokens, ratelimit.NewLimiter(), httpClient)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, f := newService(t)
	s.env.Client = client
	title := "NL test " + time.Now().UTC().Format("20060102T150405.000000000Z")
	sections, err := client.ListDocumentSections(ctx, documentID)
	if err != nil {
		t.Fatal("live section listing failed")
	}
	parent := ""
	for _, section := range sections {
		if section.Name == "2. Environmental information" {
			parent = section.ID
			break
		}
	}
	if parent == "" {
		t.Fatal("live nesting parent 2. Environmental information was not found")
	}
	nestedTitle := "NL test " + time.Now().UTC().Format("20060102T150405.000000000Z")
	nestedID, err := client.CreateDocumentSection(ctx, documentID, nestedTitle, parent)
	if err != nil {
		t.Fatal("live nested section creation failed")
	}
	cleanup := func(sectionID, createdTitle string) {
		t.Helper()
		if !strings.HasPrefix(createdTitle, "NL test ") {
			t.Fatal("refuse cleanup outside NL test sections")
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := client.DeleteDocumentSection(cleanupCtx, documentID, sectionID); err != nil {
			t.Error("live NL test section cleanup failed")
		}
	}
	defer cleanup(nestedID, nestedTitle)
	sections, err = client.ListDocumentSections(ctx, documentID)
	if err != nil {
		t.Fatal("live nested section listing failed")
	}
	found := false
	for _, section := range sections {
		if section.ID == nestedID {
			found = section.Name == nestedTitle && section.Parent != nil && section.Parent.ID == parent
		}
	}
	if !found {
		t.Fatal("created test section is not nested under the requested parent")
	}
	t.Log("PASS nested section under 2. Environmental information")
	// Pre-create the top-level section to register cleanup before confirmation.
	// Confirm's EnsureSection must reuse it. The demo test covers creation there.
	sectionID, err := client.EnsureSection(ctx, documentID, title)
	if err != nil {
		t.Fatal("live NL test section creation failed")
	}
	defer cleanup(sectionID, title)
	s.recipes = liveRecipe{f, recipe.SectionInfo{ID: "environmental", Title: title, DocumentID: documentID, Period: "2025", PriorPeriod: "2024"}}
	paragraphs := []string{"Energy consumption fell 8.1% to 1.24 GWh in 2025.", "Scope 1 emissions in 2024."}
	preview, err := s.Stage(ctx, "analyst", "environmental", []Block{{Kind: "narrative", Text: paragraphs[0], Note: "NL AGENT: live bookmark check."}, {Kind: "narrative", Text: paragraphs[1]}, {Kind: "chart", Text: "scope 1 energy"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Confirm(ctx, "analyst", preview.ID, preview.Token)
	if err != nil || result.Status != "completed" || result.WorkivaSectionID != sectionID {
		t.Fatalf("live confirmation failed: status %q", result.Status)
	}
	if result.Notes != "completed" {
		t.Fatalf("live NL AGENT bookmark failed: %q", result.Notes)
	}
	if result.Chart != "completed" {
		t.Fatalf("live chart insert failed: %q", result.Chart)
	}
	if reviewer := os.Getenv("NL_WORKIVA_REVIEWER_ID"); reviewer != "" {
		task, err := client.CreateReviewTask(ctx, documentID, sectionID, reviewer, "NL test review", "NL AGENT: live review task check.", time.Now().Add(72*time.Hour))
		if err != nil || task.ID == "" {
			t.Fatal("live review task creation failed")
		}
		defer func() {
			if err := client.DeleteTask(context.Background(), task.ID); err != nil {
				t.Error("live review task cleanup failed")
			}
		}()
		tasks, err := client.SectionTasks(ctx, documentID, sectionID)
		if err != nil || len(tasks) == 0 || tasks[0].ID != task.ID {
			t.Fatal("live review task not listed on the section")
		}
		t.Log("PASS review task created and listed on the section")
	}
	actual, err := client.ReadSectionText(ctx, documentID, sectionID)
	if err != nil || !reflect.DeepEqual(actual, append(paragraphs, "\uFFFC")) {
		t.Fatal("live initial paragraphs differ")
	}
	t.Log("PASS create, confirm and exact two-paragraph read-back")
	replacement := []string{"Replacement text with accented é and emoji 😀.", "This replaces the previous content é 😀."}
	if _, err := client.WriteSectionText(ctx, documentID, sectionID, replacement); err != nil {
		t.Fatal("live replacement deleteText/insertText batch failed")
	}
	actual, err = client.ReadSectionText(ctx, documentID, sectionID)
	if err != nil || !reflect.DeepEqual(actual, replacement) {
		t.Fatal("live replacement paragraphs differ")
	}
	// Replace again so selection offsets must handle the non-ASCII last paragraph.
	replacement = []string{"Second replacement.", "Final paragraph é 😀."}
	if _, err := client.WriteSectionText(ctx, documentID, sectionID, replacement); err != nil {
		t.Fatal("live second replacement batch failed")
	}
	actual, err = client.ReadSectionText(ctx, documentID, sectionID)
	if err != nil || !reflect.DeepEqual(actual, replacement) {
		t.Fatal("live second replacement differs")
	}
	t.Log("PASS replacement of existing content with deleteText then insertText in one batch")
}
