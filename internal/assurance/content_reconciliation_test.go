package assurance

import (
	"encoding/json"
	"testing"
	"time"
)

func TestVerifyAppliedContentReconciliationEvidenceRequiresFreshExactTypedReadback(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	claimed := now.Add(-time.Minute)
	preview := acknowledgementPreview{
		PlacementIntentID: "placement-1", ActorBindingID: "tenant/actor", IntendedValue: json.RawMessage(`"Exact value"`),
		Destination: acknowledgementPreviewDestination{ID: "profile", Revision: 1, ContentHash: "profile-hash", ResourceID: "spreadsheet", SheetID: "sheet", Cell: "B4"},
	}
	formats := json.RawMessage(`{}`)
	formatHash, err := ContentFormattingHash(formats, true, formats, true)
	if err != nil {
		t.Fatal(err)
	}
	preview.ProviderMetadata.FormattingSHA256 = formatHash
	readback, err := CanonicalJSON(ContentReadbackEvidence{
		SpreadsheetID: "spreadsheet", SheetID: "sheet", Locator: "B4", RawValue: json.RawMessage(`"Exact value"`), ValuePresent: true,
		RawFormats: formats, FormatsPresent: true, EffectiveFormats: formats, EffectiveFormatsPresent: true, FormattingSHA256: formatHash,
		Protection: "unprotected", Writable: "writable",
		Provenance: struct {
			Provider   string `json:"provider"`
			APIVersion string `json:"api_version"`
			Endpoint   string `json:"endpoint"`
			QueryRange string `json:"query_range"`
			Cache      string `json:"cache"`
		}{"workiva_rest", "2026-01-01", "GET /spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata", "B4", "bypassed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := CanonicalJSON(struct {
		Reference   string
		Status      string
		ResourceURL string
	}{"op-123", "completed", ""})
	if err != nil {
		t.Fatal(err)
	}
	targetHash, err := contentPlacementTargetHash("spreadsheet", "sheet", "B4")
	if err != nil {
		t.Fatal(err)
	}
	base := ContentReconciliationEvidence{Kind: "read_back", OperationReferenceHash: digestHex(HashBytes([]byte("op-123"))), OperationStatus: "completed", OperationInspectionJSON: inspection, OperationInspectionHash: digestHex(HashBytes(inspection)), TargetHash: targetHash, IntendedHash: digestHex(HashBytes(preview.IntendedValue)), ReadbackJSON: readback, ReadbackHash: digestHex(HashBytes(readback)), CacheBypassed: true, ProviderObservedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	if err := verifyAppliedContentReconciliationEvidence(base, preview, "op-123", base.TargetHash, base.IntendedHash, claimed, now); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}

	cases := map[string]func(*ContentReconciliationEvidence){
		"expired": func(e *ContentReconciliationEvidence) { e.ExpiresAt = now },
		"wrong operation digest": func(e *ContentReconciliationEvidence) {
			e.OperationReferenceHash = digestHex(HashBytes([]byte("other")))
		},
		"inspection reference mismatch": func(e *ContentReconciliationEvidence) {
			e.OperationInspectionJSON, _ = CanonicalJSON(struct {
				Reference   string
				Status      string
				ResourceURL string
			}{"other", "completed", ""})
			e.OperationInspectionHash = digestHex(HashBytes(e.OperationInspectionJSON))
		},
		"inspection status mismatch": func(e *ContentReconciliationEvidence) {
			e.OperationInspectionJSON, _ = CanonicalJSON(struct {
				Reference   string
				Status      string
				ResourceURL string
			}{"op-123", "failed", ""})
			e.OperationInspectionHash = digestHex(HashBytes(e.OperationInspectionJSON))
		},
		"stale observation":      func(e *ContentReconciliationEvidence) { e.ProviderObservedAt = claimed },
		"readback hash mismatch": func(e *ContentReconciliationEvidence) { e.ReadbackHash = digestHex(HashBytes([]byte(`{}`))) },
		"cached":                 func(e *ContentReconciliationEvidence) { e.CacheBypassed = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			observed := base
			mutate(&observed)
			if err := verifyAppliedContentReconciliationEvidence(observed, preview, "op-123", base.TargetHash, base.IntendedHash, claimed, now); err == nil {
				t.Fatal("invalid applied proof accepted")
			}
		})
	}
}
