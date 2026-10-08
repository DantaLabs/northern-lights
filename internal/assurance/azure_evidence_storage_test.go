package assurance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/dantalabs/northern-lights/internal/identity"
)

func TestAzureEvidenceStorageRequiresTrustedTenantAndReturnsOpaqueBoundReference(t *testing.T) {
	fake := &evidenceBlobFake{objects: make(map[string][]byte)}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{
		ServiceURL: server.URL, ContainerName: "evidence-private",
		TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 1024,
	}, client)
	if err != nil {
		t.Fatalf("construct evidence adapter: %v", err)
	}
	if err := storage.ValidatePrivateContainer(context.Background()); err != nil {
		t.Fatalf("private container rejected: %v", err)
	}

	ctx := evidenceTrustedContext(testTenant)
	data := []byte(`{"sensitive":"payload"}`)
	ref, err := storage.Put(ctx, "manifest.json", "application/json", data)
	if err != nil {
		t.Fatalf("put evidence: %v", err)
	}
	if ref == "" || bytes.Contains([]byte(ref), []byte(testTenant)) || bytes.Contains([]byte(ref), []byte("manifest.json")) || bytes.Contains([]byte(ref), []byte("sensitive")) {
		t.Fatalf("reference is not opaque: %q", ref)
	}
	got, err := storage.Read(ctx, ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read evidence = %q, %v", got, err)
	}
	wrongTenant := evidenceTrustedContext("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if _, err := storage.Read(wrongTenant, ref); err == nil {
		t.Fatal("cross-tenant read was accepted")
	}
	if err := storage.Delete(wrongTenant, ref); err == nil {
		t.Fatal("cross-tenant delete was accepted")
	}
	if err := storage.Delete(ctx, ref); err != nil {
		t.Fatalf("delete evidence: %v", err)
	}
	if _, err := storage.Read(ctx, ref); err == nil {
		t.Fatal("deleted evidence remained readable")
	}

	fake.mu.Lock()
	if fake.putConditions != 1 || fake.gets != 4 || fake.deletes != 1 {
		t.Fatalf("Blob calls put conditions=%d GET=%d DELETE=%d", fake.putConditions, fake.gets, fake.deletes)
	}
	if fake.policyGets != 5 {
		t.Fatalf("container policy checks=%d, want startup plus each storage operation", fake.policyGets)
	}
	if fake.putIfNoneMatch != "*" {
		t.Fatalf("evidence upload was not create-only: If-None-Match=%q", fake.putIfNoneMatch)
	}
	if fake.deleteIfMatch != `"evidence-1"` {
		t.Fatalf("evidence delete was not bound to the verified ETag: If-Match=%q", fake.deleteIfMatch)
	}
	fake.mu.Unlock()
	if err := storage.ValidatePrivateContainer(context.Background()); err != nil {
		t.Fatalf("private container recheck: %v", err)
	}
	fake.mu.Lock()
	fake.publicAccess = "blob"
	fake.mu.Unlock()
	if err := storage.ValidatePrivateContainer(context.Background()); err == nil {
		t.Fatal("public container policy was admitted")
	}
	fake.mu.Lock()
	fake.publicAccess = ""
	fake.policyStatus = http.StatusServiceUnavailable
	fake.mu.Unlock()
	if err := storage.ValidatePrivateContainer(context.Background()); err == nil {
		t.Fatal("unobservable container policy was admitted")
	}
	if _, err := storage.Read(ctx, ref); err == nil {
		t.Fatal("operation proceeded when container policy was unobservable")
	}
	fake.mu.Lock()
	if fake.gets != 4 || fake.putConditions != 1 || fake.policyGets != 9 {
		t.Fatalf("private-policy denial reached artifact storage: data GET=%d PUT=%d policy GET=%d", fake.gets, fake.putConditions, fake.policyGets)
	}
	fake.mu.Unlock()
}

func TestAzureEvidenceStorageRejectsUntrustedCanceledWrongEnvironmentAndMalformedReferences(t *testing.T) {
	fake := &evidenceBlobFake{objects: make(map[string][]byte)}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := AzureEvidenceConfig{ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 32}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := evidenceTrustedContext(testTenant)
	ref, err := storage.Put(ctx, "subject.json", "application/json", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	wrongEnvCfg := cfg
	wrongEnvCfg.EnvironmentDigest = "sandbox-b"
	wrongEnv, err := NewAzureEvidenceStorageWithClient(context.Background(), wrongEnvCfg, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{ref + "x", "azev1.not-valid", "https://account.blob.core.windows.net/c?sig=secret", "../../private.db"} {
		if _, err := storage.Read(ctx, invalid); err == nil {
			t.Errorf("malformed/path reference %q accepted", invalid)
		}
	}
	if _, err := wrongEnv.Read(ctx, ref); err == nil {
		t.Fatal("reference from another environment accepted")
	}
	noPermission := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "actor-without-permission"})
	if _, err := storage.Put(noPermission, "subject.json", "application/json", []byte("x")); err == nil {
		t.Fatal("unauthorized Put accepted")
	}
	if _, err := storage.Read(noPermission, ref); err == nil {
		t.Fatal("unauthorized Read accepted")
	}
	if err := storage.Delete(noPermission, ref); err == nil {
		t.Fatal("unauthorized Delete accepted")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := storage.Read(canceled, ref); err == nil {
		t.Fatal("canceled Read accepted")
	}
	var nilStorage *AzureEvidenceStorage
	if _, err := nilStorage.Put(ctx, "subject.json", "application/json", []byte("x")); err == nil {
		t.Fatal("nil storage Put accepted")
	}
	if _, err := nilStorage.Read(ctx, ref); err == nil {
		t.Fatal("nil storage Read accepted")
	}
	if err := nilStorage.Delete(ctx, ref); err == nil {
		t.Fatal("nil storage Delete accepted")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.putConditions != 1 || fake.gets != 1 || fake.deletes != 0 {
		t.Fatalf("rejected calls reached Blob: put=%d get=%d delete=%d", fake.putConditions, fake.gets, fake.deletes)
	}
}

func TestAzureEvidenceStorageReturnsKnownReferenceForUnverifiedUploadWithoutHiddenDelete(t *testing.T) {
	fake := &evidenceBlobFake{objects: make(map[string][]byte), corruptGets: true}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 64}, client)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := storage.Put(evidenceTrustedContext(testTenant), "subject.json", "application/json", []byte(`{"x":1}`))
	if err == nil || ref == "" {
		t.Fatalf("unverified upload ref=%q err=%v, want known ref plus error", ref, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.putConditions != 1 || fake.gets != 1 || fake.deletes != 0 || len(fake.objects) != 1 {
		t.Fatalf("failed readback was hidden/retried: PUT=%d GET=%d DELETE=%d objects=%d", fake.putConditions, fake.gets, fake.deletes, len(fake.objects))
	}
}

func TestAzureEvidenceStorageReadDeleteFailClosedOnETagDigestAndOversize(t *testing.T) {
	fake := &evidenceBlobFake{objects: make(map[string][]byte)}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 32}, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := evidenceTrustedContext(testTenant)
	ref, err := storage.Put(ctx, "subject.json", "application/json", []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.changedETagGets = true
	fake.mu.Unlock()
	if _, err := storage.Read(ctx, ref); err == nil {
		t.Fatal("ETag-mutated object read accepted")
	}
	if err := storage.Delete(ctx, ref); err == nil {
		t.Fatal("ETag-mutated object delete accepted")
	}
	fake.mu.Lock()
	fake.changedETagGets = false
	fake.oversizeGets = true
	fake.mu.Unlock()
	if _, err := storage.Read(ctx, ref); err == nil {
		t.Fatal("oversized response accepted")
	}
	if err := storage.Delete(ctx, ref); err == nil {
		t.Fatal("oversized object delete accepted")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deletes != 0 || len(fake.objects) != 1 {
		t.Fatalf("unverified object was deleted: deletes=%d objects=%d", fake.deletes, len(fake.objects))
	}
}

func TestAzureEvidenceStorageMissingUploadETagRequiresAdminResolution(t *testing.T) {
	fake := &evidenceBlobFake{objects: make(map[string][]byte), omitPutETag: true}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 32}, client)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := storage.Put(evidenceTrustedContext(testTenant), "subject.json", "application/json", []byte(`{"x":1}`))
	if err == nil || ref == "" {
		t.Fatalf("missing upload ETag ref=%q err=%v, want opaque cleanup ref plus fail-closed error", ref, err)
	}
	fake.mu.Lock()
	if fake.deletes != 0 || len(fake.objects) != 1 {
		t.Fatalf("Put performed hidden cleanup: deletes=%d objects=%d", fake.deletes, len(fake.objects))
	}
	fake.mu.Unlock()
	if err := storage.Delete(evidenceExportOnlyContext(testTenant), ref); err == nil {
		t.Fatal("exporter-only caller resolved an unknown create version")
	}
	if err := storage.Delete(evidenceAdminContext(testTenant), ref); err != nil {
		t.Fatalf("admin could not resolve missing create ETag from verified object: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deletes != 1 || len(fake.objects) != 0 || fake.deleteIfMatch != `"evidence-1"` {
		t.Fatalf("admin resolution was not exact/conditional: deletes=%d objects=%d if-match=%q", fake.deletes, len(fake.objects), fake.deleteIfMatch)
	}
}

func TestAzureEvidenceStorageAdminUnknownVersionCleanupFailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		prepare    func(*testing.T, *evidenceBlobFake, *AzureEvidenceStorage, *azblob.Client, string) (*AzureEvidenceStorage, context.Context, bool)
		wantGet    int
		wantDelete int
		wantRemain bool
	}{
		{name: "absent is already resolved", prepare: func(t *testing.T, fake *evidenceBlobFake, storage *AzureEvidenceStorage, _ *azblob.Client, ref string) (*AzureEvidenceStorage, context.Context, bool) {
			parsed, err := decodeAzureEvidenceReference(storage, ref)
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			delete(fake.objects, "/evidence-private/"+storage.objectName(parsed.ObjectID))
			fake.mu.Unlock()
			return storage, evidenceAdminContext(testTenant), true
		}, wantGet: 1},
		{name: "corrupt body remains pending", prepare: func(_ *testing.T, fake *evidenceBlobFake, storage *AzureEvidenceStorage, _ *azblob.Client, _ string) (*AzureEvidenceStorage, context.Context, bool) {
			fake.mu.Lock()
			fake.corruptGets = true
			fake.mu.Unlock()
			return storage, evidenceAdminContext(testTenant), false
		}, wantGet: 1, wantRemain: true},
		{name: "missing observed ETag remains pending", prepare: func(_ *testing.T, fake *evidenceBlobFake, storage *AzureEvidenceStorage, _ *azblob.Client, _ string) (*AzureEvidenceStorage, context.Context, bool) {
			fake.mu.Lock()
			fake.omitGetETag = true
			fake.mu.Unlock()
			return storage, evidenceAdminContext(testTenant), false
		}, wantGet: 1, wantRemain: true},
		{name: "public container remains pending", prepare: func(_ *testing.T, fake *evidenceBlobFake, storage *AzureEvidenceStorage, _ *azblob.Client, _ string) (*AzureEvidenceStorage, context.Context, bool) {
			fake.mu.Lock()
			fake.publicAccess = "blob"
			fake.mu.Unlock()
			return storage, evidenceAdminContext(testTenant), false
		}, wantRemain: true},
		{name: "cross tenant remains pending", prepare: func(_ *testing.T, _ *evidenceBlobFake, storage *AzureEvidenceStorage, _ *azblob.Client, _ string) (*AzureEvidenceStorage, context.Context, bool) {
			return storage, evidenceAdminContext("22222222-2222-2222-2222-222222222222"), false
		}, wantRemain: true},
		{name: "cross environment remains pending", prepare: func(t *testing.T, _ *evidenceBlobFake, _ *AzureEvidenceStorage, client *azblob.Client, _ string) (*AzureEvidenceStorage, context.Context, bool) {
			otherEnv, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{ServiceURL: "http://127.0.0.1", ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-b", Timeout: time.Second, MaxArtifactBytes: 32}, client)
			if err != nil {
				t.Fatal(err)
			}
			return otherEnv, evidenceAdminContext(testTenant), false
		}, wantRemain: true},
		{name: "export permission cannot resolve unknown version", prepare: func(_ *testing.T, _ *evidenceBlobFake, storage *AzureEvidenceStorage, _ *azblob.Client, _ string) (*AzureEvidenceStorage, context.Context, bool) {
			return storage, evidenceExportOnlyContext(testTenant), false
		}, wantRemain: true},
		{name: "concurrent overwrite defeats conditional delete", prepare: func(_ *testing.T, fake *evidenceBlobFake, storage *AzureEvidenceStorage, _ *azblob.Client, _ string) (*AzureEvidenceStorage, context.Context, bool) {
			fake.mu.Lock()
			fake.overwriteBeforeDelete = true
			fake.mu.Unlock()
			return storage, evidenceAdminContext(testTenant), false
		}, wantGet: 1, wantDelete: 1, wantRemain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &evidenceBlobFake{objects: make(map[string][]byte), failAfterCommit: true}
			storage, client := newAmbiguousEvidenceStorage(t, fake)
			ref, putErr := storage.Put(evidenceExportOnlyContext(testTenant), "subject.json", "application/json", []byte(`{"x":1}`))
			if putErr == nil || ref == "" {
				t.Fatalf("wanted committed ambiguous create, ref=%q err=%v", ref, putErr)
			}
			targetStorage, ctx, wantAbsent := tc.prepare(t, fake, storage, client, ref)
			deleteErr := targetStorage.Delete(ctx, ref)
			if wantAbsent {
				if !errors.Is(deleteErr, ErrEvidenceNotFound) {
					t.Fatalf("absent object result=%v, want confirmed not-found", deleteErr)
				}
			} else if deleteErr == nil {
				t.Fatal("unverified, unauthorized, or changed object was deleted")
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.putConditions != 1 || fake.gets != tc.wantGet || fake.deletes != tc.wantDelete {
				t.Fatalf("unexpected operations: PUT=%d GET=%d conditional-delete-attempts=%d", fake.putConditions, fake.gets, fake.deletes)
			}
			gotRemain := len(fake.objects) != 0
			if gotRemain != tc.wantRemain {
				t.Fatalf("object presence mismatch: remain=%v objects=%d", gotRemain, len(fake.objects))
			}
			if tc.wantDelete > 0 && fake.deleteIfMatch != `"evidence-1"` {
				t.Fatalf("delete was not conditional on the observed version: If-Match=%q", fake.deleteIfMatch)
			}
		})
	}
}

func newAmbiguousEvidenceStorage(t *testing.T, fake *evidenceBlobFake) (*AzureEvidenceStorage, *azblob.Client) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 32}, client)
	if err != nil {
		t.Fatal(err)
	}
	return storage, client
}

func TestAzureEvidenceProductionConfigRejectsNoncanonicalAccountURLs(t *testing.T) {
	base := AzureEvidenceConfig{ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 1024}
	for _, endpoint := range []string{
		"https://account.blob.core.windows.net/?",
		"https://user:secret@account.blob.core.windows.net",
		"https://account.blob.core.windows.net/?sig=secret",
		"https://account.blob.core.windows.net/container",
		"https://nonazure.example",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := base
			cfg.ServiceURL = endpoint
			if err := validateAzureEvidenceConfig(cfg, true); err == nil {
				t.Fatalf("production constructor accepted noncanonical endpoint %q", endpoint)
			}
		})
	}
	cfg := base
	cfg.ServiceURL = "http://127.0.0.1:1234"
	if err := validateAzureEvidenceConfig(cfg, false); err != nil {
		t.Fatalf("test seam rejected local endpoint: %v", err)
	}
}

func TestAzureEvidenceStorageAmbiguousUploadResolvesOnlyThroughAdminReconciliation(t *testing.T) {
	fake := &evidenceBlobFake{objects: make(map[string][]byte), failAfterCommit: true}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant, EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 32}, client)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := storage.Put(evidenceTrustedContext(testTenant), "subject.json", "application/json", []byte(`{"x":1}`))
	if err == nil || ref == "" {
		t.Fatalf("ambiguous upload ref=%q err=%v; want bounded cleanup reference", ref, err)
	}
	exporter := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "exporter", Permissions: []identity.Permission{identity.PermissionEvidenceExport}})
	if err := storage.Delete(exporter, ref); err == nil {
		t.Fatal("exporter-only cleanup must not resolve an unknown Blob version")
	}
	if _, err := storage.Read(exporter, ref); err == nil {
		t.Fatal("ordinary Read accepted an unknown Blob version")
	}
	admin := identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: testTenant, ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
	if err := storage.Delete(admin, ref); err != nil {
		t.Fatalf("tenant admin could not reconcile exact ambiguous object: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.putConditions != 1 || fake.deletes != 1 || len(fake.objects) != 0 || fake.deleteIfMatch != `"evidence-1"` {
		t.Fatalf("ambiguous reconciliation was not a single verified conditional delete: puts=%d deletes=%d objects=%d if-match=%q", fake.putConditions, fake.deletes, len(fake.objects), fake.deleteIfMatch)
	}
}

func evidenceTrustedContext(tenant string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Permissions: []identity.Permission{identity.PermissionEvidenceExport, identity.PermissionTenantAdmin}})
}

func evidenceExportOnlyContext(tenant string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "exporter", Permissions: []identity.Permission{identity.PermissionEvidenceExport}})
}

func evidenceAdminContext(tenant string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin}})
}

type evidenceBlobFake struct {
	mu                    sync.Mutex
	objects               map[string][]byte
	putConditions         int
	putIfNoneMatch        string
	deleteIfMatch         string
	gets, deletes         int
	policyGets            int
	publicAccess          string
	policyStatus          int
	corruptGets           bool
	changedETagGets       bool
	oversizeGets          bool
	omitPutETag           bool
	omitGetETag           bool
	overwriteBeforeDelete bool
	currentETag           string
	failAfterCommit       bool
}

func (f *evidenceBlobFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("restype") == "container" {
		f.mu.Lock()
		access, status := f.publicAccess, f.policyStatus
		f.policyGets++
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if access != "" {
			w.Header().Set("x-ms-blob-public-access", access)
		}
		w.Header().Set("ETag", `"private"`)
		w.WriteHeader(status)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.putConditions++
		f.putIfNoneMatch = r.Header.Get("If-None-Match")
		if f.putIfNoneMatch != "*" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		f.objects[r.URL.Path] = body
		f.currentETag = `"evidence-1"`
		if f.failAfterCommit {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if !f.omitPutETag {
			w.Header().Set("ETag", `"evidence-1"`)
		}
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		f.gets++
		body, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.corruptGets {
			body = append(append([]byte(nil), body...), '!')
		}
		if f.oversizeGets {
			body = append(body, bytes.Repeat([]byte{'x'}, 40)...)
		}
		etag := f.currentETag
		if etag == "" {
			etag = `"evidence-1"`
		}
		if f.changedETagGets {
			etag = `"changed"`
		}
		if !f.omitGetETag {
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case http.MethodDelete:
		f.deletes++
		f.deleteIfMatch = r.Header.Get("If-Match")
		if f.overwriteBeforeDelete {
			f.objects[r.URL.Path] = []byte(`{"replacement":true}`)
			f.currentETag = `"evidence-2"`
			f.overwriteBeforeDelete = false
		}
		if _, ok := f.objects[r.URL.Path]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		currentETag := f.currentETag
		if currentETag == "" {
			currentETag = `"evidence-1"`
		}
		if f.deleteIfMatch != currentETag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
