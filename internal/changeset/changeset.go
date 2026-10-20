// Package changeset stages and confirms a set of field writes as one change set.
package changeset

import (
	"context"
	"time"
)

// Item is one requested field write.
type Item struct{ FieldName, Value, Source string }

// PreviewItem is one staged field write with its before and after values.
type PreviewItem struct{ FieldName, SpreadsheetID, SheetID, Range, Before, After, Source string }

// Preview is a staged change set awaiting confirmation.
type Preview struct {
	ID, Token string
	Summary   string
	Items     []PreviewItem
	ExpiresAt time.Time
}

// ItemResult is the outcome of one field write.
type ItemResult struct {
	FieldName, Status, ReadBack, AuditID, OperationURL string
	Before, After, Message                             string
}

// Result is the outcome of confirming a change set.
// Status values: completed | partial | reconciliation_required.
type Result struct {
	ID, Status string
	Summary    string
	Items      []ItemResult
}

// Service stages change sets and confirms them.
type Service interface {
	Stage(ctx context.Context, actor string, items []Item) (Preview, error)
	Confirm(ctx context.Context, actor, id, token string) (Result, error)
}
