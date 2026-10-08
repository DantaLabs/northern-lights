package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type fakeAzureBlob struct {
	body []byte
	etag string
}

type fakeAzureBlobServer struct {
	mu sync.Mutex

	blobs   map[string]fakeAzureBlob
	pending map[string][]byte
	puts    []fakeRequest
	gets    []string

	commitStatus int
	corruptGet   bool
	omitETag     bool
	block        chan struct{}
}

type fakeRequest struct {
	method      string
	path        string
	ifNoneMatch string
	query       string
	status      int
}

func newFakeAzureBlobServer() *fakeAzureBlobServer {
	return &fakeAzureBlobServer{
		blobs:   make(map[string]fakeAzureBlob),
		pending: make(map[string][]byte),
	}
}

func TestNewAzureBlobFenceWithClientValidatesContextConfigAndClient(t *testing.T) {
	client, err := azblob.NewClientWithNoCredential("https://account.blob.core.windows.net", &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := AzureFenceConfig{ServiceURL: "https://account.blob.core.windows.net", ContainerName: "private", TenantID: "tenant-a", EnvironmentDigest: "env-a", Timeout: time.Second, MaxScanPages: 2, MaxScanObjects: 8, MaxScanBytes: 1024}
	fence, err := NewAzureBlobFenceWithClient(context.Background(), cfg, client)
	if err != nil || fence == nil || !fence.Durable() || fence.maxScanPages != 2 || fence.maxScanObjects != 8 || fence.maxScanBytes != 1024 {
		t.Fatalf("valid injected client fence=%+v err=%v", fence, err)
	}
	var nilContext context.Context
	if _, err := NewAzureBlobFenceWithClient(nilContext, cfg, client); err == nil {
		t.Fatal("nil context accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewAzureBlobFenceWithClient(canceled, cfg, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error=%v", err)
	}
	if _, err := NewAzureBlobFenceWithClient(context.Background(), cfg, nil); err == nil {
		t.Fatal("nil client accepted")
	}
}

func (s *fakeAzureBlobServer) handler(w http.ResponseWriter, r *http.Request) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-r.Context().Done():
			return
		}
	}

	path := r.URL.Path
	if r.Method == http.MethodGet {
		s.mu.Lock()
		s.gets = append(s.gets, path)
		blob, ok := s.blobs[path]
		corrupt := s.corruptGet
		omitETag := s.omitETag
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := blob.body
		if corrupt {
			body = append(append([]byte(nil), body...), 'x')
		}
		if !omitETag {
			w.Header().Set("ETag", blob.etag)
		}
		w.Header().Set("Content-Length", stringLength(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("comp") == "block" {
		s.mu.Lock()
		s.pending[path] = append([]byte(nil), body...)
		s.puts = append(s.puts, fakeRequest{method: r.Method, path: path, ifNoneMatch: r.Header.Get("If-None-Match"), query: r.URL.RawQuery, status: http.StatusCreated})
		s.mu.Unlock()
		w.Header().Set("ETag", `"staged"`)
		w.WriteHeader(http.StatusCreated)
		return
	}

	status := s.commitStatus
	if status == 0 {
		status = http.StatusCreated
	}
	s.mu.Lock()
	_, exists := s.blobs[path]
	if exists && r.Header.Get("If-None-Match") == "*" {
		status = http.StatusPreconditionFailed
	}
	request := fakeRequest{method: r.Method, path: path, ifNoneMatch: r.Header.Get("If-None-Match"), query: r.URL.RawQuery, status: status}
	s.puts = append(s.puts, request)
	responseETag := ""
	if status == http.StatusCreated {
		content := append([]byte(nil), s.pending[path]...)
		if len(content) == 0 {
			content = append([]byte(nil), body...)
		}
		etag := `"etag-` + stringLength(len(s.blobs)+1) + `"`
		s.blobs[path] = fakeAzureBlob{body: content, etag: etag}
		responseETag = etag
	}
	s.mu.Unlock()
	if status == http.StatusCreated {
		w.Header().Set("ETag", responseETag)
	}
	w.WriteHeader(status)
}

func stringLength(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func newTestAzureFence(t *testing.T, serverURL string, timeout time.Duration) *AzureBlobFence {
	t.Helper()
	client, err := azblob.NewClientWithNoCredential(serverURL, &azblob.ClientOptions{
		ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := newAzureBlobFence(client, AzureFenceConfig{
		ContainerName:     "private",
		TenantID:          "tenant-a",
		EnvironmentDigest: "env-a",
		Timeout:           timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fence
}

func bodyDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func TestAzureBlobFenceCreateOnlyReadbackAndTenantBoundPath(t *testing.T) {
	fake := newFakeAzureBlobServer()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newTestAzureFence(t, server.URL, time.Second)
	var _ Fence = fence
	if !fence.Durable() {
		t.Fatal("configured Azure fence must be durable")
	}
	if (&FakeFence{}).Durable() {
		t.Fatal("FakeFence must not be durable")
	}

	body := azureClaimFixture(t, "transfer-1")
	digest, err := fence.CreateClaim(context.Background(), "transfer-1", body)
	if err != nil {
		t.Fatal(err)
	}
	if digest != bodyDigest(body) {
		t.Fatalf("digest = %q, want %q", digest, bodyDigest(body))
	}
	if err := fence.VerifyClaim(context.Background(), "transfer-1", digest); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	if len(fake.puts) == 0 {
		fake.mu.Unlock()
		t.Fatal("expected upload requests")
	}
	path := fake.puts[len(fake.puts)-1].path
	requests := append([]fakeRequest(nil), fake.puts...)
	fake.mu.Unlock()
	wantTenant := bodyDigest([]byte("tenant-a"))
	if !strings.Contains(path, "/env-a/tenant/"+wantTenant+"/fences/transfer-1/claim.json") {
		t.Fatalf("path %q is not tenant-bound", path)
	}
	for _, request := range requests {
		if request.query != "" && request.method == http.MethodPut && strings.Contains(request.query, "comp=blocklist") && request.ifNoneMatch != "*" {
			t.Fatalf("commit request missing If-None-Match: *: %+v", request)
		}
	}

	terminalDigest, err := fence.CreateTerminal(context.Background(), "transfer-1", azureTerminalFixture(t, "transfer-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fence.VerifyTerminal(context.Background(), "transfer-1", terminalDigest); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for path := range fake.blobs {
		if strings.HasSuffix(path, "/claim.json") || strings.HasSuffix(path, "/terminal.json") {
			if !strings.Contains(path, "/tenant/"+wantTenant+"/") {
				t.Fatalf("fence path %q escaped tenant binding", path)
			}
		}
	}
}

func TestAzureBlobFenceRejectsExistingBlobEvenWhenIdentical(t *testing.T) {
	fake := newFakeAzureBlobServer()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newTestAzureFence(t, server.URL, time.Second)
	body := azureClaimFixture(t, "transfer-2")
	if _, err := fence.CreateClaim(context.Background(), "transfer-2", body); err != nil {
		t.Fatal(err)
	}
	if _, err := fence.CreateClaim(context.Background(), "transfer-2", body); err == nil {
		t.Fatal("identical existing blob was adopted")
	}
}

func TestAzureBlobFenceRejectsCreateConflicts(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusPreconditionFailed} {
		t.Run(stringLength(status), func(t *testing.T) {
			fake := newFakeAzureBlobServer()
			fake.commitStatus = status
			server := httptest.NewServer(http.HandlerFunc(fake.handler))
			defer server.Close()
			fence := newTestAzureFence(t, server.URL, time.Second)
			if _, err := fence.CreateTerminal(context.Background(), "transfer-conflict", azureTerminalFixture(t, "transfer-conflict")); err == nil {
				t.Fatalf("status %d was accepted", status)
			}
		})
	}
}

func TestAzureBlobFenceConcurrentCreateRaceHasOneWinner(t *testing.T) {
	fake := newFakeAzureBlobServer()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newTestAzureFence(t, server.URL, 3*time.Second)
	body := azureClaimFixture(t, "transfer-race")

	const contenders = 24
	results := make(chan error, contenders)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < contenders; i++ {
		go func() {
			start.Wait()
			_, err := fence.CreateClaim(context.Background(), "transfer-race", body)
			results <- err
		}()
	}
	start.Done()

	winners := 0
	for i := 0; i < contenders; i++ {
		if err := <-results; err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("successful creates = %d, want 1", winners)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.blobs) != 1 {
		t.Fatalf("stored blobs = %d, want 1", len(fake.blobs))
	}
}

func TestAzureBlobFenceRejectsReadbackCorruptionAndMissingETag(t *testing.T) {
	for _, test := range []struct {
		name     string
		corrupt  bool
		omitETag bool
	}{
		{name: "bytes", corrupt: true},
		{name: "etag", omitETag: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeAzureBlobServer()
			fake.corruptGet = test.corrupt
			fake.omitETag = test.omitETag
			server := httptest.NewServer(http.HandlerFunc(fake.handler))
			defer server.Close()
			fence := newTestAzureFence(t, server.URL, time.Second)
			if _, err := fence.CreateClaim(context.Background(), "transfer-corrupt-"+test.name, azureClaimFixture(t, "transfer-corrupt-"+test.name)); err == nil {
				t.Fatal("corrupt read-back was accepted")
			}
		})
	}
}

func TestAzureBlobFenceVerifyRejectsReadbackCorruption(t *testing.T) {
	fake := newFakeAzureBlobServer()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newTestAzureFence(t, server.URL, time.Second)
	body := azureClaimFixture(t, "transfer-verify")
	digest, err := fence.CreateClaim(context.Background(), "transfer-verify", body)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.corruptGet = true
	fake.mu.Unlock()
	if err := fence.VerifyClaim(context.Background(), "transfer-verify", digest); err == nil {
		t.Fatal("corrupt verify read-back was accepted")
	}
}

func TestAzureBlobFenceUsesBoundedContext(t *testing.T) {
	fake := newFakeAzureBlobServer()
	fake.block = make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	defer close(fake.block)
	fence := newTestAzureFence(t, server.URL, 25*time.Millisecond)
	started := time.Now()
	_, err := fence.CreateClaim(context.Background(), "transfer-timeout", azureClaimFixture(t, "transfer-timeout"))
	if err == nil {
		t.Fatal("blocked create succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded call took %s", elapsed)
	}
}

func TestAzureBlobFenceConfigurationValidation(t *testing.T) {
	client, err := azblob.NewClientWithNoCredential("http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]AzureFenceConfig{
		"missing container":   {TenantID: "tenant", EnvironmentDigest: "env", Timeout: time.Second},
		"missing tenant":      {ContainerName: "private", EnvironmentDigest: "env", Timeout: time.Second},
		"missing environment": {ContainerName: "private", TenantID: "tenant", Timeout: time.Second},
		"missing timeout":     {ContainerName: "private", TenantID: "tenant", EnvironmentDigest: "env"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newAzureBlobFence(client, config); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
