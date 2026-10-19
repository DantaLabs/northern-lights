// Package sectiontext stages and confirms the narrative and verbatim text of a
// report document section.
package sectiontext

import (
	"context"
	"time"
)

// Block is one paragraph of section text. Kind: narrative | verbatim
type Block struct{ Kind, Text, VerbatimID string }

// Preview is staged section text awaiting confirmation.
type Preview struct {
	ID, Token, SectionID, SectionTitle string
	Blocks                             []Block
	ExpiresAt                          time.Time
}

// Result is the outcome of confirming section text.
type Result struct{ SectionID, DocumentID, WorkivaSectionID, Status, AuditID string }

// Service stages section text and confirms it.
type Service interface {
	Stage(ctx context.Context, actor, sectionID string, blocks []Block) (Preview, error)
	Confirm(ctx context.Context, actor, id, token string) (Result, error)
}
