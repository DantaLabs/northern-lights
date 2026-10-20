package workiva

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

type demoDocument struct {
	Next     int
	Sections []demoDocumentSection
}
type demoDocumentSection struct {
	ID, Name, Parent string
	Revision         int
	Paragraphs       []string
}

// demoDocumentsResponse stores document state under a reserved WrittenCells key
// on this transport, using its existing mutex. This keeps instances isolated
// without changing the shared DemoTransport scaffold owned by L0.
func (t *DemoTransport) demoDocumentsResponse(req *http.Request) *http.Response {
	t.mu.Lock()
	defer t.mu.Unlock()
	const documentID = "demo-doc-annual-report"
	const stateKey = "document:" + documentID
	if t.WrittenCells == nil {
		t.WrittenCells = make(map[string]string)
	}
	state := demoDocument{Sections: []demoDocumentSection{}}
	if raw := t.WrittenCells[stateKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return demoServerError("decode demo document")
		}
	}
	save := func() { t.WrittenCells[stateKey] = string(mustJSON(state)) }
	sectionJSON := func(s demoDocumentSection) map[string]any {
		var parent any
		if s.Parent != "" {
			parent = map[string]string{"id": s.Parent}
		}
		return map[string]any{"id": s.ID, "name": s.Name, "parent": parent, "children": []any{}, "index": 0, "body": map[string]string{"richText": "body-" + s.ID, "revision": fmt.Sprint(s.Revision)}}
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if req.Method == http.MethodGet && req.URL.Path == "/documents" {
		return demoJSONResponse(mustJSON(map[string]any{"data": []any{map[string]string{"id": documentID, "name": "Annual report"}}}))
	}
	if len(parts) >= 3 && parts[0] == "documents" && parts[1] == documentID && parts[2] == "sections" {
		if len(parts) == 3 {
			switch req.Method {
			case http.MethodGet:
				data := []any{}
				for _, s := range state.Sections {
					data = append(data, sectionJSON(s))
				}
				return demoJSONResponse(mustJSON(map[string]any{"data": data}))
			case http.MethodPost:
				var input struct {
					Name   string `json:"name"`
					Parent *struct {
						ID string `json:"id"`
					} `json:"parent"`
				}
				if err := json.NewDecoder(req.Body).Decode(&input); err != nil || input.Name == "" {
					return demoBadRequest("section name is required")
				}
				parent := ""
				if input.Parent != nil {
					parent = input.Parent.ID
					found := false
					for _, s := range state.Sections {
						found = found || s.ID == parent
					}
					if !found {
						return demoBadRequest("parent not found")
					}
				}
				state.Next++
				s := demoDocumentSection{ID: fmt.Sprintf("%s_%d", documentID, state.Next), Name: input.Name, Parent: parent, Revision: 1, Paragraphs: []string{""}}
				state.Sections = append(state.Sections, s)
				save()
				resp := demoJSONResponse(mustJSON(sectionJSON(s)))
				resp.StatusCode = http.StatusCreated
				return resp
			}
		}
		if len(parts) == 4 {
			for i, s := range state.Sections {
				if s.ID != parts[3] {
					continue
				}
				switch req.Method {
				case http.MethodGet:
					return demoJSONResponse(mustJSON(sectionJSON(s)))
				case http.MethodDelete:
					state.Sections = append(state.Sections[:i], state.Sections[i+1:]...)
					save()
					resp := demoJSONResponse(nil)
					resp.StatusCode = http.StatusNoContent
					return resp
				}
			}
		}
	}
	if len(parts) == 4 && parts[0] == "content" && parts[1] == "richText" {
		for i, s := range state.Sections {
			if "body-"+s.ID != parts[2] {
				continue
			}
			if parts[3] == "paragraphs" && req.Method == http.MethodGet {
				data := []any{}
				for index, text := range s.Paragraphs {
					data = append(data, map[string]any{"index": index, "elements": []any{map[string]any{"type": "textSpan", "textSpan": map[string]any{"text": text, "length": utf8.RuneCountInString(text)}}}})
				}
				return demoJSONResponse(mustJSON(map[string]any{"data": data}))
			}
			if parts[3] == "edit" && req.Method == http.MethodPost {
				var input struct {
					Revision     string `json:"revision"`
					IsolateEdits bool   `json:"isolateEdits"`
					Data         []struct {
						Type       string `json:"type"`
						DeleteText struct {
							Selection struct{ Start, Stop textCaret } `json:"selection"`
						} `json:"deleteText"`
						InsertText struct {
							InsertAt textCaret `json:"insertAt"`
							Text     string    `json:"text"`
						} `json:"insertText"`
					} `json:"data"`
				}
				if err := json.NewDecoder(req.Body).Decode(&input); err != nil || input.Revision != fmt.Sprint(s.Revision) || input.IsolateEdits || len(input.Data) == 0 {
					return demoBadRequest("invalid rich text edit revision or batch")
				}
				for _, edit := range input.Data {
					switch edit.Type {
					case "deleteText":
						sel := edit.DeleteText.Selection
						if sel.Start != (textCaret{}) || sel.Stop != (textCaret{ParagraphIndex: len(s.Paragraphs) - 1, Offset: utf8.RuneCountInString(s.Paragraphs[len(s.Paragraphs)-1])}) {
							return demoBadRequest("demo delete must select all existing text")
						}
						s.Paragraphs = []string{""}
					case "insertText":
						if edit.InsertText.InsertAt != (textCaret{}) {
							return demoBadRequest("demo insert must start at the beginning")
						}
						s.Paragraphs = strings.Split(edit.InsertText.Text+strings.Join(s.Paragraphs, "\n"), "\n")
					default:
						return demoBadRequest("unsupported rich text edit")
					}
				}
				s.Revision++
				state.Sections[i] = s
				save()
				resp := demoJSONResponse([]byte(`{"operationLocation":"/operations/demo-document-edit"}`))
				resp.StatusCode = http.StatusAccepted
				resp.Header.Set("Location", "/operations/demo-document-edit")
				return resp
			}
		}
	}
	return demoLaneHookNotFound("demo report document resource not found: " + req.URL.Path)
}

func demoLaneHookNotFound(message string) *http.Response {
	resp := demoJSONResponse(mustJSON(map[string]string{"error": message}))
	resp.StatusCode = http.StatusNotFound
	return resp
}
