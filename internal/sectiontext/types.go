// Package sectiontext stages and confirms the narrative and verbatim text of a
// report document section.
package sectiontext

import (
	"context"
	"time"
)

// Block is one paragraph of section text. Kind: narrative | verbatim | chart.
// A chart block lists field keys in Text; the server draws them from the
// prepared facts and inserts the image after the text. Note is
// the agent's reasoning for a narrative paragraph; it is written as a Workiva
// bookmark named "NL AGENT: <note>", never as report text.
type Block struct{ Kind, Text, VerbatimID, Note string }

// Preview is staged section text awaiting confirmation.
type Preview struct {
	ID, Token, SectionID, SectionTitle string
	Blocks                             []Block
	ExpiresAt                          time.Time
}

// Result is the outcome of confirming section text.
// Notes and Chart are "", "completed" or "failed"; neither undoes the text.
// ReviewTask is the Workiva review task ID ("failed" if it could not be
// opened); ReviewURL links to it.
type Result struct {
	SectionID, DocumentID, WorkivaSectionID, Status, AuditID, Notes, Chart string
	ReviewTask, ReviewURL                                                  string
}

// Service stages section text and confirms it.
type Service interface {
	Stage(ctx context.Context, actor, sectionID string, blocks []Block) (Preview, error)
	Confirm(ctx context.Context, actor, id, token string) (Result, error)
}
