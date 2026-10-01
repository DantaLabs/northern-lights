package assurance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dantalabs/northern-lights/internal/identity"
)

const testTenant = "11111111-1111-1111-1111-111111111111"

func assuranceContext() context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "actor-1"})
}

func openTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewWithDB(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewWithDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return store, db
}

func signedBundle(t *testing.T, private ed25519.PrivateKey, mutate func(*Bundle)) ([]byte, []byte) {
	t.Helper()
	bundle := Bundle{
		SchemaVersion: 1,
		BundleID:      "bundle-1",
		BundleVersion: 1,
		TenantID:      testTenant,
		Reports: []ReportRevision{{
			ReportID:           "energy-report",
			Revision:           1,
			Name:               "Energy report",
			Description:        "Quarterly energy assurance",
			Owner:              "sustainability",
			Status:             "active",
			RetentionClass:     "standard",
			ResourcePolicyHash: strings.Repeat("a", 64),
			Periods:            []Period{{Key: "2026-Q3", Label: "Q3 2026", Start: "2026-07-01", End: "2026-09-30"}},
			Fields: []FieldDefinition{{
				FieldID: "scope2-kwh", ResourceID: "resource-1", ExternalResourceID: "sp-1", SubresourceID: "sh-1",
				Locator: "B3", Kind: ValueNumber, Unit: "kWh", Scale: "ones", Required: true, Order: 1,
			}},
		}},
	}
	if mutate != nil {
		mutate(&bundle)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalJSONBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, ed25519.Sign(private, canonical)
}

func activateTestBundle(t *testing.T, store *Store) ed25519.PublicKey {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, nil)
	validated, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatalf("ValidateBundle: %v", err)
	}
	ctx := assuranceContext()
	if err := store.StageBundle(ctx, validated); err != nil {
		t.Fatalf("StageBundle: %v", err)
	}
	if err := store.RequestActivation(ctx, validated.Bundle.BundleID, validated.Bundle.BundleVersion); err != nil {
		t.Fatalf("RequestActivation: %v", err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return public
}

func TestC066AssuranceMigrationsAreContiguousV1ThroughV9AndTenantFirst(t *testing.T) {
	_, db := openTestStore(t)
	rows, err := db.Query(`SELECT version FROM schema_migrations WHERE app='assurance' ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var versions []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if got := strings.Trim(strings.ReplaceAll(strings.TrimSpace(toJSON(versions)), ",", " "), "[]"); got != "1 2 3 4 5 6 7 8 9" {
		t.Fatalf("assurance versions = %v, want 1..9", versions)
	}
	for _, table := range assuranceTables() {
		columns, err := tableColumns(db, table)
		if err != nil {
			t.Fatalf("%s columns: %v", table, err)
		}
		if len(columns) == 0 || columns[0] != "tenant_id" {
			t.Fatalf("%s first column = %v, want tenant_id", table, columns)
		}
		for _, column := range columns {
			lower := strings.ToLower(column)
			if lower == "idempotency_key" || lower == "confirmation_token" || lower == "raw_token" || lower == "provider_secret" {
				t.Fatalf("%s contains forbidden raw secret column %q", table, column)
			}
		}
	}
}

func TestC061SignedTenantBoundBundleActivatesAtomicallyAndInvalidCandidateFailsClosed(t *testing.T) {
	store, db := openTestStore(t)
	public := activateTestBundle(t, store)
	report, err := store.ResolveReport(assuranceContext(), "energy-report", Period{Key: "2026-Q3"}, nil)
	if err != nil || report.Revision != 1 {
		t.Fatalf("ResolveReport active: report=%#v err=%v", report, err)
	}

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, func(bundle *Bundle) {
		bundle.BundleID = "bundle-2"
		bundle.BundleVersion = 2
		bundle.Reports[0].Revision = 2
	})
	validated, err := ValidateBundle(raw, sig, testTenant, private.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(assuranceContext(), validated); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(assuranceContext(), "bundle-2", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE assurance_bundle_candidates SET bundle_json='{}' WHERE tenant_id=? AND bundle_id='bundle-2'`, testTenant); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(assuranceContext(), testTenant, public); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Bootstrap tampered candidate = %v, want ErrNotReady", err)
	}
	if _, err := store.ResolveReport(assuranceContext(), "energy-report", Period{Key: "2026-Q3"}, nil); !errors.Is(err, ErrNotReady) {
		t.Fatalf("prior active report served while candidate invalid: %v", err)
	}
	var revision2 int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_report_revisions WHERE tenant_id=? AND report_id='energy-report' AND revision=2`, testTenant).Scan(&revision2); err != nil {
		t.Fatal(err)
	}
	if revision2 != 0 {
		t.Fatalf("invalid candidate partially activated %d revision rows", revision2)
	}
}

func TestC061BundleRejectsTamperCrossTenantUnknownKeysAndDuplicateIDs(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, sig := signedBundle(t, private, nil)
	if _, err := ValidateBundle(append(raw, ' '), sig, "other-tenant", public); err == nil {
		t.Fatal("cross-tenant bundle accepted")
	}
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-2] ^= 1
	if _, err := ValidateBundle(tampered, sig, testTenant, public); err == nil {
		t.Fatal("tampered bundle accepted")
	}
	unknown := append(raw[:len(raw)-1], []byte(`,"secret":"nope"}`)...)
	canonical, _ := CanonicalJSONBytes(unknown)
	if _, err := ValidateBundle(unknown, ed25519.Sign(private, canonical), testTenant, public); err == nil {
		t.Fatal("bundle with unknown key accepted")
	}
	duplicateRaw, duplicateSig := signedBundle(t, private, func(bundle *Bundle) {
		bundle.Reports[0].Fields = append(bundle.Reports[0].Fields, bundle.Reports[0].Fields[0])
	})
	if _, err := ValidateBundle(duplicateRaw, duplicateSig, testTenant, public); err == nil {
		t.Fatal("bundle with duplicate field ID accepted")
	}
}

func TestC061ActivationExcludesReportsOmittedFromNewBundle(t *testing.T) {
	store, _ := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := assuranceContext()

	raw, sig := signedBundle(t, private, nil)
	first, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, first.Bundle.BundleID, first.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}

	raw, sig = signedBundle(t, private, func(bundle *Bundle) {
		bundle.BundleID = "bundle-2"
		bundle.BundleVersion = 2
		bundle.Reports[0].ReportID = "new-report"
	})
	second, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, second.Bundle.BundleID, second.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ResolveReport(ctx, "energy-report", Period{Key: "2026-Q3"}, nil); err == nil {
		t.Fatal("report omitted from active bundle remained resolvable")
	}
	if _, err := store.ResolveReport(ctx, "new-report", Period{Key: "2026-Q3"}, nil); err != nil {
		t.Fatalf("new active report did not resolve: %v", err)
	}
}

func TestC061ActivationIsIdempotentForUnchangedReportRevision(t *testing.T) {
	store, db := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := assuranceContext()

	raw, sig := signedBundle(t, private, nil)
	first, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, first.Bundle.BundleID, first.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}

	raw, sig = signedBundle(t, private, func(bundle *Bundle) {
		bundle.BundleID = "bundle-2"
		bundle.BundleVersion = 2
	})
	second, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if second.Bundle.Reports[0].ContentHash != first.Bundle.Reports[0].ContentHash {
		t.Fatalf("unchanged report content hash changed: %q != %q", second.Bundle.Reports[0].ContentHash, first.Bundle.Reports[0].ContentHash)
	}
	if err := store.StageBundle(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, second.Bundle.BundleID, second.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatalf("activating newer bundle with unchanged report: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assurance_report_revisions WHERE tenant_id=? AND report_id=? AND revision=?`, testTenant, "energy-report", 1).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unchanged report revision row count = %d, want 1", count)
	}
}

func TestC061ActivationKeepsHistoricalRevisionsInactive(t *testing.T) {
	store, db := openTestStore(t)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := assuranceContext()
	raw, sig := signedBundle(t, private, nil)
	first, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, first.Bundle.BundleID, first.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}

	raw, sig = signedBundle(t, private, func(bundle *Bundle) {
		bundle.BundleID = "bundle-2"
		bundle.BundleVersion = 2
		latest := bundle.Reports[0]
		latest.Revision = 2
		bundle.Reports = append(bundle.Reports, latest)
	})
	second, err := ValidateBundle(raw, sig, testTenant, public)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StageBundle(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestActivation(ctx, second.Bundle.BundleID, second.Bundle.BundleVersion); err != nil {
		t.Fatal(err)
	}
	if err := store.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}

	var oldStatus, newStatus string
	if err := db.QueryRow(`SELECT status FROM assurance_report_revisions WHERE tenant_id=? AND report_id=? AND revision=1`, testTenant, "energy-report").Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM assurance_report_revisions WHERE tenant_id=? AND report_id=? AND revision=2`, testTenant, "energy-report").Scan(&newStatus); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "inactive" || newStatus != "active" {
		t.Fatalf("revision statuses = old %q new %q, want inactive/active", oldStatus, newStatus)
	}
}

func TestC061BundleValidationRejectsBoundsIntervalsAndUnresolvedReferences(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Bundle)
	}{
		{name: "report name bound", mutate: func(bundle *Bundle) { bundle.Reports[0].Name = strings.Repeat("n", 257) }},
		{name: "description bound", mutate: func(bundle *Bundle) { bundle.Reports[0].Description = strings.Repeat("d", 4097) }},
		{name: "period label bound", mutate: func(bundle *Bundle) { bundle.Reports[0].Periods[0].Label = strings.Repeat("p", 257) }},
		{name: "field locator bound", mutate: func(bundle *Bundle) { bundle.Reports[0].Fields[0].Locator = strings.Repeat("l", 513) }},
		{name: "field reference bound", mutate: func(bundle *Bundle) { bundle.Reports[0].Fields[0].ResourceID = strings.Repeat("r", 129) }},
		{name: "unsupported rule set reference", mutate: func(bundle *Bundle) { bundle.Reports[0].RuleSetID = "rules-v1" }},
		{name: "unsupported materiality policy reference", mutate: func(bundle *Bundle) { bundle.Reports[0].MaterialityPolicyID = "materiality-v1" }},
		{name: "incomplete effective interval", mutate: func(bundle *Bundle) { bundle.Reports[0].EffectiveFrom = "2026-01-01" }},
		{name: "negative field precision", mutate: func(bundle *Bundle) { bundle.Reports[0].Fields[0].Precision = -1 }},
		{name: "invalid field mapping revision", mutate: func(bundle *Bundle) { bundle.Reports[0].Fields[0].MappingRevision = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, sig := signedBundle(t, private, tc.mutate)
			if _, err := ValidateBundle(raw, sig, testTenant, public); err == nil {
				t.Fatalf("ValidateBundle accepted %s", tc.name)
			}
		})
	}

	t.Run("candidate revisions overlap", func(t *testing.T) {
		raw, sig := signedBundle(t, private, func(bundle *Bundle) {
			base := bundle.Reports[0]
			base.Revision = 2
			base.EffectiveFrom = "2026-06-01"
			base.EffectiveTo = "2026-12-31"
			bundle.Reports[0].EffectiveFrom = "2026-01-01"
			bundle.Reports[0].EffectiveTo = "2026-09-30"
			bundle.Reports = append(bundle.Reports, base)
		})
		if _, err := ValidateBundle(raw, sig, testTenant, public); err == nil {
			t.Fatal("ValidateBundle accepted overlapping effective intervals")
		}
	})

	t.Run("candidate revisions are monotonic in declared order", func(t *testing.T) {
		raw, sig := signedBundle(t, private, func(bundle *Bundle) {
			first := bundle.Reports[0]
			first.Revision = 2
			bundle.Reports = []ReportRevision{first, bundle.Reports[0]}
		})
		if _, err := ValidateBundle(raw, sig, testTenant, public); err == nil {
			t.Fatal("ValidateBundle accepted non-monotonic candidate revisions")
		}
	})
}

func TestC041ReservationHeartbeatExtendsLease(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := assuranceContext()
	base := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	request := ReservationRequest{ActorID: "actor-1", Tool: "workiva_snapshot_report", Action: "capture", IdempotencyDigest: DigestIdempotencyKey("heartbeat-key"), RequestDigest: HashBytes([]byte("request"))}
	owner, err := store.Reserve(ctx, request, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExecutionStarted(ctx, owner.RecordID, owner.OwnerNonce, base.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ExpireStaleReservations(ctx, base.Add(61*time.Second)); err != nil || changed != 0 {
		t.Fatalf("reservation expired despite execution heartbeat: changed=%d err=%v", changed, err)
	}
	if err := store.RenewReservation(ctx, owner.RecordID, owner.OwnerNonce, base.Add(60*time.Second)); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ExpireStaleReservations(ctx, base.Add(119*time.Second)); err != nil || changed != 0 {
		t.Fatalf("reservation expired despite renewal: changed=%d err=%v", changed, err)
	}
}

func TestC041StaleSnapshotReservationIsTerminalizedDuringBootstrapRecovery(t *testing.T) {
	store, db := openTestStore(t)
	public := activateTestBundle(t, store)
	ctx := assuranceContext()
	base := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	request := ReservationRequest{ActorID: "actor-1", Tool: "workiva_snapshot_report", Action: "capture", IdempotencyDigest: DigestIdempotencyKey("stale-snapshot"), RequestDigest: HashBytes([]byte("request"))}
	start := &snapshotStart{SnapshotID: "snapshot-running", ReportID: "energy-report", DefinitionRevision: 1, PeriodJSON: `{"key":"2026-Q3","label":"Q3 2026"}`, MappingSetHash: "mapping", ProviderRoute: "rest", RetentionClass: "standard", AuditID: "audit-1"}
	owner, err := store.reserveTx(ctx, nil, request, base, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExecutionStarted(ctx, owner.RecordID, owner.OwnerNonce, base); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE assurance_idempotency_records SET lease_expires_at=? WHERE record_id=?`, formatTimestamp(base.Add(-time.Second)), owner.RecordID); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Bootstrap(ctx, testTenant, public); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM assurance_snapshots WHERE snapshot_id=?`, start.SnapshotID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(SnapshotFailed) {
		t.Fatalf("stale running snapshot status = %q, want failed", status)
	}
}

func TestC041StartedReservationRenewalUsesOwnerNonceCAS(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := assuranceContext()
	request := ReservationRequest{ActorID: "actor-1", Tool: "workiva_snapshot_report", Action: "capture", IdempotencyDigest: DigestIdempotencyKey("owner-cas"), RequestDigest: HashBytes([]byte("request"))}
	owner, err := store.Reserve(ctx, request, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RenewReservation(ctx, owner.RecordID, "wrong-owner", time.Now().UTC()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("renewal with wrong owner nonce = %v, want ErrInvalidTransition", err)
	}
}

func TestC041C073IdempotencyLifecycleIsDigestOnlyAndTerminal(t *testing.T) {
	store, db := openTestStore(t)
	ctx := assuranceContext()
	rawKey := "raw-client-key-MUST-NOT-PERSIST"
	digest := DigestIdempotencyKey(rawKey)
	requestDigest := HashBytes([]byte("canonical-request"))
	request := ReservationRequest{ActorID: "actor-1", Tool: "workiva_snapshot_report", Action: "capture", IdempotencyDigest: digest, RequestDigest: requestDigest}

	owner, err := store.Reserve(ctx, request, time.Now().UTC())
	if err != nil || owner.Disposition != ReservationOwned || owner.OwnerNonce == "" || owner.CorrelationID == "" {
		t.Fatalf("owner reservation = %#v err=%v", owner, err)
	}
	inProgress, err := store.Reserve(ctx, request, time.Now().UTC())
	if err != nil || inProgress.Disposition != ReservationInProgress {
		t.Fatalf("concurrent reservation = %#v err=%v", inProgress, err)
	}
	conflictRequest := request
	conflictRequest.RequestDigest = HashBytes([]byte("different"))
	conflict, err := store.Reserve(ctx, conflictRequest, time.Now().UTC())
	if err != nil || conflict.Disposition != ReservationConflict {
		t.Fatalf("conflict reservation = %#v err=%v", conflict, err)
	}
	envelope := []byte(`{"status":"completed","snapshot_id":"snap-1"}`)
	if err := store.SealReservation(ctx, owner.RecordID, owner.OwnerNonce, envelope, "completed", "snap-1", "audit-1", time.Now().UTC()); err != nil {
		t.Fatalf("SealReservation: %v", err)
	}
	replay, err := store.Reserve(ctx, request, time.Now().UTC())
	canonicalEnvelope, _ := CanonicalJSONBytes(envelope)
	if err != nil || replay.Disposition != ReservationReplay || string(replay.Envelope) != string(canonicalEnvelope) {
		t.Fatalf("replay = %#v err=%v", replay, err)
	}
	if err := store.FailReservation(ctx, owner.RecordID, owner.OwnerNonce, StructuredError{Code: "late", Message: "late", Retryable: false}, time.Now().UTC()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal reservation changed: %v", err)
	}

	var schemaAndData strings.Builder
	rows, err := db.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		schemaAndData.WriteString(value)
	}
	_ = rows.Close()
	var storedDigest string
	if err := db.QueryRow(`SELECT idempotency_digest FROM assurance_idempotency_records WHERE record_id=?`, owner.RecordID).Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	schemaAndData.WriteString(storedDigest)
	if strings.Contains(schemaAndData.String(), rawKey) {
		t.Fatal("raw idempotency key found in schema or persisted values")
	}
	if storedDigest != hex.EncodeToString(digest[:]) {
		t.Fatalf("stored digest = %q", storedDigest)
	}
}

func TestC041ConcurrentSameKeyHasOneOwnerAndJanitorNeverReopens(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := assuranceContext()
	base := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	request := ReservationRequest{ActorID: "actor-1", Tool: "workiva_snapshot_report", Action: "capture", IdempotencyDigest: DigestIdempotencyKey("race-key"), RequestDigest: HashBytes([]byte("request"))}
	const callers = 20
	results := make(chan ReservationResult, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			result, err := store.Reserve(ctx, request, base)
			results <- result
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	var owner ReservationResult
	owners, inProgress := 0, 0
	for range callers {
		result := <-results
		if err := <-errs; err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		switch result.Disposition {
		case ReservationOwned:
			owners++
			owner = result
		case ReservationInProgress:
			inProgress++
		default:
			t.Fatalf("unexpected disposition %#v", result)
		}
	}
	if owners != 1 || inProgress != callers-1 {
		t.Fatalf("owners=%d in_progress=%d", owners, inProgress)
	}
	if changed, err := store.ExpireStaleReservations(ctx, base.Add(61*time.Second)); err != nil || changed != 1 {
		t.Fatalf("ExpireStaleReservations changed=%d err=%v", changed, err)
	}
	replay, err := store.Reserve(ctx, request, base.Add(62*time.Second))
	if err != nil || replay.Disposition != ReservationReplay || replay.State != ReservationStateExpired {
		t.Fatalf("expired replay = %#v err=%v owner=%#v", replay, err, owner)
	}
}

func TestC041StartedExpiredReservationFailsUnknownAndRequiresReconciliation(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := assuranceContext()
	base := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	request := ReservationRequest{ActorID: "actor-1", Tool: "mutation", Action: "confirm", IdempotencyDigest: DigestIdempotencyKey("started-key"), RequestDigest: HashBytes([]byte("request"))}
	owner, err := store.Reserve(ctx, request, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExecutionStarted(ctx, owner.RecordID, owner.OwnerNonce, base); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.ExpireStaleReservations(ctx, base.Add(61*time.Second)); err != nil || changed != 1 {
		t.Fatalf("ExpireStaleReservations changed=%d err=%v", changed, err)
	}
	replay, err := store.Reserve(ctx, request, base.Add(62*time.Second))
	if err != nil || replay.State != ReservationStateFailed {
		t.Fatalf("failed replay = %#v err=%v", replay, err)
	}
	var failure StructuredError
	if err := json.Unmarshal(replay.Envelope, &failure); err != nil || failure.Code != "idempotency_execution_unknown" || !failure.ReconciliationRequired || failure.Retryable {
		t.Fatalf("failure envelope = %#v err=%v", failure, err)
	}
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
