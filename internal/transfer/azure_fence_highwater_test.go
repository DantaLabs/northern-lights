package transfer

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestAzureCaptureHighWaterFromStorageServiceDate(t *testing.T) {
	fake := newScanFakeBlobServer()
	server := newScanHTTPServer(t, http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newScanFence(t, server.URL, time.Second, 4, 4, 1<<20)
	mark, err := fence.CaptureHighWater(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if mark.Authority != "azure_blob_last_modified" || mark.TenantDigest != digest("tenant-a") || mark.EnvironmentDigest != "env-a" || mark.CapturedAt != "2026-10-06T01:02:03Z" {
		t.Fatalf("mark=%+v", mark)
	}
	if _, err := fence.CaptureHighWater(context.Background(), "other"); err == nil {
		t.Fatal("wrong tenant accepted")
	}
}
