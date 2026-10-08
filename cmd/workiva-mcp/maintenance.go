package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/google/uuid"
)

// maintenanceCoordinator owns the shared drain gate and production backup
// operation. Production may expose only the separately configured,
// authenticated maintenance endpoint; callers must reuse this coordinator
// rather than constructing a second gate against the shared database.
type maintenanceCoordinator struct {
	gate      *assurance.DrainGate
	backup    *assurance.BackupManager
	db        *sql.DB
	operation *productionBackupOperation
}

type productionHandler struct {
	http.Handler
	maintenance       *maintenanceCoordinator
	backupMaintenance http.Handler
}

func (h *productionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/maintenance/backup" && h.backupMaintenance != nil {
		h.backupMaintenance.ServeHTTP(w, r)
		return
	}
	h.Handler.ServeHTTP(w, r)
}

type backupMaintenanceRunner func(context.Context, identity.Principal) (sealedBackupReceipt, error)

type backupFence interface {
	CaptureHighWater(context.Context, string) (assurance.BackupFenceHighWater, error)
}
type backupBlob interface {
	ValidatePrivateContainer(context.Context) error
	UploadEnvelope(context.Context, string, assurance.RestoreOptions) (assurance.BlobBackupSeal, error)
}
type BackupOperationConfig struct {
	TenantID, EnvironmentDigest, TempParent string
	SigningKey                              assurance.SigningKey
	CheckpointKey                           ed25519.PrivateKey
	Audit                                   *audit.Log
	Fence                                   backupFence
	Blob                                    backupBlob
}
type sealedBackupReceipt struct {
	EnvelopeID          string `json:"envelope_id"`
	DatabaseSHA256      string `json:"database_sha256"`
	ManifestSHA256      string `json:"manifest_sha256"`
	DatabaseETag        string `json:"database_etag"`
	ManifestETag        string `json:"manifest_etag"`
	AuditHighWater      int64  `json:"audit_high_water"`
	CheckpointPublicKey string `json:"checkpoint_public_key"`
	CheckpointID        string `json:"checkpoint_id"`
	MinimumCreatedAt    string `json:"minimum_created_at"`
}
type productionBackupOperation struct {
	db     *sql.DB
	config BackupOperationConfig
}

func (m *maintenanceCoordinator) CreateProductionBackup(ctx context.Context, caller identity.Principal) (sealedBackupReceipt, error) {
	if m == nil || m.operation == nil || m.gate == nil || m.operation.db == nil {
		return sealedBackupReceipt{}, errors.New("backup operation unavailable")
	}
	cfg := m.operation.config
	principal, ok := identity.PrincipalFromContext(ctx)
	if !ok || caller.TenantID != principal.TenantID || caller.ObjectID != principal.ObjectID || !caller.HasPermission(identity.PermissionTenantAdmin) || principal.TenantID == "" || principal.TenantID != cfg.TenantID || strings.TrimSpace(principal.ObjectID) == "" || !principal.HasPermission(identity.PermissionTenantAdmin) {
		return sealedBackupReceipt{}, errors.New("backup authorization failed")
	}
	if len(cfg.CheckpointKey) != ed25519.PrivateKeySize || len(cfg.SigningKey.Ed25519Private) != ed25519.PrivateKeySize || cfg.Audit == nil || cfg.Fence == nil || cfg.Blob == nil || cfg.TempParent == "" || cfg.EnvironmentDigest == "" || !cfg.Audit.SharesDB(m.operation.db) {
		return sealedBackupReceipt{}, errors.New("backup operation configuration unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	release, err := m.gate.BeginDrain(bounded)
	if err != nil {
		return sealedBackupReceipt{}, err
	}
	keepClosed := false
	defer func() {
		// Always inspect the shared handle independently of the request deadline.
		// A failed/ambiguous check leaves admission closed on every exit path.
		if !sharedDBWritable(m.operation.db) {
			keepClosed = true
		}
		if !keepClosed {
			release()
		}
	}()
	if err = cfg.Blob.ValidatePrivateContainer(bounded); err != nil {
		return sealedBackupReceipt{}, errors.New("private backup container validation failed")
	}
	id := uuid.NewString()
	entry, err := cfg.Audit.Append(identity.ContextWithPrincipal(bounded, principal), audit.Entry{Actor: principal.ObjectID, Tool: "maintenance", Action: "backup.checkpoint", Target: id})
	if err != nil {
		return sealedBackupReceipt{}, errors.New("backup checkpoint audit failed")
	}
	var seq int64
	var hash string
	if err = m.operation.db.QueryRowContext(bounded, `SELECT seq,hash FROM audit_log WHERE tenant_id=? ORDER BY seq DESC LIMIT 1`, cfg.TenantID).Scan(&seq, &hash); err != nil || seq != entry.Seq || hash != entry.Hash {
		return sealedBackupReceipt{}, errors.New("backup checkpoint terminal audit mismatch")
	}
	signature := ed25519.Sign(cfg.CheckpointKey, []byte(strings.Join([]string{cfg.TenantID, fmt.Sprint(seq), hash, id}, "\x00")))
	if _, err = m.operation.db.ExecContext(bounded, `INSERT INTO assurance_checkpoints(tenant_id,checkpoint_id,checkpoint_kind,high_water_mark,checkpoint_hash,signature_hex,metadata_json,created_at) VALUES(?,?,'terminal',?,?,?,'{}',?)`, cfg.TenantID, id, fmt.Sprint(seq), hash, hex.EncodeToString(signature), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return sealedBackupReceipt{}, errors.New("backup checkpoint persistence failed")
	}
	high, err := cfg.Fence.CaptureHighWater(bounded, cfg.TenantID)
	if err != nil {
		return sealedBackupReceipt{}, errors.New("backup fence high-water capture failed")
	}
	if !validBackupHighWater(high, cfg.TenantID, cfg.EnvironmentDigest) {
		return sealedBackupReceipt{}, errors.New("backup fence high-water invalid")
	}
	dir, err := os.MkdirTemp(cfg.TempParent, "northern-lights-backup-")
	if err != nil {
		return sealedBackupReceipt{}, errors.New("private backup staging unavailable")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return sealedBackupReceipt{}, errors.New("private backup staging unavailable")
	}
	envelope, err := assurance.CreateBackup(bounded, m.operation.db, filepath.Join(dir, "envelope"), assurance.BackupOptions{TenantID: cfg.TenantID, SigningKey: cfg.SigningKey, CheckpointPublicKey: cfg.CheckpointKey.Public().(ed25519.PublicKey), CheckpointID: id, RequireAuditCheckpoint: true, FenceHighWater: &high})
	if err != nil {
		if !sharedDBWritable(m.operation.db) {
			keepClosed = true
		}
		return sealedBackupReceipt{}, fmt.Errorf("backup snapshot creation failed: %w", err)
	}
	minimumCreated, err := time.Parse(time.RFC3339Nano, envelope.Manifest.CreatedAt)
	if err != nil {
		if !sharedDBWritable(m.operation.db) {
			keepClosed = true
		}
		return sealedBackupReceipt{}, errors.New("backup snapshot timestamp invalid")
	}
	restoreTrust := assurance.RestoreOptions{ExpectedTenantID: cfg.TenantID, SigningKey: cfg.SigningKey, CheckpointPublicKey: cfg.CheckpointKey.Public().(ed25519.PublicKey), CheckpointID: id, MinimumCreatedAt: minimumCreated, MinimumAuditSequence: seq, ExpectedDatabaseSHA256: envelope.Manifest.DatabaseSHA256, RequireFenceHighWater: true}
	seal, err := cfg.Blob.UploadEnvelope(bounded, envelope.Directory, restoreTrust)
	if err != nil {
		if !sharedDBWritable(m.operation.db) {
			keepClosed = true
		}
		return sealedBackupReceipt{}, errors.New("backup upload or read-back failed; sealed local staging retained")
	}
	if !sharedDBWritable(m.operation.db) {
		keepClosed = true
		return sealedBackupReceipt{}, errors.New("backup sealed but SQLite write readiness could not be verified")
	}
	if err = os.RemoveAll(dir); err != nil {
		return sealedBackupReceipt{}, errors.New("backup sealed but local staging cleanup failed")
	}
	pub := cfg.CheckpointKey.Public().(ed25519.PublicKey)
	return sealedBackupReceipt{EnvelopeID: seal.EnvelopeID, DatabaseSHA256: seal.DatabaseSHA256, ManifestSHA256: seal.ManifestSHA256, DatabaseETag: seal.DatabaseETag, ManifestETag: seal.ManifestETag, AuditHighWater: seal.AuditHighWater, CheckpointPublicKey: hex.EncodeToString(pub), CheckpointID: id, MinimumCreatedAt: minimumCreated.Format(time.RFC3339Nano)}, nil
}

func validBackupHighWater(high assurance.BackupFenceHighWater, tenant, environment string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, high.CapturedAt)
	digest := assurance.HashBytes([]byte(tenant))
	return err == nil && !parsed.IsZero() && high.TenantDigest == hex.EncodeToString(digest[:]) && high.EnvironmentDigest == environment && high.Authority == "azure_blob_last_modified"
}

func sharedDBWritable(db *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var queryOnly int
	return db != nil && db.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly) == nil && queryOnly == 0
}

func newBackupMaintenanceEndpoint(verifier identity.TokenVerifier, tenant string, run backupMaintenanceRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if verifier == nil || run == nil || tenant == "" {
			http.Error(w, "maintenance unavailable", http.StatusServiceUnavailable)
			return
		}
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")) == "" || strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")) != strings.TrimPrefix(header, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		principal, err := verifier.Verify(r.Context(), strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if principal.TenantID != tenant || strings.TrimSpace(principal.ObjectID) == "" || !principal.HasPermission(identity.PermissionTenantAdmin) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ctx := identity.ContextWithPrincipal(r.Context(), principal)
		trusted, ok := identity.PrincipalFromContext(ctx)
		if !ok || trusted.TenantID != tenant || trusted.ObjectID != principal.ObjectID || !trusted.HasPermission(identity.PermissionTenantAdmin) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		receipt, err := run(bounded, trusted)
		if err != nil {
			http.Error(w, "backup failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(receipt)
	})
}

// newMaintenanceCoordinator creates the sole in-process maintenance owner for
// the shared database. Production route exposure is separately opt-in and
// installs the authenticated backup endpoint only after configuration checks.
func newMaintenanceCoordinator(db *sql.DB) *maintenanceCoordinator {
	gate := assurance.NewDrainGate()
	return &maintenanceCoordinator{gate: gate, backup: assurance.NewBackupManager(db, gate), db: db}
}

// CreateLocalBackup is the legacy local snapshot API. Production backup uses
// CreateProductionBackup so the signed checkpoint, external high-water,
// durable Blob receipt, and shared admission gate remain one operation.
func (m *maintenanceCoordinator) CreateLocalBackup(ctx context.Context, destination string, opts assurance.BackupOptions) (assurance.BackupEnvelope, error) {
	if m == nil || m.backup == nil {
		return assurance.BackupEnvelope{}, errors.New("backup coordinator unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return m.backup.Create(bounded, destination, opts)
}
