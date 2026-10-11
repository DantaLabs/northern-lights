package content

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type contentBlob struct {
	body         []byte
	etag         string
	lastModified time.Time
}
type contentBlobServer struct {
	mu               sync.Mutex
	blobs            map[string]contentBlob
	pending          map[string][]byte
	listCalls        int
	puts             []string
	listETagUnquoted bool
}

func newContentBlobServer() *contentBlobServer {
	return &contentBlobServer{blobs: map[string]contentBlob{}, pending: map[string][]byte{}}
}

func newContentBlobHTTPServer(t *testing.T, fake *contentBlobServer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(fake.handler))
}

func (s *contentBlobServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("comp") == "list" {
		s.mu.Lock()
		s.listCalls++
		listETagUnquoted := s.listETagUnquoted
		blobs := make(map[string]contentBlob, len(s.blobs))
		for name, value := range s.blobs {
			blobs[name] = value
		}
		s.mu.Unlock()
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/xml")
		var listing strings.Builder
		listing.WriteString(`<EnumerationResults><Blobs>`)
		for encodedName, blob := range blobs {
			logical, _ := url.PathUnescape(strings.TrimPrefix(encodedName, "/private/"))
			var name bytes.Buffer
			_ = xml.EscapeText(&name, []byte(logical))
			listETag := blob.etag
			if listETagUnquoted {
				listETag = strings.Trim(listETag, `"`)
			}
			listing.WriteString(`<Blob><Name>` + name.String() + `</Name><Properties><Last-Modified>` + blob.lastModified.Format(http.TimeFormat) + `</Last-Modified><Etag>` + listETag + `</Etag><Content-Length>` + strconv.Itoa(len(blob.body)) + `</Content-Length><BlobType>BlockBlob</BlobType></Properties><Metadata/></Blob>`)
		}
		listing.WriteString(`</Blobs><NextMarker></NextMarker></EnumerationResults>`)
		_, _ = io.WriteString(w, listing.String())
		return
	}
	path := r.URL.Path
	if r.Method == http.MethodGet {
		s.mu.Lock()
		item, ok := s.blobs[path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", item.etag)
		w.Header().Set("Last-Modified", item.lastModified.Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(item.body)))
		_, _ = w.Write(item.body)
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
	s.mu.Lock()
	s.puts = append(s.puts, r.URL.RawQuery+"|"+r.Header.Get("If-None-Match"))
	if r.URL.Query().Get("comp") == "block" {
		s.pending[path] = append([]byte(nil), body...)
		s.mu.Unlock()
		w.Header().Set("ETag", `"staged"`)
		w.WriteHeader(http.StatusCreated)
		return
	}
	if r.URL.Query().Get("comp") == "blocklist" || r.URL.RawQuery == "" {
		if _, exists := s.blobs[path]; exists && r.Header.Get("If-None-Match") == "*" {
			s.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		stored := append([]byte(nil), s.pending[path]...)
		if len(stored) == 0 {
			stored = append([]byte(nil), body...)
		}
		etag := `"content-etag"`
		s.blobs[path] = contentBlob{body: stored, etag: etag, lastModified: time.Now().UTC().Add(-time.Second).Truncate(time.Second)}
		s.mu.Unlock()
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", time.Now().UTC().Add(-time.Second).Truncate(time.Second).Format(http.TimeFormat))
		w.WriteHeader(http.StatusCreated)
		return
	}
	s.mu.Unlock()
	http.Error(w, "unsupported PUT query: "+r.URL.RawQuery, http.StatusBadRequest)
}

func newTestAzureContentFence(t *testing.T, serviceURL string, timeout time.Duration) *AzureContentFence {
	t.Helper()
	client, err := azblobTestClient(serviceURL)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := NewAzureContentFenceWithClient(context.Background(), AzureContentFenceConfig{ServiceURL: serviceURL, ContainerName: "private", TenantID: "tenant", EnvironmentDigest: "env-test", Timeout: timeout}, client)
	if err != nil {
		t.Fatal(err)
	}
	return fence
}

func azblobTestClient(serviceURL string) (*azblob.Client, error) {
	return azblob.NewClientWithNoCredential(serviceURL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: 0}}})
}

func TestAzureContentFenceUsesDistinctCreateOnlyNamespace(t *testing.T) {
	fake := newContentBlobServer()
	server := newContentBlobHTTPServer(t, fake)
	defer server.Close()
	fence := newTestAzureContentFence(t, server.URL, time.Second)
	claim := testContentClaim()
	_, td, err := fence.Identity("tenant")
	if err != nil {
		t.Fatal(err)
	}
	claim.TenantDigest = td
	digest, err := fence.CreateClaim(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := fence.VerifyClaim(context.Background(), claim, digest); err != nil {
		t.Fatal(err)
	}
	terminal := testContentTerminal(claim)
	if _, _, err := fence.ReadVerifiedTerminal(context.Background(), claim, ""); !errors.Is(err, ErrContentFenceObjectNotFound) {
		t.Fatalf("missing exact Azure terminal should be definitive not-found: %v", err)
	}
	terminalDigest, err := fence.CreateTerminal(context.Background(), claim, terminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := fence.VerifyTerminal(context.Background(), claim, terminal, terminalDigest); err != nil {
		t.Fatal(err)
	}
	readTerminal, readDigest, err := fence.ReadVerifiedTerminal(context.Background(), claim, "")
	if err != nil || readDigest != terminalDigest || readTerminal != terminal {
		t.Fatalf("read terminal=%+v digest=%s err=%v", readTerminal, readDigest, err)
	}
	if _, err := fence.CreateClaim(context.Background(), claim); err == nil {
		t.Fatal("existing claim was adopted")
	}
	wantPrefix := "/private/env/env-test/tenant/" + td + "/content_placement_fences/place-1/"
	fake.mu.Lock()
	if len(fake.blobs) != 2 {
		fake.mu.Unlock()
		t.Fatalf("stored blobs=%d", len(fake.blobs))
	}
	for path := range fake.blobs {
		if !strings.HasPrefix(path, wantPrefix) || strings.Contains(path, "/fences/") {
			fake.mu.Unlock()
			t.Fatalf("unexpected fence path %q", path)
		}
	}
	for _, request := range fake.puts {
		if !strings.HasSuffix(request, "|*") {
			fake.mu.Unlock()
			t.Fatalf("conditional create missing If-None-Match *: %q", request)
		}
	}
	fake.mu.Unlock()
	highWater, err := fence.CaptureHighWater(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := fence.Scan(context.Background(), ContentFenceScanRequest{TenantID: "tenant", HighWater: highWater})
	if err != nil || len(objects) != 2 {
		t.Fatalf("complete scan objects=%d err=%v", len(objects), err)
	}
	boundary := highWater
	boundary.CapturedAt = objects[0].LastModified.UTC().Format(time.RFC3339Nano)
	if _, err := fence.Scan(context.Background(), ContentFenceScanRequest{TenantID: "tenant", HighWater: boundary}); err == nil {
		t.Fatal("same-second fence at the high-water boundary was accepted")
	}
}

func TestAzureContentFenceScanAcceptsAzureListETagWithoutQuotes(t *testing.T) {
	fake := newContentBlobServer()
	fake.listETagUnquoted = true
	server := newContentBlobHTTPServer(t, fake)
	defer server.Close()
	fence := newTestAzureContentFence(t, server.URL, time.Second)
	claim := testContentClaim()
	_, tenantDigest, err := fence.Identity("tenant")
	if err != nil {
		t.Fatal(err)
	}
	claim.TenantDigest = tenantDigest
	claimDigest, err := fence.CreateClaim(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	terminal := testContentTerminal(claim)
	if _, err := fence.CreateTerminal(context.Background(), claim, terminal); err != nil {
		t.Fatal(err)
	}
	highWater, err := fence.CaptureHighWater(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := fence.Scan(context.Background(), ContentFenceScanRequest{TenantID: "tenant", HighWater: highWater})
	if err != nil {
		t.Fatalf("scan rejected matching strong ETags in Azure list/header representations (claim %s): %v", claimDigest, err)
	}
	if len(objects) != 2 {
		t.Fatalf("scan objects=%d, want claim and terminal", len(objects))
	}
}

func TestSameStrongContentFenceETagEnforcesOpaqueEqualityAndRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		want              bool
	}{
		{name: "same unquoted", left: "azure-tag", right: "azure-tag", want: true},
		{name: "same quoted", left: `"azure-tag"`, right: `"azure-tag"`, want: true},
		{name: "Azure list and HTTP header forms", left: "azure-tag", right: `"azure-tag"`, want: true},
		{name: "different opaque tags", left: "azure-tag", right: `"other-tag"`},
		{name: "weak uppercase", left: `W/"azure-tag"`, right: `W/"azure-tag"`},
		{name: "weak lowercase", left: `w/"azure-tag"`, right: `w/"azure-tag"`},
		{name: "unmatched quote", left: `"azure-tag`, right: `"azure-tag`},
		{name: "embedded quote", left: `"azure"tag"`, right: `"azure"tag"`},
		{name: "control", left: "azure\x01tag", right: "azure\x01tag"},
		{name: "whitespace", left: " azure-tag", right: " azure-tag"},
		{name: "oversized", left: strings.Repeat("a", maxContentFenceETagLength+1), right: strings.Repeat("a", maxContentFenceETagLength+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameContentETag(tc.left, tc.right); got != tc.want {
				t.Fatalf("sameContentETag(%q,%q)=%v, want %v", tc.left, tc.right, got, tc.want)
			}
		})
	}
}
