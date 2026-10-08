package assurance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/dantalabs/northern-lights/internal/identity"
)

type nilBodyBlobTransport struct {
	base     policy.Transporter
	nilBody  atomic.Bool
	getCount atomic.Int64
}

type httpRoundTripperAdapter struct{ http.RoundTripper }

func (t httpRoundTripperAdapter) Do(request *http.Request) (*http.Response, error) {
	return t.RoundTrip(request)
}

func (t *nilBodyBlobTransport) Do(request *http.Request) (*http.Response, error) {
	response, err := t.base.Do(request)
	if err != nil || response == nil {
		return response, err
	}
	if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/evidence/") {
		t.getCount.Add(1)
		if t.nilBody.Load() && response.StatusCode >= 200 && response.StatusCode < 300 {
			if response.Body != nil {
				_ = response.Body.Close()
			}
			response.Body = nil
		}
	}
	return response, nil
}

func TestAzureEvidenceCleanupNilSuccessfulGetBodyStaysPending(t *testing.T) {
	store, db := openTestStore(t)
	fake := &evidenceBlobFake{objects: make(map[string][]byte)}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	defer server.Close()
	transport := &nilBodyBlobTransport{base: httpRoundTripperAdapter{http.DefaultTransport}}
	client, err := azblob.NewClientWithNoCredential(server.URL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{
		Retry: policy.RetryOptions{MaxRetries: -1}, Transport: transport,
	}})
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewAzureEvidenceStorageWithClient(context.Background(), AzureEvidenceConfig{
		ServiceURL: server.URL, ContainerName: "evidence-private", TenantID: testTenant,
		EnvironmentDigest: "sandbox-a", Timeout: time.Second, MaxArtifactBytes: 1024,
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := evidenceTrustedContext(testTenant)
	ref, err := storage.Put(ctx, "subject.json", "application/json", []byte(`{"record":"owned"}`))
	if err != nil {
		t.Fatal(err)
	}
	known, err := decodeAzureEvidenceReference(storage, ref)
	if err != nil {
		t.Fatal(err)
	}
	unknownRef, err := encodeAzureEvidenceReference(storage, azureEvidenceReference{ObjectID: known.ObjectID, SHA256: known.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	now := formatTimestamp(time.Now().UTC())
	for i, candidate := range []string{ref, unknownRef} {
		if _, err := db.Exec(`INSERT INTO assurance_evidence_cleanup (tenant_id, cleanup_id, record_id, storage_reference, state, error, created_at, updated_at) VALUES (?, ?, ?, ?, 'reconciliation_required', '', ?, ?)`, testTenant, []string{"known-body-missing", "unknown-body-missing"}[i], "nil-body-test", candidate, now, now); err != nil {
			t.Fatal(err)
		}
	}
	transport.nilBody.Store(true)
	admin := identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: testTenant, ObjectID: "cleanup-admin", Permissions: []identity.Permission{identity.PermissionTenantAdmin},
	})
	result, err := store.ReconcileEvidenceCleanup(admin, storage, time.Now().UTC(), 10)
	if err == nil || result.Attempted != 2 || result.Pending != 2 || result.Deleted != 0 {
		t.Fatalf("nil successful GET body reconciliation=%+v err=%v; want both rows pending", result, err)
	}
	for _, id := range []string{"known-body-missing", "unknown-body-missing"} {
		if state := cleanupStateFor(t, db, id); state != "reconciliation_required" {
			t.Errorf("cleanup %s state=%q; malformed successful GET must not prove absence", id, state)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deletes != 0 || len(fake.objects) != 1 || transport.getCount.Load() < 2 {
		t.Fatalf("malformed successful GET caused false deletion: GET=%d DELETE=%d objects=%d", transport.getCount.Load(), fake.deletes, len(fake.objects))
	}
}
