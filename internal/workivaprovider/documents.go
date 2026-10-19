package workivaprovider

import "context"

// DocumentWriter is an additive optional capability for writing section text in
// report documents. Callers probe for it by type assertion, the same pattern as
// ContentMetadataReader in content_metadata.go. The Router does not implement it.
type DocumentWriter interface {
	EnsureSection(ctx context.Context, documentID, title string) (sectionID string, err error)
	WriteSectionText(ctx context.Context, documentID, sectionID string, paragraphs []string) (operationURL string, err error)
	ReadSectionText(ctx context.Context, documentID, sectionID string) ([]string, error)
}
