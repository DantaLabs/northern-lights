package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/transfer"
	"github.com/google/uuid"
)

// restoreAdmission is deliberately separate from the ordinary Wave 2 opener.
// All selection and rollback evidence must come from the operator, not the Blob
// manifest being selected. No application handle exists until this returns.
type restoreAdmission func(context.Context, *assurance.AzureBackupTransport, string, string, assurance.RestoreOptions, transfer.FenceInventory, string) (assurance.RestoreResult, transfer.RestoreScanResult, error)

var startupRestoreAdmission restoreAdmission = transfer.RestoreSelectedEnvelopeAndScan
var startupBackupTransport = assurance.NewAzureBackupTransport
var startupFenceInventory = transfer.NewAzureBlobFence
var startupPrivateContainerAdmission = func(ctx context.Context, backup *assurance.AzureBackupTransport) error {
	if backup == nil {
		return errors.New("startup restore: backup transport unavailable")
	}
	return backup.ValidatePrivateContainer(ctx)
}

func restoreOptIn() bool { return os.Getenv("NL_STARTUP_RESTORE_ADMISSION") != "" }

// blockedStartup has no application database or transfer service. It serves
// only a liveness response; POST and readiness remain unavailable.
func blockedStartup() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.Error(w, "startup restore admission failed", http.StatusServiceUnavailable)
	})
}

func admitStartupRestore(ctx context.Context, cfg *config.Config) (*transfer.AzureBlobFence, error) {
	if os.Getenv("NL_STARTUP_RESTORE_ADMISSION") != "true" {
		return nil, errors.New("startup restore: opt-in must be exactly true")
	}
	if cfg == nil || !cfg.AssuranceEnabled || cfg.AuthMode != config.AuthModeEntra || cfg.EntraTenantID == "" || cfg.DBPath == "" || cfg.DemoMode {
		return nil, errors.New("startup restore: verified Entra tenant, assurance, non-demo mode and database path required")
	}
	get := func(key string) (string, error) {
		value := os.Getenv(key)
		if value == "" || strings.TrimSpace(value) != value {
			return "", fmt.Errorf("startup restore: %s is required", key)
		}
		return value, nil
	}
	url, err := get("NL_RESTORE_BLOB_SERVICE_URL")
	if err != nil {
		return nil, err
	}
	if err := validateBackupAccountURL(url); err != nil {
		return nil, errors.New("startup restore: invalid Azure Blob account endpoint")
	}
	container, err := get("NL_RESTORE_BLOB_CONTAINER")
	if err != nil {
		return nil, err
	}
	if !validBackupContainer(container) {
		return nil, errors.New("startup restore: invalid Blob container")
	}
	environment, err := get("NL_RESTORE_ENVIRONMENT_DIGEST")
	if err != nil {
		return nil, err
	}
	if !validBackupIdentifier(environment) {
		return nil, errors.New("startup restore: invalid environment identifier")
	}
	id, err := get("NL_RESTORE_ENVELOPE_ID")
	if err != nil {
		return nil, err
	}
	if !validBackupIdentifier(id) {
		return nil, errors.New("startup restore: invalid envelope identifier")
	}
	expectedHash, err := get("NL_RESTORE_DATABASE_SHA256")
	if err != nil {
		return nil, err
	}
	hash, err := hex.DecodeString(expectedHash)
	if err != nil || len(hash) != 32 || strings.ToLower(expectedHash) != expectedHash {
		return nil, errors.New("startup restore: expected database SHA-256 must be lowercase hex")
	}
	minimum, err := get("NL_RESTORE_MIN_AUDIT_SEQUENCE")
	if err != nil {
		return nil, err
	}
	sequence, err := strconv.ParseInt(minimum, 10, 64)
	if err != nil || sequence < 1 {
		return nil, errors.New("startup restore: positive trusted audit sequence required")
	}
	checkpointPublicHex, err := get("NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX")
	if err != nil {
		return nil, err
	}
	checkpointPublicBytes, err := hex.DecodeString(checkpointPublicHex)
	if err != nil || len(checkpointPublicBytes) != ed25519.PublicKeySize || strings.ToLower(checkpointPublicHex) != checkpointPublicHex {
		return nil, errors.New("startup restore: checkpoint public key must be lowercase Ed25519 hex")
	}
	checkpointID, err := get("NL_RESTORE_CHECKPOINT_ID")
	if err != nil {
		return nil, err
	}
	if checkpointID != "audit-terminal" {
		parsedID, parseErr := uuid.Parse(checkpointID)
		if parseErr != nil || parsedID.String() != checkpointID {
			return nil, errors.New("startup restore: checkpoint ID must be a canonical UUID or audit-terminal")
		}
	}
	var minimumCreatedAt time.Time
	if rawMinimumCreatedAt := os.Getenv("NL_RESTORE_MIN_CREATED_AT"); rawMinimumCreatedAt != "" {
		if strings.TrimSpace(rawMinimumCreatedAt) != rawMinimumCreatedAt {
			return nil, errors.New("startup restore: minimum created-at must be RFC3339Nano")
		}
		minimumCreatedAt, err = time.Parse(time.RFC3339Nano, rawMinimumCreatedAt)
		if err != nil {
			return nil, errors.New("startup restore: minimum created-at must be RFC3339Nano")
		}
	}
	key := assurance.SigningKey{}
	publicHex, hmacHex := os.Getenv("NL_RESTORE_ED25519_PUBLIC_KEY_HEX"), os.Getenv("NL_RESTORE_HMAC_KEY_HEX")
	if (publicHex == "") == (hmacHex == "") {
		return nil, errors.New("startup restore: exactly one signed backup verification key required")
	}
	if publicHex != "" {
		raw, err := hex.DecodeString(publicHex)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, errors.New("startup restore: invalid Ed25519 public key")
		}
		key.Ed25519Public = ed25519.PublicKey(raw)
	} else {
		raw, err := hex.DecodeString(hmacHex)
		if err != nil || len(raw) < 32 {
			return nil, errors.New("startup restore: invalid HMAC verification key")
		}
		key.HMAC = raw
	}
	opts := assurance.RestoreOptions{
		ExpectedTenantID: cfg.EntraTenantID, SigningKey: key,
		CheckpointPublicKey: ed25519.PublicKey(checkpointPublicBytes), CheckpointID: checkpointID,
		MinimumCreatedAt:       minimumCreatedAt,
		ExpectedDatabaseSHA256: expectedHash, MinimumAuditSequence: sequence,
		RequireFenceHighWater: true,
	}
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	backup, err := startupBackupTransport(work, assurance.AzureBackupConfig{ServiceURL: url, ContainerName: container, TenantID: cfg.EntraTenantID, EnvironmentDigest: environment, Timeout: 90 * time.Second, MaxDatabaseBytes: 2 << 30})
	if err != nil {
		return nil, fmt.Errorf("startup restore: backup transport: %w", err)
	}
	if backup == nil {
		return nil, errors.New("startup restore: backup transport unavailable")
	}
	if startupPrivateContainerAdmission == nil {
		return nil, errors.New("startup restore: private container admission unavailable")
	}
	if err := startupPrivateContainerAdmission(work, backup); err != nil {
		return nil, fmt.Errorf("startup restore: private container validation failed: %w", err)
	}
	fence, err := startupFenceInventory(work, transfer.AzureFenceConfig{ServiceURL: url, ContainerName: container, TenantID: cfg.EntraTenantID, EnvironmentDigest: environment, Timeout: 90 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("startup restore: fence inventory: %w", err)
	}
	restored, scan, err := startupRestoreAdmission(work, backup, id, cfg.DBPath, opts, fence, environment)
	if err != nil {
		return nil, fmt.Errorf("startup restore admission: %w", err)
	}
	if !restored.Ready || !scan.Ready {
		return nil, fmt.Errorf("startup restore: fence join refused readiness (%d findings, %d quarantined)", scan.Findings, scan.Quarantined)
	}
	return fence, nil
}
