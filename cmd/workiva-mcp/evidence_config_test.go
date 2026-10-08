package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

type evidenceConfigFake struct {
	validateErr error
	validations int
}

func (f *evidenceConfigFake) Put(context.Context, string, string, []byte) (string, error) {
	return "opaque", nil
}
func (f *evidenceConfigFake) Read(context.Context, string) ([]byte, error) { return nil, nil }
func (f *evidenceConfigFake) Delete(context.Context, string) error         { return nil }
func (f *evidenceConfigFake) ValidatePrivateContainer(context.Context) error {
	f.validations++
	return f.validateErr
}

func TestProductionEvidenceStorageOptInValidatesAndRequiresPrivateContainer(t *testing.T) {
	for _, name := range []string{evidenceStorageOptInEnv, "NL_EVIDENCE_BLOB_SERVICE_URL", "NL_EVIDENCE_BLOB_CONTAINER", "NL_EVIDENCE_ENVIRONMENT_DIGEST"} {
		t.Setenv(name, "")
	}
	// Disabled delivery does not instantiate any storage dependency.
	t.Setenv(evidenceStorageOptInEnv, "false")
	if err := configureProductionEvidenceStorageWithFactory(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("disabled delivery required a dependency: %v", err)
	}

	db, err := sqlitedb.Open(t.TempDir() + "/evidence.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}
	t.Setenv(evidenceStorageOptInEnv, "true")
	t.Setenv("NL_EVIDENCE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
	t.Setenv("NL_EVIDENCE_BLOB_CONTAINER", "evidence-private")
	t.Setenv("NL_EVIDENCE_ENVIRONMENT_DIGEST", "sandbox-a")
	fake := &evidenceConfigFake{}
	var got assurance.AzureEvidenceConfig
	factory := func(_ context.Context, value assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error) {
		got = value
		return fake, nil
	}
	if err := configureProductionEvidenceStorageWithFactory(context.Background(), cfg, store, factory); err != nil {
		t.Fatalf("configure durable evidence: %v", err)
	}
	if got.TenantID != cfg.EntraTenantID || got.ContainerName != "evidence-private" || got.EnvironmentDigest != "sandbox-a" || got.MaxArtifactBytes <= 0 || fake.validations != 1 {
		t.Fatalf("evidence config/policy validation = %+v, validations=%d", got, fake.validations)
	}

	denied := &evidenceConfigFake{validateErr: errors.New("policy unavailable")}
	factory = func(context.Context, assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error) {
		return denied, nil
	}
	if err := configureProductionEvidenceStorageWithFactory(context.Background(), cfg, store, factory); err == nil {
		t.Fatal("unobservable private-container policy admitted evidence delivery")
	}
	if denied.validations != 1 {
		t.Fatalf("private policy checks=%d", denied.validations)
	}
}

func TestProductionEvidenceStorageRejectsCredentialEndpointBeforeFactory(t *testing.T) {
	t.Setenv(evidenceStorageOptInEnv, "true")
	t.Setenv("NL_EVIDENCE_BLOB_SERVICE_URL", "https://user:secret@account.blob.core.windows.net/?sig=token")
	t.Setenv("NL_EVIDENCE_BLOB_CONTAINER", "evidence-private")
	t.Setenv("NL_EVIDENCE_ENVIRONMENT_DIGEST", "sandbox-a")
	db, err := sqlitedb.Open(t.TempDir() + "/evidence.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}
	called := false
	factory := func(context.Context, assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error) {
		called = true
		return &evidenceConfigFake{}, nil
	}
	if err := configureProductionEvidenceStorageWithFactory(context.Background(), cfg, store, factory); err == nil {
		t.Fatal("credential-bearing evidence Blob endpoint was accepted")
	}
	if called {
		t.Fatal("storage factory ran before endpoint validation")
	}
}

func TestProductionEvidenceStorageRejectsBackupOrRestoreContainerReuseBeforeFactory(t *testing.T) {
	for _, name := range []string{
		evidenceStorageOptInEnv, "NL_EVIDENCE_BLOB_SERVICE_URL", "NL_EVIDENCE_BLOB_CONTAINER", "NL_EVIDENCE_ENVIRONMENT_DIGEST",
		"NL_BACKUP_BLOB_SERVICE_URL", "NL_BACKUP_BLOB_CONTAINER", "NL_BACKUP_ENVIRONMENT_DIGEST",
		"NL_RESTORE_BLOB_SERVICE_URL", "NL_RESTORE_BLOB_CONTAINER", "NL_RESTORE_ENVIRONMENT_DIGEST",
	} {
		t.Setenv(name, "")
	}
	t.Setenv(evidenceStorageOptInEnv, "true")
	t.Setenv("NL_EVIDENCE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
	t.Setenv("NL_EVIDENCE_BLOB_CONTAINER", "shared-physical-container")
	t.Setenv("NL_EVIDENCE_ENVIRONMENT_DIGEST", "env-a")
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}

	for _, namespace := range []string{"backup", "restore"} {
		t.Run(namespace, func(t *testing.T) {
			t.Setenv("NL_"+strings.ToUpper(namespace)+"_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
			t.Setenv("NL_"+strings.ToUpper(namespace)+"_BLOB_CONTAINER", "shared-physical-container")
			t.Setenv("NL_"+strings.ToUpper(namespace)+"_ENVIRONMENT_DIGEST", "different-env")
			db, err := sqlitedb.Open(t.TempDir() + "/evidence.db")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			store, err := assurance.NewWithDB(db)
			if err != nil {
				t.Fatal(err)
			}
			factoryCalled := false
			err = configureProductionEvidenceStorageWithFactory(context.Background(), cfg, store, func(context.Context, assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error) {
				factoryCalled = true
				return &evidenceConfigFake{}, nil
			})
			if err == nil || factoryCalled {
				t.Fatalf("physical-container collision err=%v factoryCalled=%v; want pre-factory refusal", err, factoryCalled)
			}
		})
	}
}

func TestProductionEvidenceStorageRejectsPartialRecoveryNamespaceBeforeFactory(t *testing.T) {
	for _, name := range []string{
		evidenceStorageOptInEnv, "NL_EVIDENCE_BLOB_SERVICE_URL", "NL_EVIDENCE_BLOB_CONTAINER", "NL_EVIDENCE_ENVIRONMENT_DIGEST",
		"NL_BACKUP_BLOB_SERVICE_URL", "NL_BACKUP_BLOB_CONTAINER", "NL_BACKUP_ENVIRONMENT_DIGEST",
		"NL_RESTORE_BLOB_SERVICE_URL", "NL_RESTORE_BLOB_CONTAINER", "NL_RESTORE_ENVIRONMENT_DIGEST",
	} {
		t.Setenv(name, "")
	}
	t.Setenv(evidenceStorageOptInEnv, "true")
	t.Setenv("NL_EVIDENCE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
	t.Setenv("NL_EVIDENCE_BLOB_CONTAINER", "evidence-private")
	t.Setenv("NL_EVIDENCE_ENVIRONMENT_DIGEST", "env-a")
	t.Setenv("NL_BACKUP_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}
	db, err := sqlitedb.Open(t.TempDir() + "/evidence.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	store, err := assurance.NewWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	factoryCalled := false
	err = configureProductionEvidenceStorageWithFactory(context.Background(), cfg, store, func(context.Context, assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error) {
		factoryCalled = true
		return &evidenceConfigFake{}, nil
	})
	if err == nil || factoryCalled {
		t.Fatalf("partial backup namespace err=%v factoryCalled=%v; want pre-factory refusal", err, factoryCalled)
	}
}

func TestProductionEvidenceStorageAllowsDistinctPhysicalNamespaces(t *testing.T) {
	for _, name := range []string{
		evidenceStorageOptInEnv, "NL_EVIDENCE_BLOB_SERVICE_URL", "NL_EVIDENCE_BLOB_CONTAINER", "NL_EVIDENCE_ENVIRONMENT_DIGEST",
		"NL_BACKUP_BLOB_SERVICE_URL", "NL_BACKUP_BLOB_CONTAINER", "NL_BACKUP_ENVIRONMENT_DIGEST",
		"NL_RESTORE_BLOB_SERVICE_URL", "NL_RESTORE_BLOB_CONTAINER", "NL_RESTORE_ENVIRONMENT_DIGEST",
	} {
		t.Setenv(name, "")
	}
	t.Setenv(evidenceStorageOptInEnv, "true")
	t.Setenv("NL_EVIDENCE_BLOB_SERVICE_URL", "https://account.blob.core.windows.net")
	t.Setenv("NL_EVIDENCE_BLOB_CONTAINER", "evidence-private")
	t.Setenv("NL_EVIDENCE_ENVIRONMENT_DIGEST", "env-a")
	cfg := &config.Config{AuthMode: config.AuthModeEntra, AssuranceEnabled: true, EntraTenantID: "tenant-a"}

	cases := []struct{ name, serviceURL, container string }{
		{name: "different container", serviceURL: "https://account.blob.core.windows.net", container: "backup-private"},
		{name: "same container name on another account", serviceURL: "https://other.blob.core.windows.net", container: "evidence-private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NL_BACKUP_BLOB_SERVICE_URL", tc.serviceURL)
			t.Setenv("NL_BACKUP_BLOB_CONTAINER", tc.container)
			t.Setenv("NL_BACKUP_ENVIRONMENT_DIGEST", "env-a")
			t.Setenv("NL_RESTORE_BLOB_SERVICE_URL", "")
			t.Setenv("NL_RESTORE_BLOB_CONTAINER", "")
			t.Setenv("NL_RESTORE_ENVIRONMENT_DIGEST", "")
			db, err := sqlitedb.Open(t.TempDir() + "/evidence.db")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			store, err := assurance.NewWithDB(db)
			if err != nil {
				t.Fatal(err)
			}
			factoryCalled := false
			err = configureProductionEvidenceStorageWithFactory(context.Background(), cfg, store, func(context.Context, assurance.AzureEvidenceConfig) (evidenceStorageAdmission, error) {
				factoryCalled = true
				return &evidenceConfigFake{}, nil
			})
			if err != nil || !factoryCalled {
				t.Fatalf("distinct physical namespace err=%v factoryCalled=%v", err, factoryCalled)
			}
		})
	}
}
