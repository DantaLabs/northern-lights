package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

type scriptedReader struct {
	mu      sync.Mutex
	calls   int
	results map[string]ProviderRead
	errors  map[string]error
}

func (r *scriptedReader) ReadUncached(_ context.Context, request SourceRequest) (ProviderRead, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if err := r.errors[request.Locator]; err != nil {
		return ProviderRead{}, err
	}
	return r.results[request.Locator], nil
}

func (r *scriptedReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func snapshotBundle(t *testing.T, store *Store, fields []FieldDefinition) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		bundle.Reports[0].Fields = fields
		bundle.ExportProfiles = []ExportProfile{{ProfileID: "profile-standard", Revision: 1, Status: "active", PermittedSubjects: []string{"snapshot", "validation_run", "comparison"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 1000, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}}
		bundle.Reports[0].ExportProfiles = []string{"profile-standard"}
		bundle.MaterialityPolicies = []MaterialityPolicy{{PolicyID: "mat-default", Revision: 1, Status: "active", AbsoluteThreshold: "1", RelativeThreshold: "0.05", Direction: "absolute_or_relative", ZeroBaseline: "not_comparable", MissingBehavior: "not_comparable", TypeChangeBehavior: "not_comparable", Rounding: "half_even"}}
		bundle.Reports[0].MaterialityPolicyID = "mat-default"
	})
	validated, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(assuranceContext(), validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(assuranceContext(), validated.Bundle.BundleID, validated.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, public); err != nil {
		t.Fatal(err)
	}
}

func TestC009C010SnapshotIsUncachedImmutableAndIdempotent(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "scope2-kwh", ResourceID: "resource-1", ExternalResourceID: "sp-1", SubresourceID: "sh-1", Locator: "B3", Kind: ValueNumber, Unit: "kWh", Scale: "ones", Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	reader := &scriptedReader{results: map[string]ProviderRead{
		"B3": {Value: ProviderValue{Value: json.Number("24683.00")}, ProviderRevision: ProviderRevision{Value: "rev-7", Strength: RevisionVerified}, CacheBypassed: true},
	}, errors: map[string]error{}}
	service := SnapshotService{Store: store, Reader: reader, Now: func() time.Time { return time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC) }}
	request := SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, Consistency: ConsistencyBestEffort, RetentionClass: "standard", IdempotencyKey: "snapshot-key-1"}
	first, err := service.Capture(assuranceContext(), "actor-1", "audit-1", request)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if first.Status != SnapshotCompleted || first.Completeness != CompletenessComplete || first.SnapshotID == "" || len(first.Observations) != 1 {
		t.Fatalf("first snapshot = %#v", first)
	}
	if first.Observations[0].TypedValue.Number != "24683" || first.ContentHash == "" || !reader.results["B3"].CacheBypassed {
		t.Fatalf("observation = %#v", first.Observations[0])
	}
	stored, err := store.Snapshot(assuranceContext(), first.SnapshotID)
	if err != nil {
		t.Fatalf("Snapshot read-after-capture: %v", err)
	}
	if len(stored.Observations) != 1 || stored.Observations[0].FieldID != "scope2-kwh" || stored.Observations[0].TypedValue.Number != "24683" || stored.Observations[0].ProviderRevision.Value != "rev-7" || stored.Observations[0].SourceFingerprint == "" {
		t.Fatalf("stored snapshot observation = %#v", stored.Observations)
	}
	if len(stored.ItemErrors) != 0 {
		t.Fatalf("stored complete snapshot item errors = %#v", stored.ItemErrors)
	}

	replay, err := service.Capture(assuranceContext(), "actor-1", "audit-2", request)
	if err != nil {
		t.Fatalf("Capture replay: %v", err)
	}
	if replay.Status != SnapshotIdempotencyReplay || replay.SnapshotID != first.SnapshotID || reader.callCount() != 1 {
		t.Fatalf("replay = %#v calls=%d", replay, reader.callCount())
	}
	if _, err := db.Exec(`UPDATE assurance_snapshot_observations SET typed_value_json='{}' WHERE tenant_id=? AND snapshot_id=?`, testTenant, first.SnapshotID); err == nil {
		t.Fatal("immutable observation update succeeded")
	}
	if _, err := db.Exec(`DELETE FROM assurance_snapshots WHERE tenant_id=? AND snapshot_id=?`, testTenant, first.SnapshotID); err == nil {
		t.Fatal("immutable snapshot delete succeeded")
	}
}

func TestC011SnapshotPartialAndFailedBehaviorIsExplicit(t *testing.T) {
	fields := []FieldDefinition{
		{FieldID: "ok", ResourceID: "r1", ExternalResourceID: "sp-1", SubresourceID: "sh-1", Locator: "B3", Kind: ValueInteger, Required: true, Order: 1},
		{FieldID: "missing", ResourceID: "r2", ExternalResourceID: "sp-1", SubresourceID: "sh-1", Locator: "B4", Kind: ValueInteger, Required: true, Order: 2},
	}
	for _, allowPartial := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "partial"}[allowPartial], func(t *testing.T) {
			store, _ := openTestStore(t)
			snapshotBundle(t, store, fields)
			reader := &scriptedReader{
				results: map[string]ProviderRead{"B3": {Value: ProviderValue{Value: json.Number("0")}, CacheBypassed: true}},
				errors:  map[string]error{"B4": errors.New("permission denied")},
			}
			service := SnapshotService{Store: store, Reader: reader}
			response, err := service.Capture(assuranceContext(), "actor-1", "audit-1", SnapshotRequest{
				ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, AllowPartial: allowPartial, IdempotencyKey: "partial-key",
			})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			if len(response.ItemErrors) != 1 || response.ItemErrors[0].FieldID != "missing" {
				t.Fatalf("item errors = %#v", response.ItemErrors)
			}
			if allowPartial {
				if response.Status != SnapshotPartial || response.Completeness != CompletenessIncomplete || response.SnapshotID == "" || len(response.Observations) != 1 {
					t.Fatalf("partial response = %#v", response)
				}
				stored, err := store.Snapshot(assuranceContext(), response.SnapshotID)
				if err != nil {
					t.Fatalf("partial Snapshot read-after-capture: %v", err)
				}
				if len(stored.Observations) != 1 || stored.Observations[0].FieldID != "ok" || len(stored.ItemErrors) != 1 || stored.ItemErrors[0].FieldID != "missing" || stored.ItemErrors[0].Code != "denied" {
					t.Fatalf("stored partial snapshot = %#v", stored)
				}
			} else if response.Status != SnapshotFailed || response.Completeness != CompletenessNotCreated || response.SnapshotID != "" || len(response.Observations) != 0 {
				t.Fatalf("failed response = %#v", response)
			}
		})
	}
}

func TestC057C070AuthorizationPrecedesIdempotencyReservation(t *testing.T) {
	store, db := openTestStore(t)
	field := FieldDefinition{FieldID: "known", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "A1", Kind: ValueText, Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	reader := &scriptedReader{results: map[string]ProviderRead{}, errors: map[string]error{}}
	service := SnapshotService{
		Store: store, Reader: reader,
		Authorize: func(context.Context, FieldDefinition) error { return errors.New("denied") },
	}
	_, err := service.Capture(assuranceContext(), "actor-1", "audit-1", SnapshotRequest{
		ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, IdempotencyKey: "must-not-reserve",
	})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "resource_denied" {
		t.Fatalf("Capture error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_idempotency_records`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || reader.callCount() != 0 {
		t.Fatalf("unauthorized request created %d reservations and %d provider calls", count, reader.callCount())
	}
}

func TestC008C064ReportResolutionAndConsistencyFailBeforeProvider(t *testing.T) {
	store, _ := openTestStore(t)
	field := FieldDefinition{FieldID: "known", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "A1", Kind: ValueText, Required: true, Order: 1}
	snapshotBundle(t, store, []FieldDefinition{field})
	reader := &scriptedReader{results: map[string]ProviderRead{}, errors: map[string]error{}}
	service := SnapshotService{Store: store, Reader: reader}
	for _, tc := range []struct {
		name string
		req  SnapshotRequest
		code string
	}{
		{name: "unknown field", req: SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, FieldIDs: []string{"unknown"}, IdempotencyKey: "k1"}, code: "field_not_approved"},
		{name: "ambiguous period", req: SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3", Label: "wrong"}, IdempotencyKey: "k2"}, code: "period_mismatch"},
		{name: "revision pinned unsupported", req: SnapshotRequest{ReportID: "energy-report", Period: Period{Key: "2026-Q3"}, Consistency: ConsistencyRevisionPinned, IdempotencyKey: "k3"}, code: "capability_unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := service.Capture(assuranceContext(), "actor-1", "audit-1", tc.req)
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != tc.code {
				t.Fatalf("Capture error = %v, want code %s", err, tc.code)
			}
		})
	}
	if reader.callCount() != 0 {
		t.Fatalf("provider calls = %d, want zero", reader.callCount())
	}
}
