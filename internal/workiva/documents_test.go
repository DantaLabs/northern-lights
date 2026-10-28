package workiva

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func documentTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	return NewClient(base, NewTokenProvider(base, "test", "test", "file:read file:write", srv.Client()), nil, srv.Client())
}
func documentToken(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/iam/v1/oauth2/token" {
		_, _ = w.Write([]byte(`{"access_token":"test","expires_in":3600}`))
		return true
	}
	return false
}
func TestDocumentWriterDemo(t *testing.T) {
	base, _ := url.Parse("https://api.eu.wdesk.com")
	c := NewDemoClient(base)
	ctx := context.Background()
	first, err := c.EnsureSection(ctx, "demo-doc-annual-report", "Environmental disclosures")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.EnsureSection(ctx, "demo-doc-annual-report", "Environmental disclosures")
	if err != nil || first != second {
		t.Fatalf("duplicate section: %q %q %v", first, second, err)
	}
	all, err := c.ListDocumentSections(ctx, "demo-doc-annual-report")
	if err != nil || len(all) != 1 {
		t.Fatalf("sections: %+v %v", all, err)
	}
	for _, paragraphs := range [][]string{{"First paragraph", "Second paragraph", "Approved policy."}, {"Replacement é", "Second replacement 😀"}, {""}} {
		if _, err := c.WriteSectionText(ctx, "demo-doc-annual-report", first, paragraphs); err != nil {
			t.Fatal(err)
		}
		actual, err := c.ReadSectionText(ctx, "demo-doc-annual-report", first)
		if err != nil || !reflect.DeepEqual(actual, paragraphs) {
			t.Fatalf("got %q want %q err %v", actual, paragraphs, err)
		}
	}
	// A second demo transport cannot see this one's sections.
	other := NewDemoClient(base)
	all, err = other.ListDocumentSections(ctx, "demo-doc-annual-report")
	if err != nil || len(all) != 0 {
		t.Fatalf("demo transports share state: %+v %v", all, err)
	}
}
func TestDocumentWriterRB02Requests(t *testing.T) {
	var creates, edits int
	c := documentTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if documentToken(w, r) {
			return
		}
		if r.Header.Get("X-Version") != "2026-01-01" {
			t.Error("wrong API version")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/documents/report/sections" && r.URL.Query().Get("page") == "2":
			_, _ = w.Write([]byte(`{"data":[{"id":"existing","name":"Exact title"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/documents/report/sections":
			if r.URL.Query().Get("$maxpagesize") != "100" {
				t.Error("wrong page size")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"not-exact","name":"exact title"}],"@nextLink":"/documents/report/sections?page=2"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/documents/report/sections":
			creates++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if !reflect.DeepEqual(body, map[string]any{"name": "New title"}) {
				t.Errorf("create body: %+v", body)
			}
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"id":"new","name":"New title"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/documents/report/sections/existing":
			_, _ = w.Write([]byte(`{"id":"existing","body":{"richText":"body-rich","revision":"base-revision"},"headers":{"odd":{"richText":"wrong-rich"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/content/richText/body-rich/paragraphs" && r.URL.Query().Get("page") == "2":
			_, _ = w.Write([]byte(`{"data":[{"index":1,"elements":[{"type":"textSpan","textSpan":{"text":"Last é😀"}}]}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/content/richText/body-rich/paragraphs":
			_, _ = w.Write([]byte(`{"data":[{"index":0,"elements":[{"type":"textSpan","textSpan":{"text":"Old "}},{"type":"textSpan","textSpan":{"text":"text"}}]}],"@nextLink":"/content/richText/body-rich/paragraphs?page=2"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/content/richText/body-rich/edit":
			edits++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			want := `{"revision":"base-revision","isolateEdits":false,"data":[{"type":"deleteText","deleteText":{"selection":{"start":{"paragraphIndex":0,"offset":0},"stop":{"paragraphIndex":1,"offset":7}}}},{"type":"insertText","insertText":{"insertAt":{"paragraphIndex":0,"offset":0},"text":"New one\nNew two"}}]}`
			var expected map[string]any
			_ = json.Unmarshal([]byte(want), &expected)
			if !reflect.DeepEqual(body, expected) {
				t.Errorf("edit body: %+v, want %+v", body, expected)
			}
			w.Header().Set("Location", "/operations/document-test")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"operationLocation":"/operations/document-test"}`))
		case r.URL.Path == "/operations/document-test":
			_, _ = w.Write([]byte(`{"status":"completed","resourceUrl":"/operations/document-test/richTextBatchEditResults"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	id, err := c.EnsureSection(ctx, "report", "Exact title")
	if err != nil || id != "existing" {
		t.Fatalf("find: %q %v", id, err)
	}
	id, err = c.EnsureSection(ctx, "report", "New title")
	if err != nil || id != "new" || creates != 1 {
		t.Fatalf("create: %q %v", id, err)
	}
	actual, err := c.ReadSectionText(ctx, "report", "existing")
	if err != nil || !reflect.DeepEqual(actual, []string{"Old text", "Last é😀"}) {
		t.Fatalf("read: %q %v", actual, err)
	}
	if _, err := c.WriteSectionText(ctx, "report", "existing", []string{"New one", "New two"}); err != nil || edits != 1 {
		t.Fatalf("write: %v edits %d", err, edits)
	}
}
func TestDocumentWriteNoRetryOnUncertainOutcome(t *testing.T) {
	var writes atomic.Int32
	c := documentTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if documentToken(w, r) {
			return
		}
		switch r.URL.Path {
		case "/documents/report/sections/section":
			_, _ = w.Write([]byte(`{"body":{"richText":"body","revision":"r"}}`))
		case "/content/richText/body/paragraphs":
			_, _ = w.Write([]byte(`{"data":[{"index":0,"elements":[]}]}`))
		case "/content/richText/body/edit":
			writes.Add(1)
			http.Error(w, "uncertain", 500)
		default:
			http.NotFound(w, r)
		}
	})
	if _, err := c.WriteSectionText(context.Background(), "report", "section", []string{"Text"}); err == nil || writes.Load() != 1 {
		t.Fatalf("uncertain write: %v calls %d", err, writes.Load())
	}
}
func TestDocumentRejectsUnsafeOrRepeatedPagination(t *testing.T) {
	for _, link := range []string{"https://outside.example/sections", "/documents/report/sections?$maxpagesize=100"} {
		t.Run(fmt.Sprint(strings.Contains(link, "outside")), func(t *testing.T) {
			c := documentTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if documentToken(w, r) {
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "@nextLink": link})
			})
			if _, err := c.ListDocumentSections(context.Background(), "report"); err == nil {
				t.Fatal("unsafe pagination accepted")
			}
		})
	}
}

func TestEnsureChildSectionNestsUnderTheChapter(t *testing.T) {
	base, _ := url.Parse("https://api.eu.wdesk.com")
	c := NewDemoClient(base)
	ctx := context.Background()
	const doc = "demo-doc-annual-report"
	chapter, err := c.CreateDocumentSection(ctx, doc, "2. Environmental information", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnsureSection(ctx, doc, "2.3 Environmental performance"); err != nil { // a stray top-level twin must be ignored
		t.Fatal(err)
	}
	first, err := c.EnsureChildSection(ctx, doc, "2. Environmental information", "2.3 Environmental performance")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.EnsureChildSection(ctx, doc, "2. Environmental information", "2.3 Environmental performance")
	if err != nil || first != second {
		t.Fatalf("duplicate child: %q %q %v", first, second, err)
	}
	all, _ := c.ListDocumentSections(ctx, doc)
	for _, s := range all {
		if s.ID == first && (s.Parent == nil || s.Parent.ID != chapter) {
			t.Fatalf("section not under chapter: %+v", s)
		}
	}
}
