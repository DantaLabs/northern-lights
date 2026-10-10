package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func testContentClaim() ContentClaim {
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	return ContentClaim{SchemaVersion: 1, MutationKind: ContentMutationKind, EnvironmentDigest: "env-test", TenantDigest: h("tenant"), PlacementIntentID: "place-1", CandidateKind: "extracted_item", ActorDigest: h("actor"), IdempotencyDigest: h("idempotency"), RequestDigest: h("request"), PreviewHash: h("preview"), SourceArtifactHash: h("source-artifact"), CandidateHash: h("candidate"), ProfileHash: h("profile"), ResourcePolicyHash: h("resource-policy"), ConversionPolicyHash: h("conversion-policy"), AccessPolicyHash: h("access-policy"), ProviderPolicyHash: h("provider-policy"), RetentionPolicyHash: h("retention-policy"), ActiveBundleHash: h("active-bundle"), ActiveBundleVersion: 1, TargetHash: h("target"), IntendedHash: h("intended"), ClaimedAt: time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC), ClaimRowVersion: 2}
}

func testContentTerminal(claim ContentClaim) ContentTerminal {
	body, _ := CanonicalContentClaim(claim)
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	return ContentTerminal{SchemaVersion: 1, MutationKind: ContentMutationKind, EnvironmentDigest: claim.EnvironmentDigest, TenantDigest: claim.TenantDigest, PlacementIntentID: claim.PlacementIntentID, ClaimDigest: contentDigest(body), Outcome: ContentOutcomeAccepted, OperationReferenceHash: h("operation"), ReadbackHash: h("readback"), ReadbackCacheBypassed: true, TerminalAt: claim.ClaimedAt.Add(time.Minute), TerminalRowVersion: 3}
}

func TestContentFencePayloadsCanonicalAndClosed(t *testing.T) {
	claim := testContentClaim()
	body, err := CanonicalContentClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContainsAny(body, []byte("source text"), []byte("confirmation token")) {
		t.Fatal("payload leaked raw source or token")
	}
	if _, err := ValidateContentClaimJSON(body); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	fields["unexpected"] = "value"
	unknown, _ := json.Marshal(fields)
	if _, err := ValidateContentClaimJSON(unknown); err == nil {
		t.Fatal("unknown claim field accepted")
	}
	if _, err := ValidateContentClaimJSON(append(append([]byte(nil), body...), []byte(" {}")...)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	missingIdempotency := claim
	missingIdempotency.IdempotencyDigest = ""
	if _, err := CanonicalContentClaim(missingIdempotency); err == nil {
		t.Fatal("claim without idempotency digest accepted")
	}
	itemWithInventedDraft := claim
	itemWithInventedDraft.DraftArtifactHash = strings.Repeat("a", 64)
	if _, err := CanonicalContentClaim(itemWithInventedDraft); err == nil {
		t.Fatal("extracted-item claim accepted draft hash")
	}
	itemWithLineage := claim
	itemWithLineage.CandidateLineageHash = strings.Repeat("c", 64)
	if _, err := CanonicalContentClaim(itemWithLineage); err != nil {
		t.Fatalf("extracted-item claim rejected verified optional lineage: %v", err)
	}
	itemWithLineage.CandidateLineageHash = "not-a-digest"
	if _, err := CanonicalContentClaim(itemWithLineage); err == nil {
		t.Fatal("extracted-item claim accepted invalid optional lineage")
	}
	draft := claim
	draft.CandidateKind = "draft_artifact"
	draft.DraftArtifactHash = draft.CandidateHash
	draft.CandidateLineageHash = strings.Repeat("b", 64)
	if _, err := CanonicalContentClaim(draft); err != nil {
		t.Fatalf("valid draft claim rejected: %v", err)
	}
	draft.CandidateLineageHash = ""
	if _, err := CanonicalContentClaim(draft); err == nil {
		t.Fatal("draft claim without lineage accepted")
	}

	terminal := testContentTerminal(claim)
	terminalBody, err := CanonicalContentTerminal(claim, terminal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateContentTerminalJSON(terminalBody, claim); err != nil {
		t.Fatal(err)
	}
	terminal.ClaimDigest = strings.Repeat("0", 64)
	if _, err := CanonicalContentTerminal(claim, terminal); err == nil {
		t.Fatal("terminal with mismatched claim accepted")
	}
	terminal = testContentTerminal(claim)
	terminal.Outcome = "provider_said_maybe"
	if _, err := CanonicalContentTerminal(claim, terminal); err == nil {
		t.Fatal("unknown terminal outcome accepted")
	}
	terminal = testContentTerminal(claim)
	terminal.Outcome = ContentOutcomeUnknown
	terminal.OperationReferenceHash = ""
	terminal.ReadbackHash = ""
	terminal.ReadbackCacheBypassed = false
	if _, err := CanonicalContentTerminal(claim, terminal); err != nil {
		t.Fatalf("unknown terminal without fabricated operation/readback proof rejected: %v", err)
	}
	terminal = testContentTerminal(claim)
	terminal.ReadbackHash = ""
	terminal.ReadbackCacheBypassed = false
	if _, err := CanonicalContentTerminal(claim, terminal); err == nil {
		t.Fatal("accepted terminal without uncached readback proof accepted")
	}
}

func TestLocalTestFenceCreateOnlyAndTerminalClaimBinding(t *testing.T) {
	fence, err := NewLocalTestFence("tenant", "env-test")
	if err != nil {
		t.Fatal(err)
	}
	if fence.Durable() {
		t.Fatal("local test fence reported durable")
	}
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
	if err := fence.VerifyClaim(context.Background(), claim, claimDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := fence.CreateClaim(context.Background(), claim); err == nil {
		t.Fatal("existing claim was adopted")
	}

	terminal := testContentTerminal(claim)
	if _, _, err := fence.ReadVerifiedTerminal(context.Background(), claim, ""); !errors.Is(err, ErrContentFenceObjectNotFound) {
		t.Fatalf("missing exact terminal should be definitive not-found: %v", err)
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
		t.Fatalf("discovered terminal=%+v digest=%s err=%v", readTerminal, readDigest, err)
	}
	terminal.ClaimDigest = strings.Repeat("0", 64)
	if _, err := fence.CreateTerminal(context.Background(), claim, terminal); err == nil {
		t.Fatal("terminal detached from claim accepted")
	}

	highWater, err := fence.CaptureHighWater(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	objects, err := fence.Scan(context.Background(), ContentFenceScanRequest{TenantID: "tenant", HighWater: highWater})
	if err != nil || len(objects) != 2 {
		t.Fatalf("objects=%d err=%v", len(objects), err)
	}
	if _, err := fence.Scan(context.Background(), ContentFenceScanRequest{TenantID: "tenant"}); err == nil {
		t.Fatal("missing high-water accepted")
	}
	if _, _, err := fence.Identity("other"); err == nil {
		t.Fatal("wrong tenant accepted")
	}
}

func TestAzureContentFenceRejectsMissingHighWaterWithoutListing(t *testing.T) {
	fake := newContentBlobServer()
	server := newContentBlobHTTPServer(t, fake)
	defer server.Close()
	fence := newTestAzureContentFence(t, server.URL, time.Second)
	if fence.Durable() == false {
		t.Fatal("configured Azure fence is not durable")
	}
	if _, err := fence.Scan(context.Background(), ContentFenceScanRequest{TenantID: "tenant"}); err == nil {
		t.Fatal("missing high-water accepted")
	}
	if fake.listCalls != 0 {
		t.Fatalf("missing high-water made %d list calls", fake.listCalls)
	}
}

func TestAzureContentFenceConfigurationAndContext(t *testing.T) {
	client, err := azblobTestClient("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	base := AzureContentFenceConfig{ServiceURL: "http://127.0.0.1:1", ContainerName: "private", TenantID: "tenant", EnvironmentDigest: "env-test", Timeout: time.Second}
	if _, err := NewAzureContentFenceWithClient(context.Background(), base, client); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]AzureContentFenceConfig{
		"no container":    {ServiceURL: base.ServiceURL, TenantID: base.TenantID, EnvironmentDigest: base.EnvironmentDigest, Timeout: base.Timeout},
		"no tenant":       {ServiceURL: base.ServiceURL, ContainerName: base.ContainerName, EnvironmentDigest: base.EnvironmentDigest, Timeout: base.Timeout},
		"no environment":  {ServiceURL: base.ServiceURL, ContainerName: base.ContainerName, TenantID: base.TenantID, Timeout: base.Timeout},
		"no timeout":      {ServiceURL: base.ServiceURL, ContainerName: base.ContainerName, TenantID: base.TenantID, EnvironmentDigest: base.EnvironmentDigest},
		"overlarge bound": {ServiceURL: base.ServiceURL, ContainerName: base.ContainerName, TenantID: base.TenantID, EnvironmentDigest: base.EnvironmentDigest, Timeout: base.Timeout, MaxScanObjects: defaultContentFenceScanObjects + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAzureContentFenceWithClient(context.Background(), cfg, client); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewAzureContentFenceWithClient(canceled, base, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled constructor error=%v", err)
	}
}

func bytesContainsAny(body []byte, terms ...[]byte) bool {
	for _, term := range terms {
		if strings.Contains(string(body), string(term)) {
			return true
		}
	}
	return false
}
