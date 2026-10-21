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

// DocumentWriterFor resolves the optional document capability on the primary
// backend. Sheet read/write overrides do not change the document destination.
func DocumentWriterFor(backend Backend) (DocumentWriter, bool) {
	if router, ok := backend.(*Router); ok {
		return DocumentWriterFor(router.primary)
	}
	writer, ok := backend.(DocumentWriter)
	return writer, ok
}
