package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFencePayloadRejectsAlteredBindingsBeforeUpload(t *testing.T) {
	fake := newFakeAzureBlobServer()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newTestAzureFence(t, server.URL, time.Second)
	claim := azureClaimFixture(t, "tr-1")
	var original claimPayload
	if err := json.Unmarshal(claim, &original); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*claimPayload){
		"environment": func(p *claimPayload) { p.EnvironmentDigest = "wrong" },
		"tenant":      func(p *claimPayload) { p.TenantDigest = "wrong" },
		"transfer":    func(p *claimPayload) { p.TransferID = "wrong" },
		"row version": func(p *claimPayload) { p.ClaimRowVersion = 0 },
		"claim state": func(p *claimPayload) { p.ClaimState = "staged" },
		"source":      func(p *claimPayload) { p.SourceHash = "" },
		"request":     func(p *claimPayload) { p.RequestDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			p := original
			mutate(&p)
			b, _ := json.Marshal(p)
			if _, err := fence.CreateClaim(context.Background(), "tr-1", b); err == nil {
				t.Fatal("invalid binding uploaded")
			}
		})
	}
	if _, _, err := fence.Identity("wrong-tenant"); err == nil {
		t.Fatal("wrong tenant accepted")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.puts) != 0 {
		t.Fatalf("invalid payloads sent %d PUTs", len(fake.puts))
	}
}
func TestFenceExactBytesAndDigestRejectAlteration(t *testing.T) {
	fake := newFakeAzureBlobServer()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	fence := newTestAzureFence(t, server.URL, time.Second)
	b := azureClaimFixture(t, "tr-1")
	d, err := fence.CreateClaim(context.Background(), "tr-1", b)
	if err != nil {
		t.Fatal(err)
	}
	altered := append([]byte(nil), b...)
	altered[len(altered)-1] = ' '
	if err := fence.VerifyClaim(context.Background(), "tr-1", d, altered); err == nil {
		t.Fatal("altered expected bytes accepted")
	}
	if err := fence.VerifyClaim(context.Background(), "tr-1", digest("not the blob"), b); err == nil {
		t.Fatal("wrong digest accepted")
	}
	if err := fence.VerifyClaim(context.Background(), "tr-1", d, b); err != nil {
		t.Fatal(err)
	}
}
func TestConfirmAlteredRowVersionReadbackNeverPosts(t *testing.T) {
	svc, provider, _ := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, provider)
	base := svc.fence.(*FakeFence)
	svc.fence = &c2Fence{base: base, createHook: func() {
		base.mu.Lock()
		defer base.mu.Unlock()
		var p claimPayload
		if err := json.Unmarshal(base.claimBytes[staged.TransferID], &p); err != nil {
			t.Error(err)
			return
		}
		p.ClaimRowVersion++
		altered, err := json.Marshal(p)
		if err != nil {
			t.Error(err)
			return
		}
		base.claimBytes[staged.TransferID] = altered
		base.Claims[staged.TransferID] = digest(string(altered))
	}}
	_, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now(), IdempotencyDigest: "confirm-key", RequestDigest: "confirm-request"})
	var unknown *ConfirmUnknownError
	if !errors.As(err, &unknown) || !unknown.NoRepeat {
		t.Fatalf("want no-repeat outcome: %v", err)
	}
	if provider.posts.Load() != 0 {
		t.Fatal("POST after altered claim row version")
	}
}

func TestConfirmCorruptClaimFenceNeverPosts(t *testing.T) {
	svc, provider, _ := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, provider)
	base := svc.fence.(*FakeFence)
	wrapper := &c2Fence{base: base, createHook: func() {
		base.mu.Lock()
		for id := range base.claimBytes {
			base.claimBytes[id] = []byte("corrupted")
		}
		base.mu.Unlock()
	}}
	svc.fence = wrapper
	_, err := svc.Confirm(b1TrustedConfirmContext(tenant), ConfirmRequest{TenantID: tenant, ActorID: actor, Permission: permission, TransferID: staged.TransferID, Token: staged.Token, Now: time.Now(), IdempotencyDigest: "confirm-key", RequestDigest: "confirm-request"})
	var unknown *ConfirmUnknownError
	if !errors.As(err, &unknown) || !unknown.NoRepeat {
		t.Fatalf("want no-repeat outcome: %v", err)
	}
	if provider.posts.Load() != 0 {
		t.Fatal("POST after corrupt fence")
	}
}
