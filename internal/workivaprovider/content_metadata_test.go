package workivaprovider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/workiva"
)

type metadataBackend struct {
	data      *workiva.SheetData
	gotRange  string
	gotFields []string
}

type explicitMetadataBackend struct {
	*metadataBackend
	metadata ContentMetadata
	calls    int
}

func (b *explicitMetadataBackend) ReadContentMetadata(_ context.Context, _, _, _ string) (ContentMetadata, error) {
	b.calls++
	return b.metadata, nil
}

func (b *metadataBackend) ListSpreadsheets(context.Context) ([]workiva.Spreadsheet, error) {
	return nil, nil
}
func (b *metadataBackend) ListSheets(context.Context, string) ([]workiva.Sheet, error) {
	return nil, nil
}
func (b *metadataBackend) GetSheetData(_ context.Context, _, _, cellRange string, fields []string) (*workiva.SheetData, error) {
	b.gotRange = cellRange
	b.gotFields = fields
	if b.data != nil && len(b.data.Pages) == 0 {
		b.data.Pages = []workiva.SheetData{{Range: b.data.Range, Cells: b.data.Cells}}
	}
	return b.data, nil
}
func (b *metadataBackend) GetSheetDataTyped(ctx context.Context, spreadsheetID, sheetID, cellRange string, fields []string) (*workiva.SheetData, error) {
	return b.GetSheetData(ctx, spreadsheetID, sheetID, cellRange, fields)
}
func (b *metadataBackend) UpdateSheetWithRetryAfter(context.Context, string, string, workiva.SheetUpdate) (string, time.Duration, error) {
	return "", 0, nil
}
func (b *metadataBackend) WaitOperationWithInitialRetryAfter(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (*metadataBackend) APIVersion() string { return workiva.DefaultAPIVersion }

func TestReadContentMetadataPreservesPresenceAndUnknownAccess(t *testing.T) {
	rng, _ := workiva.A1ToRange("B3")
	b := &metadataBackend{data: &workiva.SheetData{Range: &rng, Cells: [][]workiva.Cell{{{
		Value: "=SUM(A1:A2)", ValuePresent: true, CalculatedValue: 12, CalculatedValuePresent: true,
		Formats: []byte(`{"textFormat":{"bold":false}}`), FormatsPresent: true,
		EffectiveFormats: []byte(`{"textFormat":{"bold":true}}`), EffectiveFormatsPresent: true,
	}}}}}
	router := NewRouter(b)
	got, err := router.ReadContentMetadata(context.Background(), "sp-1", "sh-1", "B3")
	if err != nil {
		t.Fatal(err)
	}
	if b.gotRange != "B3" || len(b.gotFields) != 4 {
		t.Fatalf("request range=%q fields=%v", b.gotRange, b.gotFields)
	}
	if !got.ValuePresent || string(got.RawValue) != `"=SUM(A1:A2)"` || !got.CalculatedValuePresent || string(got.CalculatedValue) != "12" {
		t.Fatalf("values=%+v", got)
	}
	if got.FormattingSHA256 == "" || got.Provenance.Cache != "bypassed" || got.Provenance.APIVersion != workiva.DefaultAPIVersion {
		t.Fatalf("provenance/hash=%+v", got)
	}
	if got.Protection != "unknown" || got.Writable != "unknown" {
		t.Fatalf("access was inferred: protection=%s writable=%s", got.Protection, got.Writable)
	}
	if got.LiteralWriteFormatPreservation != "unknown" || got.LiteralWriteAPIVersion != "" || got.LiteralWriteEndpoint != "" {
		t.Fatalf("REST fallback claimed literal-write preservation: %+v", got)
	}
	if err := ValidateContentMetadata(got); err != nil {
		t.Fatalf("validate default metadata: %v", err)
	}
}

func TestReadContentMetadataDoesNotInferMissingValue(t *testing.T) {
	rng, _ := workiva.A1ToRange("A1")
	b := &metadataBackend{data: &workiva.SheetData{Range: &rng, Cells: [][]workiva.Cell{{{CalculatedValue: 3, CalculatedValuePresent: true}}}}}
	got, err := NewRouter(b).ReadContentMetadata(context.Background(), "sp", "sh", "A1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ValuePresent || got.RawValue != nil || !got.CalculatedValuePresent {
		t.Fatalf("presence=%+v", got)
	}
}

func TestReadContentMetadataUnavailableAndExactCellGuard(t *testing.T) {
	var router *Router
	if _, err := router.ReadContentMetadata(context.Background(), "sp", "sh", "A1"); err == nil || err.Error() != "workivaprovider: content metadata provider unavailable" {
		t.Fatalf("nil router error=%v", err)
	}
	b := &metadataBackend{}
	r := NewRouter(b)
	if _, err := r.ReadContentMetadata(context.Background(), "sp", "sh", "A1:B2"); err == nil {
		t.Fatal("range locator accepted")
	}
	rng, _ := workiva.A1ToRange("A1")
	b.data = &workiva.SheetData{Range: &rng, Cells: [][]workiva.Cell{{{}, {}}}}
	if _, err := r.ReadContentMetadata(context.Background(), "sp", "sh", "A1"); err == nil {
		t.Fatal("multi-cell response accepted")
	}
}

func TestReadContentMetadataRejectsWrongRangeAndExtraPage(t *testing.T) {
	wanted, _ := workiva.A1ToRange("A1")
	wrong, _ := workiva.A1ToRange("A2")
	for _, data := range []*workiva.SheetData{
		{Range: &wrong, Cells: [][]workiva.Cell{{{}}}},
		{Range: &wanted, Cells: [][]workiva.Cell{{{}}}, Pages: []workiva.SheetData{{Range: &wanted, Cells: [][]workiva.Cell{{{}}}}, {Range: &wanted, Cells: [][]workiva.Cell{{{}}}}}},
	} {
		backend := &metadataBackend{data: data}
		if _, err := NewRouter(backend).ReadContentMetadata(context.Background(), "sp", "sh", "A1"); err == nil {
			t.Fatal("accepted response with wrong range or extra page")
		}
	}
}

func TestCanonicalRawPreservesDecimalLexeme(t *testing.T) {
	got, err := canonicalRaw([]byte(`{"n":1.2300}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"n":1.2300}` {
		t.Fatalf("canonical decimal = %s", got)
	}
}

func TestReadContentMetadataUsesAndValidatesExplicitCapability(t *testing.T) {
	backend := &explicitMetadataBackend{metadataBackend: &metadataBackend{}, metadata: ContentMetadata{
		SpreadsheetID: "sp", SheetID: "sh", Locator: "A1", FormattingSHA256: emptyFormattingHash(),
		Provenance: ContentMetadataProvenance{Provider: "independent", APIVersion: "2026-01-01", Endpoint: "provider metadata", QueryRange: "A1", Cache: "bypassed"},
		Protection: "protected", Writable: "not_writable",
		LiteralWriteFormatPreservation: "preserves", LiteralWriteAPIVersion: workiva.DefaultAPIVersion, LiteralWriteEndpoint: literalWriteUpdateEndpoint,
	}}
	got, err := NewRouter(backend).ReadContentMetadata(context.Background(), "sp", "sh", "A1")
	if err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 || got.Protection != "protected" || got.Writable != "not_writable" || got.LiteralWriteFormatPreservation != "preserves" {
		t.Fatalf("explicit capability result=%+v calls=%d", got, backend.calls)
	}
	backend.metadata.SpreadsheetID = "other"
	if _, err := NewRouter(backend).ReadContentMetadata(context.Background(), "sp", "sh", "A1"); err == nil {
		t.Fatal("mismatched capability coordinates accepted")
	}
	backend.metadata = ContentMetadata{
		SpreadsheetID: "sp", SheetID: "sh", Locator: "A1", FormattingSHA256: emptyFormattingHash(),
		Provenance: ContentMetadataProvenance{Provider: "independent", APIVersion: "2022-01-01", Endpoint: "provider metadata", QueryRange: "A1", Cache: "bypassed"},
	}
	if _, err := NewRouter(backend).ReadContentMetadata(context.Background(), "sp", "sh", "A1"); err == nil {
		t.Fatal("unsupported API version accepted")
	}
	backend.metadata = ContentMetadata{
		SpreadsheetID: "sp", SheetID: "sh", Locator: "A1", ValuePresent: true, RawValue: json.RawMessage(`{"unexpected":true}`), FormattingSHA256: emptyFormattingHash(),
		Provenance: ContentMetadataProvenance{Provider: "independent", APIVersion: "2026-01-01", Endpoint: "provider metadata", QueryRange: "A1", Cache: "bypassed"},
	}
	if _, err := NewRouter(backend).ReadContentMetadata(context.Background(), "sp", "sh", "A1"); err == nil {
		t.Fatal("object cell value accepted")
	}
}

func emptyFormattingHash() string {
	hash, _ := ContentFormattingHash(nil, false, nil, false)
	return hash
}

func TestContentFormattingHashTracksPresenceAndPreservesDecimalLexemes(t *testing.T) {
	missing, err := ContentFormattingHash(nil, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	null, err := ContentFormattingHash(json.RawMessage("null"), true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	decimal, err := ContentFormattingHash(json.RawMessage(`{"n":1.2300}`), true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if missing == null || missing == decimal {
		t.Fatal("format presence or decimal form was not included in checksum")
	}
	metadata := ContentMetadata{SpreadsheetID: "sp", SheetID: "sh", Locator: "A1", FormattingSHA256: decimal, RawFormats: json.RawMessage(`{"n":1.2300}`), FormatsPresent: true, Provenance: ContentMetadataProvenance{Provider: "trusted", APIVersion: workiva.DefaultAPIVersion, Endpoint: "metadata", QueryRange: "A1", Cache: "bypassed"}, Protection: "unknown", Writable: "unknown", LiteralWriteFormatPreservation: "unknown"}
	if err := ValidateContentMetadata(metadata); err != nil {
		t.Fatalf("validate canonical formatting metadata: %v", err)
	}
	metadata.FormattingSHA256 = missing
	if err := ValidateContentMetadata(metadata); err == nil {
		t.Fatal("mismatched formatting checksum accepted")
	}
	metadata.FormattingSHA256 = decimal
	metadata.RawFormats = json.RawMessage(`{"n":1.23}`)
	if err := ValidateContentMetadata(metadata); err == nil {
		t.Fatal("canonical payload with inconsistent checksum accepted")
	}
}

func TestValidateContentMetadataRejectsUnboundLiteralWriteClaims(t *testing.T) {
	hash := emptyFormattingHash()
	base := ContentMetadata{SpreadsheetID: "sp", SheetID: "sh", Locator: "A1", FormattingSHA256: hash, Provenance: ContentMetadataProvenance{Provider: "trusted", APIVersion: workiva.DefaultAPIVersion, Endpoint: "metadata", QueryRange: "A1", Cache: "bypassed"}, Protection: "unknown", Writable: "unknown", LiteralWriteFormatPreservation: "unknown"}
	if err := ValidateContentMetadata(base); err != nil {
		t.Fatal(err)
	}
	base.LiteralWriteFormatPreservation = "preserves"
	if err := ValidateContentMetadata(base); err == nil {
		t.Fatal("preserves claim without version/endpoint accepted")
	}
	base.LiteralWriteAPIVersion = workiva.DefaultAPIVersion
	base.LiteralWriteEndpoint = "POST /wrong"
	if err := ValidateContentMetadata(base); err == nil {
		t.Fatal("preserves claim for wrong endpoint accepted")
	}
	base.LiteralWriteEndpoint = literalWriteUpdateEndpoint
	if err := ValidateContentMetadata(base); err != nil {
		t.Fatalf("pinned preserves claim: %v", err)
	}
}

func TestReadContentMetadataDeniesUntypedReader(t *testing.T) {
	reader := &fakeBackend{}
	if _, err := NewRouter(reader).ReadContentMetadata(context.Background(), "sp", "sh", "A1"); err == nil || err.Error() != "workivaprovider: typed content metadata provider unavailable" {
		t.Fatalf("untyped reader error=%v", err)
	}
}
