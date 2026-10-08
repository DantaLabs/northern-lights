package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

// RestoreSelectedEnvelopeAndScan is the bounded restore admission operation.
// The selected signed envelope must carry an external fence high-water mark;
// a local SQLite restore alone is never sufficient to open transfer readiness.
// The returned DB path remains quarantined on any failure. No provider request
// or poll is made by this function.
func RestoreSelectedEnvelopeAndScan(ctx context.Context, backup *assurance.AzureBackupTransport, envelopeID, destination string, opts assurance.RestoreOptions, inventory FenceInventory, environmentDigest string) (assurance.RestoreResult, RestoreScanResult, error) {
	if ctx == nil || backup == nil || inventory == nil || environmentDigest == "" {
		return assurance.RestoreResult{}, RestoreScanResult{}, errors.New("transfer: restore dependencies required")
	}
	restored, err := backup.RestoreEnvelope(ctx, envelopeID, destination, opts)
	if err != nil {
		return assurance.RestoreResult{}, RestoreScanResult{}, err
	}
	restored.Ready = false
	mark := restored.Manifest.FenceHighWater
	if mark == nil || mark.EnvironmentDigest != environmentDigest || mark.TenantDigest != digest(opts.ExpectedTenantID) {
		return restored, RestoreScanResult{}, errors.New("transfer: signed backup lacks matching external fence high-water")
	}
	db, err := sqlitedb.Open(restored.DatabasePath)
	if err != nil {
		return restored, RestoreScanResult{}, fmt.Errorf("transfer: open restored database: %w", err)
	}
	defer func() { _ = db.Close() }()
	log, err := audit.NewWithDB(db)
	if err != nil {
		return restored, RestoreScanResult{}, err
	}
	as, err := assurance.NewWithDB(db)
	if err != nil {
		return restored, RestoreScanResult{}, err
	}
	store, err := NewWithDB(db)
	if err != nil {
		return restored, RestoreScanResult{}, err
	}
	scanCtx := identity.ContextWithPrincipal(ctx, identity.Principal{TenantID: opts.ExpectedTenantID})
	scan, err := ScanAndJoinRestore(scanCtx, store, as, log, inventory, RestoreScanOptions{TenantID: opts.ExpectedTenantID, EnvironmentDigest: environmentDigest, HighWater: *mark})
	if err != nil {
		return restored, scan, err
	}
	restored.Ready = scan.Ready
	return restored, scan, nil
}
