package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
)

const evidenceStorageOptInEnv = "NL_EVIDENCE_BLOB_DELIVERY"

type evidenceStorageAdmission interface {
	assurance.EvidenceStorage
	ValidatePrivateContainer(context.Context) error
}

type evidenceStorageFactory func(context.Context, assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error)

func configureProductionEvidenceStorage(ctx context.Context, cfg *config.Config, store *assurance.Store) error {
	return configureProductionEvidenceStorageWithFactory(ctx, cfg, store, newProductionEvidenceStorage)
}

func configureProductionEvidenceStorageWithFactory(ctx context.Context, cfg *config.Config, store *assurance.Store, factory evidenceStorageFactory) error {
	value, present := os.LookupEnv(evidenceStorageOptInEnv)
	if !present {
		return nil
	}
	enabled, err := backupMaintenanceOptIn(value)
	if err != nil {
		return errors.New("NL_EVIDENCE_BLOB_DELIVERY must be exactly true or false")
	}
	if !enabled {
		return nil
	}
	if ctx == nil || cfg == nil || store == nil || !cfg.AssuranceEnabled || cfg.DemoMode || cfg.AuthMode != config.AuthModeEntra || strings.TrimSpace(cfg.EntraTenantID) == "" || cfg.EntraTenantID != strings.TrimSpace(cfg.EntraTenantID) {
		return errors.New("azure evidence delivery requires non-demo Entra assurance and a trusted tenant")
	}
	if factory == nil {
		return errors.New("azure evidence storage factory unavailable")
	}
	serviceURL, err := requiredEvidenceEnv("NL_EVIDENCE_BLOB_SERVICE_URL")
	if err != nil {
		return err
	}
	if err := validateBackupAccountURL(serviceURL); err != nil {
		return fmt.Errorf("invalid evidence Blob endpoint: %w", err)
	}
	container, err := requiredEvidenceEnv("NL_EVIDENCE_BLOB_CONTAINER")
	if err != nil {
		return err
	}
	if !validBackupContainer(container) {
		return errors.New("NL_EVIDENCE_BLOB_CONTAINER is invalid")
	}
	environment, err := requiredEvidenceEnv("NL_EVIDENCE_ENVIRONMENT_DIGEST")
	if err != nil {
		return err
	}
	if !validBackupIdentifier(environment) {
		return errors.New("NL_EVIDENCE_ENVIRONMENT_DIGEST is invalid")
	}
	evidenceNamespace := blobNamespace{accountURL: serviceURL, container: container, environment: environment}
	for _, prefix := range []string{"NL_BACKUP", "NL_RESTORE"} {
		namespace, configured, err := optionalBlobNamespace(prefix)
		if err != nil {
			return err
		}
		if configured && samePhysicalBlobContainer(evidenceNamespace, namespace) {
			return fmt.Errorf("evidence Blob container must be physically separate from %s recovery storage", prefix)
		}
	}
	storage, err := factory(ctx, assurance.AzureEvidenceConfig{ServiceURL: serviceURL, ContainerName: container, TenantID: cfg.EntraTenantID, EnvironmentDigest: environment, Timeout: 30 * time.Second, MaxArtifactBytes: 64 << 20})
	if err != nil {
		return fmt.Errorf("initialize durable evidence storage: %w", err)
	}
	if storage == nil {
		return errors.New("initialize durable evidence storage: incomplete adapter")
	}
	if err := storage.ValidatePrivateContainer(ctx); err != nil {
		return fmt.Errorf("evidence Blob container must be observably private: %w", err)
	}
	store.SetEvidenceStorage(storage)
	return nil
}

func optionalBlobNamespace(prefix string) (blobNamespace, bool, error) {
	serviceURL := os.Getenv(prefix + "_BLOB_SERVICE_URL")
	container := os.Getenv(prefix + "_BLOB_CONTAINER")
	environment := os.Getenv(prefix + "_ENVIRONMENT_DIGEST")
	if serviceURL == "" && container == "" && environment == "" {
		return blobNamespace{}, false, nil
	}
	if serviceURL == "" || container == "" || environment == "" || strings.TrimSpace(serviceURL) != serviceURL || strings.TrimSpace(container) != container || strings.TrimSpace(environment) != environment {
		return blobNamespace{}, false, fmt.Errorf("%s Blob namespace must configure service URL, container, and environment together", prefix)
	}
	if err := validateBackupAccountURL(serviceURL); err != nil {
		return blobNamespace{}, false, fmt.Errorf("invalid %s Blob endpoint: %w", prefix, err)
	}
	if !validBackupContainer(container) {
		return blobNamespace{}, false, fmt.Errorf("%s_BLOB_CONTAINER is invalid", prefix)
	}
	if !validBackupIdentifier(environment) {
		return blobNamespace{}, false, fmt.Errorf("%s_ENVIRONMENT_DIGEST is invalid", prefix)
	}
	return blobNamespace{accountURL: serviceURL, container: container, environment: environment}, true, nil
}

func newProductionEvidenceStorage(ctx context.Context, cfg assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error) {
	return assurance.NewAzureEvidenceStorage(ctx, cfg)
}

func requiredEvidenceEnv(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}
