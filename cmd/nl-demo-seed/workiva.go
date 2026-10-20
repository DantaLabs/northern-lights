package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// newWorkiva builds the live client from the environment. It is a seam:
// tests replace it with a fake, and a dry run never calls it.
var newWorkiva = func(context.Context) (Workiva, error) {
	return newLiveWorkivaFromEnv(os.Getenv)
}

// regionBaseURLs mirrors baseURLs in internal/config/config.go.
var regionBaseURLs = map[string]string{
	"eu":   "https://api.eu.wdesk.com",
	"us":   "https://api.app.wdesk.com",
	"apac": "https://api.apac.wdesk.com",
}

func regionBaseURL(region string) (*url.URL, error) {
	if region == "" {
		region = "eu"
	}
	raw, ok := regionBaseURLs[region]
	if !ok {
		return nil, fmt.Errorf("NL_REGION %q is not one of eu, us, apac", region)
	}
	return url.Parse(raw)
}

// newLiveWorkivaFromEnv reads NL_WORKIVA_CLIENT_ID, NL_WORKIVA_CLIENT_SECRET
// and NL_REGION (default eu) through getenv. Errors name a variable, never
// its value.
func newLiveWorkivaFromEnv(getenv func(string) string) (*liveWorkiva, error) {
	id := getenv("NL_WORKIVA_CLIENT_ID")
	if id == "" {
		return nil, errors.New("NL_WORKIVA_CLIENT_ID is not set")
	}
	secret := getenv("NL_WORKIVA_CLIENT_SECRET")
	if secret == "" {
		return nil, errors.New("NL_WORKIVA_CLIENT_SECRET is not set")
	}
	base, err := regionBaseURL(getenv("NL_REGION"))
	if err != nil {
		return nil, err
	}
	return newLiveWorkivaAt(base, id, secret, &http.Client{Timeout: 60 * time.Second}), nil
}

// newLiveWorkivaAt builds the client the way cmd/workiva-mcp buildServer does.
func newLiveWorkivaAt(base *url.URL, id, secret string, hc *http.Client) *liveWorkiva {
	tokens := workiva.NewTokenProvider(base, id, secret, "file:read file:write", hc)
	return &liveWorkiva{Client: workiva.NewClient(base, tokens, ratelimit.NewLimiter(), hc), base: base}
}

// liveWorkiva adds the section calls the seed needs to *workiva.Client. The
// report document writer in internal/workiva has no delete, so they live here.
type liveWorkiva struct {
	*workiva.Client
	base *url.URL
}

type sectionsPage struct {
	Data []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data"`
	NextLink string `json:"@nextLink"`
}

// ListSections returns every section of the report document, following
// @nextLink.
func (l *liveWorkiva) ListSections(ctx context.Context, documentID string) ([]Section, error) {
	path := "/documents/" + url.PathEscape(documentID) + "/sections?$maxpagesize=100"
	var out []Section
	for path != "" {
		page, err := l.sectionsPage(ctx, path)
		if err != nil {
			return nil, err
		}
		for _, s := range page.Data {
			out = append(out, Section{ID: s.ID, Name: s.Name})
		}
		path, err = l.relativeLink(page.NextLink)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (l *liveWorkiva) sectionsPage(ctx context.Context, path string) (*sectionsPage, error) {
	resp, err := l.Do(ctx, http.MethodGet, path, nil, ratelimit.CategoryReads)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list sections: HTTP %d", resp.StatusCode)
	}
	var page sectionsPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("list sections: decode: %w", err)
	}
	return &page, nil
}

// relativeLink turns an absolute @nextLink into a path on the API host. A
// link to another host is refused, so the access token is never sent there.
func (l *liveWorkiva) relativeLink(link string) (string, error) {
	if link == "" {
		return "", nil
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("list sections: bad @nextLink: %w", err)
	}
	if u.IsAbs() && (u.Scheme != l.base.Scheme || u.Host != l.base.Host) {
		return "", errors.New("list sections: @nextLink points to another host")
	}
	return u.RequestURI(), nil
}

// DeleteSection deletes one section of the report document; Workiva answers
// 204.
func (l *liveWorkiva) DeleteSection(ctx context.Context, documentID, sectionID string) error {
	path := "/documents/" + url.PathEscape(documentID) + "/sections/" + url.PathEscape(sectionID)
	resp, err := l.Do(ctx, http.MethodDelete, path, nil, ratelimit.CategoryWrites)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete section: HTTP %d", resp.StatusCode)
	}
	return nil
}
