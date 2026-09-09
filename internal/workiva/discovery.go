package workiva

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

const maxDiscoveryPages = 50

// Spreadsheet is a Workiva spreadsheet returned by the discovery API.
type Spreadsheet struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Template bool   `json:"template"`
	Created  string `json:"created"`
	Modified string `json:"modified"`
}

// Sheet is a Workiva sheet returned by the discovery API.
type Sheet struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Index int    `json:"index"`
}

type discoveryPage[T any] struct {
	Data     []T    `json:"data"`
	NextLink string `json:"@nextLink"`
}

// ListSpreadsheets discovers the Workiva spreadsheets available to the
// authenticated client. All pages linked by @nextLink are followed, up to
// the shared 50-page cap.
func (c *Client) ListSpreadsheets(ctx context.Context) ([]Spreadsheet, error) {
	return getDiscoveryPages[Spreadsheet](ctx, c, "/spreadsheets", "spreadsheets")
}

// ListSheets discovers the sheets within a Workiva spreadsheet. All pages
// linked by @nextLink are followed, up to the shared 50-page cap.
func (c *Client) ListSheets(ctx context.Context, spreadsheetID string) ([]Sheet, error) {
	path := fmt.Sprintf("/spreadsheets/%s/sheets", url.PathEscape(spreadsheetID))
	return getDiscoveryPages[Sheet](ctx, c, path, "sheets")
}

func getDiscoveryPages[T any](ctx context.Context, c *Client, path, resource string) ([]T, error) {
	var items []T
	nextPath := path
	for pageNum := 1; pageNum <= maxDiscoveryPages; pageNum++ {
		resp, err := c.Do(ctx, http.MethodGet, nextPath, nil, ratelimit.CategoryReads)
		if err != nil {
			return nil, err
		}

		var page discoveryPage[T]
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		closeErr := resp.Body.Close()
		if decodeErr != nil {
			if closeErr != nil {
				decodeErr = errors.Join(decodeErr, closeErr)
			}
			return nil, fmt.Errorf("decode %s response: %w", resource, decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s response: %w", resource, closeErr)
		}

		items = append(items, page.Data...)
		if page.NextLink == "" {
			return items, nil
		}
		if pageNum == maxDiscoveryPages {
			return nil, fmt.Errorf("%s pagination exceeded maximum of %d pages", resource, maxDiscoveryPages)
		}
		nextPath = page.NextLink
	}
	return nil, fmt.Errorf("%s pagination exceeded maximum of %d pages", resource, maxDiscoveryPages)
}
