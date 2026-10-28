package workiva

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

// DocumentSection is the section metadata returned by the documents endpoint.
type DocumentSection struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Parent *struct {
		ID string `json:"id"`
	} `json:"parent,omitempty"`
	Body struct {
		RichText string `json:"richText"`
		Revision string `json:"revision"`
	} `json:"body"`
}

func documentSectionsPath(documentID string) string {
	return "/documents/" + url.PathEscape(documentID) + "/sections"
}

// documentJSON only follows links on this client's API origin. This applies to
// pagination and operation locations as well as direct document requests.
func (c *Client) documentJSON(ctx context.Context, method, path string, payload, out any) (http.Header, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	target := c.baseURL.ResolveReference(ref)
	if target.Scheme != c.baseURL.Scheme || target.Host != c.baseURL.Host || target.User != nil {
		return nil, errors.New("document endpoint is outside the Workiva API origin")
	}
	var body *bytes.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	category := ratelimit.CategoryReads
	if method != http.MethodGet {
		category = ratelimit.CategoryWrites
	}
	var resp *http.Response
	if body != nil {
		resp, err = c.Do(ctx, method, path, body, category)
	} else {
		resp, err = c.Do(ctx, method, path, nil, category)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err = json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.Header, err
		}
	}
	return resp.Header, nil
}

// ListDocumentSections follows the exact RB-02 section listing and pagination.
func (c *Client) ListDocumentSections(ctx context.Context, documentID string) ([]DocumentSection, error) {
	path := documentSectionsPath(documentID) + "?$maxpagesize=100"
	seen := map[string]bool{}
	sections := []DocumentSection{}
	for path != "" {
		if seen[path] {
			return nil, errors.New("repeated document sections pagination link")
		}
		seen[path] = true
		var page struct {
			Data []DocumentSection `json:"data"`
			Next string            `json:"@nextLink"`
		}
		if _, err := c.documentJSON(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		sections = append(sections, page.Data...)
		path = page.Next
	}
	return sections, nil
}

// EnsureSection matches the exact section name before creating synchronously.
func (c *Client) EnsureSection(ctx context.Context, documentID, title string) (string, error) {
	sections, err := c.ListDocumentSections(ctx, documentID)
	if err != nil {
		return "", err
	}
	for _, s := range sections {
		if s.Name == title {
			return s.ID, nil
		}
	}
	return c.CreateDocumentSection(ctx, documentID, title, "")
}

// CreateDocumentSection supports a parent for opt-in sandbox nesting tests.
func (c *Client) CreateDocumentSection(ctx context.Context, documentID, title, parentID string) (string, error) {
	payload := map[string]any{"name": title}
	if parentID != "" {
		payload["parent"] = map[string]string{"id": parentID}
	}
	var section DocumentSection
	_, err := c.documentJSON(ctx, http.MethodPost, documentSectionsPath(documentID), payload, &section)
	if err != nil {
		return "", err
	}
	if section.ID == "" {
		return "", errors.New("create document section returned no ID")
	}
	return section.ID, nil
}

// DeleteDocumentSection is used only by live test cleanup.
func (c *Client) DeleteDocumentSection(ctx context.Context, documentID, sectionID string) error {
	_, err := c.documentJSON(ctx, http.MethodDelete, documentSectionsPath(documentID)+"/"+url.PathEscape(sectionID), nil, nil)
	return err
}

func (c *Client) documentSection(ctx context.Context, documentID, sectionID string) (DocumentSection, error) {
	var section DocumentSection
	_, err := c.documentJSON(ctx, http.MethodGet, documentSectionsPath(documentID)+"/"+url.PathEscape(sectionID), nil, &section)
	if err == nil && (section.Body.RichText == "" || section.Body.Revision == "") {
		err = errors.New("document section has no body rich text or revision")
	}
	return section, err
}

type richParagraph struct {
	Index    int `json:"index"`
	Elements []struct {
		Type     string `json:"type"`
		TextSpan struct {
			Text string `json:"text"`
		} `json:"textSpan"`
	} `json:"elements"`
}

func (c *Client) richTextParagraphs(ctx context.Context, richTextID string) ([]string, error) {
	path := "/content/richText/" + url.PathEscape(richTextID) + "/paragraphs"
	seen := map[string]bool{}
	all := []richParagraph{}
	for path != "" {
		if seen[path] {
			return nil, errors.New("repeated rich text pagination link")
		}
		seen[path] = true
		var page struct {
			Data []richParagraph `json:"data"`
			Next string          `json:"@nextLink"`
		}
		if _, err := c.documentJSON(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Data...)
		path = page.Next
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Index < all[j].Index })
	paragraphs := make([]string, len(all))
	for i, p := range all {
		if p.Index != i {
			return nil, errors.New("rich text paragraphs have missing or duplicate indexes")
		}
		for _, e := range p.Elements {
			if e.Type == "image" {
				// An inserted chart; U+FFFC keeps its length of one for delete offsets.
				paragraphs[i] += "\uFFFC"
				continue
			}
			if e.Type != "textSpan" {
				return nil, fmt.Errorf("section contains unsupported rich text element %q", e.Type)
			}
			paragraphs[i] += e.TextSpan.Text
		}
	}
	return paragraphs, nil
}

type textCaret struct {
	ParagraphIndex int `json:"paragraphIndex"`
	Offset         int `json:"offset"`
}

// WriteSectionText uses body.richText and body.revision, then sends a single
// rich-text edit with deleteText before insertText when content already exists.
// No uncertain mutation is retried here. Client.Do only retries explicit 401/429
// rejections for POST requests, never transport failures or 5xx responses.
func (c *Client) WriteSectionText(ctx context.Context, documentID, sectionID string, paragraphs []string) (string, error) {
	if len(paragraphs) == 0 {
		return "", errors.New("section text requires at least one paragraph")
	}
	section, err := c.documentSection(ctx, documentID, sectionID)
	if err != nil {
		return "", err
	}
	old, err := c.richTextParagraphs(ctx, section.Body.RichText)
	if err != nil {
		return "", err
	}
	edits := []any{}
	if len(old) > 1 || (len(old) == 1 && old[0] != "") {
		edits = append(edits, map[string]any{"type": "deleteText", "deleteText": map[string]any{"selection": map[string]textCaret{"start": {}, "stop": {ParagraphIndex: len(old) - 1, Offset: utf8.RuneCountInString(old[len(old)-1])}}}})
	}
	edits = append(edits, map[string]any{"type": "insertText", "insertText": map[string]any{"insertAt": textCaret{}, "text": strings.Join(paragraphs, "\n")}})
	payload := map[string]any{"revision": section.Body.Revision, "isolateEdits": false, "data": edits}
	var response struct {
		OperationLocation string `json:"operationLocation"`
	}
	headers, err := c.documentJSON(ctx, http.MethodPost, "/content/richText/"+url.PathEscape(section.Body.RichText)+"/edit", payload, &response)
	if err != nil {
		return "", err
	}
	op := headers.Get("Location")
	if op == "" {
		op = response.OperationLocation
	}
	if op == "" {
		return "", errors.New("rich text edit returned no operation location")
	}
	ref, err := url.Parse(op)
	if err != nil {
		return op, err
	}
	target := c.baseURL.ResolveReference(ref)
	if target.Scheme != c.baseURL.Scheme || target.Host != c.baseURL.Host || target.User != nil {
		return op, errors.New("rich text operation is outside the Workiva API origin")
	}
	_, err = c.WaitOperationWithInitialRetryAfter(ctx, op, initialRetryAfterDelay(headers.Get("Retry-After")))
	return op, err
}

// Bookmark tags one whole paragraph of a section with a name. Workiva shows
// bookmarks in its bookmarks panel; they are not part of the printed text.
type Bookmark struct {
	ParagraphIndex, Length int
	Name                   string
}

// AddSectionBookmarks inserts the bookmarks in one atomic batch and waits for
// the operation, like WriteSectionText.
func (c *Client) AddSectionBookmarks(ctx context.Context, documentID, sectionID string, marks []Bookmark) error {
	if len(marks) == 0 {
		return nil
	}
	section, err := c.documentSection(ctx, documentID, sectionID)
	if err != nil {
		return err
	}
	edits := make([]any, len(marks))
	for i, m := range marks {
		selection := map[string]textCaret{"start": {ParagraphIndex: m.ParagraphIndex}, "stop": {ParagraphIndex: m.ParagraphIndex, Offset: m.Length}}
		location := map[string]any{"type": "richTextSelection", "richTextSelection": map[string]any{"id": section.Body.RichText, "selection": selection}}
		edits[i] = map[string]any{"type": "insertBookmark", "insertBookmark": map[string]any{"location": location, "name": m.Name}}
	}
	var response struct {
		OperationLocation string `json:"operationLocation"`
	}
	headers, err := c.documentJSON(ctx, http.MethodPost, "/documents/"+url.PathEscape(documentID)+"/bookmarks/edit", map[string]any{"data": edits}, &response)
	if err != nil {
		return err
	}
	op := headers.Get("Location")
	if op == "" {
		op = response.OperationLocation
	}
	if op == "" {
		return errors.New("bookmark edit returned no operation location")
	}
	ref, err := url.Parse(op)
	if err != nil {
		return err
	}
	if target := c.baseURL.ResolveReference(ref); target.Scheme != c.baseURL.Scheme || target.Host != c.baseURL.Host || target.User != nil {
		return errors.New("bookmark operation is outside the Workiva API origin")
	}
	_, err = c.WaitOperationWithInitialRetryAfter(ctx, op, initialRetryAfterDelay(headers.Get("Retry-After")))
	return err
}

// AppendSectionImage uploads a PNG and inserts it as a new last paragraph of
// the section. Workiva scans uploads before they can be used, so this waits for
// both the upload and the edit operations.
func (c *Client) AppendSectionImage(ctx context.Context, documentID, sectionID, fileName string, png []byte, width, height int) error {
	var upload struct {
		UploadURL string `json:"uploadUrl"`
	}
	headers, err := c.documentJSON(ctx, http.MethodPost, "/content/images/upload", map[string]string{"fileName": fileName}, &upload)
	if err != nil {
		return fmt.Errorf("start image upload: %w", err)
	}
	op := headers.Get("Location")
	if upload.UploadURL == "" || op == "" {
		return errors.New("image upload returned no upload URL or operation")
	}
	if err = c.putUpload(ctx, upload.UploadURL, png); err != nil {
		return fmt.Errorf("upload image bytes: %w", err)
	}
	resource, err := c.WaitOperationWithInitialRetryAfter(ctx, op, initialRetryAfterDelay(headers.Get("Retry-After")))
	if err != nil {
		return err
	}
	var results struct {
		Data []struct {
			Image string `json:"image"`
		} `json:"data"`
	}
	if _, err = c.documentJSON(ctx, http.MethodGet, resource, nil, &results); err != nil {
		return err
	}
	if len(results.Data) != 1 || results.Data[0].Image == "" {
		return errors.New("image upload returned no image")
	}
	section, err := c.documentSection(ctx, documentID, sectionID)
	if err != nil {
		return err
	}
	old, err := c.richTextParagraphs(ctx, section.Body.RichText)
	if err != nil {
		return err
	}
	last := len(old) - 1
	edits := []any{
		map[string]any{"type": "insertText", "insertText": map[string]any{"insertAt": textCaret{ParagraphIndex: last, Offset: utf8.RuneCountInString(old[last])}, "text": "\n"}},
		map[string]any{"type": "insertImage", "insertImage": map[string]any{"image": results.Data[0].Image, "dimensions": map[string]int{"width": width, "height": height}, "insertAt": textCaret{ParagraphIndex: last + 1}}},
	}
	var response struct {
		OperationLocation string `json:"operationLocation"`
	}
	headers, err = c.documentJSON(ctx, http.MethodPost, "/content/richText/"+url.PathEscape(section.Body.RichText)+"/edit", map[string]any{"revision": section.Body.Revision, "data": edits}, &response)
	if err != nil {
		return err
	}
	if op = headers.Get("Location"); op == "" {
		op = response.OperationLocation
	}
	if op == "" {
		return errors.New("image insert returned no operation location")
	}
	_, err = c.WaitOperationWithInitialRetryAfter(ctx, op, initialRetryAfterDelay(headers.Get("Retry-After")))
	return err
}

// putUpload sends the file bytes to the upload URL Workiva returned. The bearer
// token is sent only over HTTPS to the API host or another wdesk.com host
// (uploads go to Workiva blob storage, e.g. h.eu.wdesk.com).
func (c *Client) putUpload(ctx context.Context, rawURL string, data []byte) error {
	target, err := c.baseURL.Parse(rawURL)
	if err != nil || target.Scheme != "https" && target.Scheme != c.baseURL.Scheme {
		return errors.New("image upload URL is not valid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("X-Version", c.apiVersion)
	req.Header.Set("Content-Type", "application/octet-stream")
	if target.Host == c.baseURL.Host || target.Scheme == "https" && strings.HasSuffix(target.Hostname(), ".wdesk.com") {
		token, err := c.tokens.ClientCredentialsToken(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return &APIError{StatusCode: resp.StatusCode}
	}
	return nil
}

// ReadSectionText concatenates textSpan.text for each indexed body paragraph.
func (c *Client) ReadSectionText(ctx context.Context, documentID, sectionID string) ([]string, error) {
	section, err := c.documentSection(ctx, documentID, sectionID)
	if err != nil {
		return nil, err
	}
	return c.richTextParagraphs(ctx, section.Body.RichText)
}
