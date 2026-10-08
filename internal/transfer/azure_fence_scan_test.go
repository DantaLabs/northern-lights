package transfer

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/dantalabs/northern-lights/internal/assurance"
)

type scanFakeBlob struct {
	name         string
	body         []byte
	etag         string
	lastModified time.Time
	metadata     map[string]string
}

type scanFakeBlobServer struct {
	mu sync.Mutex

	blobs       map[string]scanFakeBlob
	pageSize    int
	listCalls   int
	getNames    []string
	listQueries []string
	blockGets   chan struct{}
	blockLists  chan struct{}
	getETag     string
	omitGetETag bool
	getMetadata map[string]string
	getBody     []byte
}

func newScanHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func newScanFakeBlobServer(blobs ...scanFakeBlob) *scanFakeBlobServer {
	server := &scanFakeBlobServer{blobs: make(map[string]scanFakeBlob), pageSize: 5000}
	for _, blob := range blobs {
		server.blobs[blob.name] = blob
	}
	return server
}

func (s *scanFakeBlobServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("comp") == "list" {
		if s.blockLists != nil {
			select {
			case <-s.blockLists:
			case <-r.Context().Done():
				return
			}
		}
		s.mu.Lock()
		s.listCalls++
		s.listQueries = append(s.listQueries, r.URL.RawQuery)
		pageSize := s.pageSize
		if pageSize <= 0 {
			pageSize = 1
		}
		items := make([]scanFakeBlob, 0, len(s.blobs))
		prefix := r.URL.Query().Get("prefix")
		for _, blob := range s.blobs {
			if strings.HasPrefix(blob.name, prefix) {
				items = append(items, blob)
			}
		}
		s.mu.Unlock()
		sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

		start := 0
		if marker := r.URL.Query().Get("marker"); marker != "" {
			for start < len(items) && items[start].name <= marker {
				start++
			}
		}
		end := start + pageSize
		if end > len(items) {
			end = len(items)
		}
		next := ""
		if end < len(items) {
			next = items[end-1].name
		}
		writeScanList(w, items[start:end], next)
		return
	}

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.blockGets != nil {
		select {
		case <-s.blockGets:
		case <-r.Context().Done():
			return
		}
	}
	name := strings.TrimPrefix(r.URL.Path, "/private/")
	s.mu.Lock()
	s.getNames = append(s.getNames, name)
	blob, ok := s.blobs[name]
	getETag, omitGetETag, getMetadata, getBody := s.getETag, s.omitGetETag, s.getMetadata, s.getBody
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if getETag == "" && !omitGetETag {
		getETag = blob.etag
	}
	if getMetadata == nil {
		getMetadata = blob.metadata
	}
	if getBody == nil {
		getBody = blob.body
	}
	if !omitGetETag {
		w.Header().Set("ETag", getETag)
	}
	w.Header().Set("Last-Modified", blob.lastModified.UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Length", fmt.Sprint(len(getBody)))
	for key, value := range getMetadata {
		w.Header().Set("X-Ms-Meta-"+key, value)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(getBody)
}

func writeScanList(w http.ResponseWriter, blobs []scanFakeBlob, next string) {
	type xmlProperties struct {
		ETag          string `xml:"Etag"`
		LastModified  string `xml:"Last-Modified"`
		ContentLength int64  `xml:"Content-Length"`
	}
	type xmlBlob struct {
		Name       string        `xml:"Name"`
		Properties xmlProperties `xml:"Properties"`
		Metadata   struct {
			Proof string `xml:"proof,omitempty"`
		} `xml:"Metadata"`
	}
	type xmlBlobs struct {
		Blobs []*xmlBlob `xml:"Blob"`
	}
	type xmlResult struct {
		XMLName xml.Name `xml:"EnumerationResults"`
		Blobs   xmlBlobs `xml:"Blobs"`
		Next    string   `xml:"NextMarker"`
	}
	result := xmlResult{}
	for _, blob := range blobs {
		result.Blobs.Blobs = append(result.Blobs.Blobs, &xmlBlob{
			Name: blob.name,
			Properties: xmlProperties{
				ETag: blob.etag, LastModified: blob.lastModified.UTC().Format(http.TimeFormat), ContentLength: int64(len(blob.body)),
			},
			Metadata: struct {
				Proof string `xml:"proof,omitempty"`
			}{Proof: blob.metadata["proof"]},
		})
	}
	result.Next = next
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Date", time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC).Format(http.TimeFormat))
	_ = xml.NewEncoder(w).Encode(result)
}

func newScanFence(t *testing.T, serverURL string, timeout time.Duration, pages, objects int, bytes int64) *AzureBlobFence {
	t.Helper()
	client, err := azblob.NewClientWithNoCredential(serverURL, &azblob.ClientOptions{
		ClientOptions: azcore.ClientOptions{Retry: policy.RetryOptions{MaxRetries: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := newAzureBlobFence(client, AzureFenceConfig{
		ContainerName:     "private",
		TenantID:          "tenant-a",
		EnvironmentDigest: "env-a",
		Timeout:           timeout,
		MaxScanPages:      pages,
		MaxScanObjects:    objects,
		MaxScanBytes:      bytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fence
}

func scanRequest(t *testing.T, capturedAt time.Time) FenceScanRequest {
	t.Helper()
	return FenceScanRequest{
		TenantID: "tenant-a", TenantDigest: digest("tenant-a"), EnvironmentDigest: "env-a",
		HighWater: assurance.BackupFenceHighWater{
			TenantDigest: digest("tenant-a"), EnvironmentDigest: "env-a",
			CapturedAt: capturedAt.UTC().Format(time.RFC3339Nano), Authority: "azure_blob_last_modified",
		},
	}
}

func scanBlob(t *testing.T, fence *AzureBlobFence, id, kind string, when time.Time, metadata map[string]string) scanFakeBlob {
	t.Helper()
	var body []byte
	if kind == "claim" {
		body = azureClaimFixture(t, id)
	} else {
		body = azureTerminalFixture(t, id)
	}
	metadataCopy := make(map[string]string, len(metadata))
	for key, value := range metadata {
		metadataCopy[key] = value
	}
	return scanFakeBlob{name: fence.objectName(id, kind), body: body, etag: `"` + id + `-` + kind + `"`, lastModified: when, metadata: metadataCopy}
}

func TestAzureBlobFenceScanListsCompleteNamespaceAndVerifiesObjects(t *testing.T) {
	captured := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	fenceProbe := &AzureBlobFence{environmentDigest: "env-a", tenantDigest: digest("tenant-a")}
	first := scanBlob(t, fenceProbe, "a", "claim", captured.Add(-time.Minute), map[string]string{"proof": "a"})
	second := scanBlob(t, fenceProbe, "b", "terminal", captured, map[string]string{"proof": "b"})
	fake := newScanFakeBlobServer(first, second)
	fake.pageSize = 1
	server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newScanFence(t, server.URL, time.Second, 4, 4, 1<<20)

	objects, err := fence.Scan(context.Background(), scanRequest(t, captured))
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("objects=%d, want 2 (list calls=%d queries=%v)", len(objects), fake.listCalls, fake.listQueries)
	}
	if objects[0].Name != first.name || objects[0].TransferID != "a" || objects[0].Kind != "claim" || objects[0].Digest != bodyDigest(first.body) || !objects[0].LastModified.Equal(first.lastModified) {
		t.Fatalf("unexpected first object: %+v", objects[0])
	}
	if objects[1].ObjectName != second.name || objects[1].FenceKind != "terminal" || objects[1].FenceDigest != bodyDigest(second.body) {
		t.Fatalf("unexpected second object: %+v", objects[1])
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listCalls != 2 || len(fake.getNames) != 2 {
		t.Fatalf("list calls=%d get calls=%d, want 2 each", fake.listCalls, len(fake.getNames))
	}
	if !strings.Contains(fake.listQueries[0], "prefix=env%2Fenv-a%2Ftenant%2F"+digest("tenant-a")+"%2Ffences%2F") || !strings.Contains(fake.listQueries[0], "include=metadata") {
		t.Fatalf("list query did not scope the configured namespace and metadata: %q", fake.listQueries[0])
	}
}

func TestAzureBlobFenceScanAcceptsAzureListETagWithoutQuotesAgainstQuotedGetHeader(t *testing.T) {
	captured := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	fenceProbe := &AzureBlobFence{environmentDigest: "env-a", tenantDigest: digest("tenant-a")}
	blob := scanBlob(t, fenceProbe, "azure-etag", "claim", captured, map[string]string{"proof": "azure-etag"})
	blob.etag = "0x8DF24E927ADCD3B"
	fake := newScanFakeBlobServer(blob)
	fake.getETag = `"0x8DF24E927ADCD3B"`
	server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newScanFence(t, server.URL, time.Second, 4, 4, 1<<20)
	objects, err := fence.Scan(context.Background(), scanRequest(t, captured))
	if err != nil {
		t.Fatalf("scan rejected equivalent Azure list/header ETags: %v", err)
	}
	if len(objects) != 1 || objects[0].Name != blob.name {
		t.Fatalf("scan returned unexpected objects: %+v", objects)
	}
}

func TestSameStrongAzureFenceETagEnforcesRepresentationsAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		want              bool
	}{
		{name: "same unquoted", left: "azure-tag", right: "azure-tag", want: true},
		{name: "same quoted", left: `"azure-tag"`, right: `"azure-tag"`, want: true},
		{name: "equivalent list and header forms", left: "azure-tag", right: `"azure-tag"`, want: true},
		{name: "different value", left: "azure-tag", right: `"other-tag"`},
		{name: "weak uppercase", left: `W/"azure-tag"`, right: `W/"azure-tag"`},
		{name: "weak lowercase", left: `w/"azure-tag"`, right: `w/"azure-tag"`},
		{name: "empty", left: "", right: ""},
		{name: "empty quoted", left: `""`, right: `""`},
		{name: "unmatched quote", left: `"azure-tag`, right: `"azure-tag`},
		{name: "multiple quote pairs", left: `""azure-tag""`, right: `""azure-tag""`},
		{name: "embedded quote", left: `"azure"tag"`, right: `"azure"tag"`},
		{name: "control", left: "azure\x01tag", right: "azure\x01tag"},
		{name: "whitespace", left: " azure-tag", right: " azure-tag"},
		{name: "raw maximum 1024", left: strings.Repeat("a", maxAzureFenceETagLength), right: strings.Repeat("a", maxAzureFenceETagLength), want: true},
		{name: "quoted maximum total length 1024", left: `"` + strings.Repeat("b", maxAzureFenceETagLength-2) + `"`, right: `"` + strings.Repeat("b", maxAzureFenceETagLength-2) + `"`, want: true},
		{name: "raw maximum plus one", left: strings.Repeat("c", maxAzureFenceETagLength+1), right: strings.Repeat("c", maxAzureFenceETagLength+1)},
		{name: "quoted maximum plus one", left: `"` + strings.Repeat("d", maxAzureFenceETagLength-1) + `"`, right: `"` + strings.Repeat("d", maxAzureFenceETagLength-1) + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameStrongAzureFenceETag(tc.left, tc.right); got != tc.want {
				t.Fatalf("sameStrongAzureFenceETag()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestAzureBlobFenceScanRejectsMalformedOrUnboundedETags(t *testing.T) {
	captured := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	fenceProbe := &AzureBlobFence{environmentDigest: "env-a", tenantDigest: digest("tenant-a")}
	base := scanBlob(t, fenceProbe, "bad-etag", "claim", captured, map[string]string{"proof": "bad-etag"})
	tests := []struct {
		name string
		list string
		get  string
	}{
		{name: "weak list and header", list: `W/"azure-tag"`, get: `W/"azure-tag"`},
		{name: "unmatched opening quote", list: `"azure-tag`, get: `"azure-tag`},
		{name: "unmatched closing quote", list: `azure-tag"`, get: `azure-tag"`},
		{name: "embedded quote", list: `"azure"tag"`, get: `"azure"tag"`},
		{name: "different opaque value", list: "azure-tag", get: `"other-tag"`},
		{name: "control character", list: "azure\x01tag", get: "azure\x01tag"},
		{name: "oversized", list: strings.Repeat("a", 1025), get: strings.Repeat("a", 1025)},
		{name: "blank list", list: "", get: ""},
		{name: "missing get header", list: "azure-tag"},
		{name: "whitespace boundary", list: " azure-tag ", get: " azure-tag "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blob := base
			blob.etag = test.list
			fake := newScanFakeBlobServer(blob)
			fake.getETag = test.get
			fake.omitGetETag = test.name == "missing get header"
			server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
			defer server.Close()
			fence := newScanFence(t, server.URL, time.Second, 4, 4, 1<<20)
			if _, err := fence.Scan(context.Background(), scanRequest(t, captured)); err == nil {
				t.Fatal("scan accepted malformed, missing, unbounded, or mismatched ETag")
			}
		})
	}
}

func TestAzureBlobFenceScanRejectsUnknownNamesHighwaterAndGetMismatches(t *testing.T) {
	captured := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	fenceProbe := &AzureBlobFence{environmentDigest: "env-a", tenantDigest: digest("tenant-a")}
	base := scanBlob(t, fenceProbe, "a", "claim", captured, map[string]string{"proof": "a"})
	tests := []struct {
		name   string
		mutate func(*scanFakeBlobServer, *scanFakeBlob)
	}{
		{name: "unknown name", mutate: func(_ *scanFakeBlobServer, blob *scanFakeBlob) { blob.name += "/unknown" }},
		{name: "after highwater", mutate: func(_ *scanFakeBlobServer, blob *scanFakeBlob) { blob.lastModified = captured.Add(time.Second) }},
		{name: "body", mutate: func(fake *scanFakeBlobServer, _ *scanFakeBlob) { fake.getBody = []byte("tampered") }},
		{name: "etag", mutate: func(fake *scanFakeBlobServer, _ *scanFakeBlob) { fake.getETag = `"different"` }},
		{name: "metadata", mutate: func(fake *scanFakeBlobServer, _ *scanFakeBlob) {
			fake.getMetadata = map[string]string{"proof": "different"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			blob := base
			fake := newScanFakeBlobServer(blob)
			test.mutate(fake, &blob)
			if blob.name != base.name {
				fake = newScanFakeBlobServer(blob)
			} else {
				fake.blobs[blob.name] = blob
			}
			server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
			defer server.Close()
			fence := newScanFence(t, server.URL, time.Second, 4, 4, 1<<20)
			if _, err := fence.Scan(context.Background(), scanRequest(t, captured)); err == nil {
				t.Fatal("scan accepted an invalid Blob")
			}
		})
	}
}

func TestAzureBlobFenceScanEnforcesPageObjectByteAndTimeBounds(t *testing.T) {
	captured := time.Date(2026, 10, 6, 1, 2, 3, 0, time.UTC)
	fenceProbe := &AzureBlobFence{environmentDigest: "env-a", tenantDigest: digest("tenant-a")}
	first := scanBlob(t, fenceProbe, "a", "claim", captured, nil)
	second := scanBlob(t, fenceProbe, "b", "claim", captured, nil)

	t.Run("pages", func(t *testing.T) {
		fake := newScanFakeBlobServer(first, second)
		fake.pageSize = 1
		server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
		defer server.Close()
		fence := newScanFence(t, server.URL, time.Second, 1, 4, 1<<20)
		if _, err := fence.Scan(context.Background(), scanRequest(t, captured)); err == nil {
			t.Fatal("scan exceeded page bound")
		}
	})

	t.Run("objects", func(t *testing.T) {
		fake := newScanFakeBlobServer(first, second)
		server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
		defer server.Close()
		fence := newScanFence(t, server.URL, time.Second, 4, 1, 1<<20)
		if _, err := fence.Scan(context.Background(), scanRequest(t, captured)); err == nil {
			t.Fatal("scan exceeded object bound")
		}
	})

	t.Run("bytes", func(t *testing.T) {
		fake := newScanFakeBlobServer(first, second)
		server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
		defer server.Close()
		fence := newScanFence(t, server.URL, time.Second, 4, 4, int64(len(first.body)-1))
		if _, err := fence.Scan(context.Background(), scanRequest(t, captured)); err == nil {
			t.Fatal("scan exceeded byte bound")
		}
	})

	t.Run("exact configured boundaries", func(t *testing.T) {
		fake := newScanFakeBlobServer(first, second)
		fake.pageSize = 1
		server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
		defer server.Close()
		maxBytes := int64(len(first.body) + len(second.body))
		fence := newScanFence(t, server.URL, time.Second, 2, 2, maxBytes)
		objects, err := fence.Scan(context.Background(), scanRequest(t, captured))
		if err != nil {
			t.Fatalf("scan at exact page/object/byte bounds failed: %v", err)
		}
		if len(objects) != 2 {
			t.Fatalf("objects=%d, want 2", len(objects))
		}
	})

	t.Run("time", func(t *testing.T) {
		fake := newScanFakeBlobServer(first)
		fake.blockGets = make(chan struct{})
		server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
		defer server.Close()
		fence := newScanFence(t, server.URL, 20*time.Millisecond, 4, 4, 1<<20)
		_, err := fence.Scan(context.Background(), scanRequest(t, captured))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v, want deadline exceeded", err)
		}
	})

	t.Run("list timeout", func(t *testing.T) {
		fake := newScanFakeBlobServer(first)
		fake.blockLists = make(chan struct{})
		server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
		defer server.Close()
		fence := newScanFence(t, server.URL, 20*time.Millisecond, 4, 4, 1<<20)
		_, err := fence.Scan(context.Background(), scanRequest(t, captured))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v, want deadline exceeded", err)
		}
	})

	t.Run("negative limits", func(t *testing.T) {
		for _, limits := range []struct {
			name    string
			pages   int
			objects int
			bytes   int64
		}{
			{name: "pages", pages: -1, objects: 4, bytes: 1 << 20},
			{name: "objects", pages: 4, objects: -1, bytes: 1 << 20},
			{name: "bytes", pages: 4, objects: 4, bytes: -1},
		} {
			t.Run(limits.name, func(t *testing.T) {
				fake := newScanFakeBlobServer(first)
				server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
				defer server.Close()
				fence := newScanFence(t, server.URL, time.Second, limits.pages, limits.objects, limits.bytes)
				if _, err := fence.Scan(context.Background(), scanRequest(t, captured)); err == nil {
					t.Fatal("scan accepted negative configured limits")
				}
			})
		}
	})
}

func TestAzureBlobFenceScanRejectsInvalidHighwaterBinding(t *testing.T) {
	fake := newScanFakeBlobServer()
	server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newScanFence(t, server.URL, time.Second, 4, 4, 1<<20)
	request := scanRequest(t, time.Now())
	request.HighWater.Authority = "local_clock"
	if _, err := fence.Scan(context.Background(), request); err == nil {
		t.Fatal("scan accepted unsigned highwater authority")
	}
	request = scanRequest(t, time.Now())
	request.TenantDigest = "wrong"
	if _, err := fence.Scan(context.Background(), request); err == nil {
		t.Fatal("scan accepted mismatched request tenant")
	}
}
