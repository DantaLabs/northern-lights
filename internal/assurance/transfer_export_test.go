package assurance

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
)

func TestTransferRouteExportProfileIsExactAndTransferPermitted(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		profile ExportProfile
		ref     ExportProfileReference
		wantErr bool
	}{
		{name: "exact transfer profile", profile: ExportProfile{ProfileID: "transfer-profile", Revision: 3, Status: "active", PermittedSubjects: []string{"transfer"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 100, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}, ref: ExportProfileReference{ProfileID: "transfer-profile", Revision: 3}},
		{name: "missing exact revision", profile: ExportProfile{ProfileID: "transfer-profile", Revision: 3, Status: "active", PermittedSubjects: []string{"transfer"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 100, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}, ref: ExportProfileReference{ProfileID: "transfer-profile", Revision: 2}, wantErr: true},
		{name: "profile excludes transfer", profile: ExportProfile{ProfileID: "transfer-profile", Revision: 3, Status: "active", PermittedSubjects: []string{"snapshot"}, RedactionProfile: "standard", RetentionClass: "long_term", MaxRows: 100, MaxBytes: 1 << 20, DeliveryPolicy: "opaque_reference"}, ref: ExportProfileReference{ProfileID: "transfer-profile", Revision: 3}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, sig := signedBundle(t, private, func(b *Bundle) {
				r := validTestRoute()
				r.ExportProfiles = []ExportProfileReference{tc.ref}
				b.TransferRoutes = []TransferRoute{r}
				b.ConversionPolicies = []ConversionPolicy{{PolicyID: "exact", Revision: 1, Mode: "typed_copy", SourceKind: ValueNumber, TargetKind: ValueNumber}}
				b.ExportProfiles = []ExportProfile{tc.profile}
			})
			_, err := ValidateBundle(raw, sig, testTenant, public)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateBundle err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestTransferEvidenceCSVFormulaEscapeIsNonAuthoritativeDerivative(t *testing.T) {
	raw, err := CanonicalJSON(map[string]any{"transfer_id": "t-1", "intent": map[string]any{"before": map[string]any{"kind": "text", "text": "=HYPERLINK(\"https://invalid\")"}}})
	if err != nil {
		t.Fatal(err)
	}
	csv := deterministicCSV(raw)
	if !strings.Contains(string(csv), "'=HYPERLINK") {
		t.Fatalf("transfer CSV did not formula-escape the typed text: %s", csv)
	}
}

func TestSafeTransferOperationReferenceAllowsOnlyProviderHTTPSOrOpaqueIDs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "provider HTTPS operation", value: "https://provider.invalid/operations/op-202", want: "https://provider.invalid/operations/op-202"},
		{name: "bounded opaque provider operation id", value: "provider-operation-202", want: "provider-operation-202"},
		{name: "empty", value: "", want: ""},
		{name: "windows drive path", value: `C:\\private\\backup.db`, want: ""},
		{name: "https credentials", value: "https://user:secret@provider.invalid/operations/op-202", want: ""},
		{name: "https query", value: "https://provider.invalid/operations/op-202?token=secret", want: ""},
		{name: "https fragment", value: "https://provider.invalid/operations/op-202#secret", want: ""},
		{name: "data scheme", value: "data:text/plain,secret", want: ""},
		{name: "mailto scheme", value: "mailto:operator@example.invalid", want: ""},
		{name: "file scheme", value: "file:///private/backup.db", want: ""},
		{name: "relative path", value: "/operations/op-202", want: ""},
		{name: "opaque id with path syntax", value: "provider/operation-202", want: ""},
		{name: "opaque id with scheme syntax", value: "provider:operation-202", want: ""},
		{name: "opaque id with control", value: "provider-operation-202\nsecret", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeTransferOperationReference(tc.value); got != tc.want {
				t.Fatalf("safeTransferOperationReference(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestTransferEvidenceTypedValuesRequireSingleCanonicalJSONValue(t *testing.T) {
	for _, raw := range []string{
		`{"formula":false,"kind":"number","number":"2"} {}`,
		`{"kind":"number","number":"2","formula":false}`,
		`{"formula":false,"kind":"number","number":"2","token":"secret"}`,
	} {
		if _, err := decodeTransferEvidenceValue(raw); !containsDomainCode(err, "subject_integrity_failed") {
			t.Errorf("noncanonical or open typed value accepted: %s err=%v", raw, err)
		}
	}
}

func TestTransferEvidenceSubjectProjectsTenantScopedAllowlist(t *testing.T) {
	store, db := openTestStore(t)
	route := validTestRoute()
	route.RouteID, route.Revision = "route-1", 2
	route.SourceLocator, route.TargetLocator = "Data!A1", "Data!B1"
	canonicalRoute, err := CanonicalJSON(transferRouteContent(route))
	if err != nil {
		t.Fatal(err)
	}
	route.ContentHash = digestHex(HashBytes(canonicalRoute))
	routeJSON, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_transfer_route_revisions(tenant_id,route_id,revision,content_hash,route_json) VALUES(?,?,?,?,?)`, testTenant, route.RouteID, route.Revision, route.ContentHash, string(routeJSON)); err != nil {
		t.Fatal(err)
	}
	intent := `{"transfer_id":"transfer-1","tenant_id":"` + testTenant + `","actor_id":"actor-1","permission":"workiva.write.confirm","route_id":"route-1","route_revision":2,"mapping_id":"mapping-energy","source":{"resource_id":"source-file","sheet_id":"source-sheet","locator":"Data!A1","fingerprint":"source-fingerprint"},"target":{"resource_id":"target-file","sheet_id":"target-sheet","locator":"Data!B1","fingerprint":"target-fingerprint"},"before":"{\"formula\":false,\"kind\":\"number\",\"number\":\"1\"}","intended":"{\"formula\":false,\"kind\":\"number\",\"number\":\"2\"}","policy_id":"exact","policy_revision":1,"policy_hash":"policy-hash","mapping_hash":"` + route.ContentHash + `","expires_at":"2026-10-08T00:00:00Z","token":"should-not-export","private_key":"should-not-export"}`
	if _, err := db.Exec(`INSERT INTO transfer_intents(tenant_id,transfer_id,actor_id,permission,intent_json,state,token_digest,idempotency_digest,request_digest,expires_at,operation_reference,operation_completed,machine_outcome,machine_outcome_provenance,visual_state,claim_fence_digest,terminal_fence_digest) VALUES(?,?,?,?,?,'visually_acknowledged','token-digest','idem-digest','request-digest','2026-10-08T00:00:00Z','opaque-operation',1,'applied','uncached_readback','acknowledged','claim-digest','terminal-digest')`, testTenant, "transfer-1", "actor-1", "workiva.write.confirm", intent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO assurance_transfer_readbacks(tenant_id,transfer_id,readback_id,typed_value_json,cache_bypassed,observed_at) VALUES(?,?,'readback-1','{"formula":false,"kind":"number","number":"2"}',1,'2026-10-07T00:02:00Z')`, testTenant, "transfer-1"); err != nil {
		t.Fatal(err)
	}
	raw, err := store.subjectJSON(assuranceContext(), "transfer", "transfer-1")
	if err != nil {
		t.Fatalf("transfer subject projection failed: %v", err)
	}
	if _, err := store.resolveTransferExportProfile(assuranceContext(), testTenant, "transfer-1", RedactionStandard, "long_term"); !containsDomainCode(err, "export_profile_not_approved") {
		t.Fatalf("legacy route without a frozen profile association must fail closed, got %v", err)
	}
	for _, want := range []string{`"route_id":"route-1"`, `"source-fingerprint"`, `"target-fingerprint"`, `"operation_reference":"opaque-operation"`, `"terminal_fence_digest":"terminal-digest"`, `"uncached_readbacks"`, `"cache_bypassed":true`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("transfer projection missing %s: %s", want, raw)
		}
	}
	for _, forbidden := range []string{"should-not-export", "token-digest", "idem-digest", "request-digest"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("transfer projection leaked %q: %s", forbidden, raw)
		}
	}
	strict, err := redactCanonical(raw, RedactionStrict)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"before"`, `"intended"`, `"uncached_readbacks"`, `"number":"2"`} {
		if strings.Contains(string(strict), forbidden) {
			t.Errorf("strict transfer redaction retained %q: %s", forbidden, strict)
		}
	}
	redactions := collectRedactionRecords(raw, RedactionStrict)
	seen := map[string]bool{}
	for _, item := range redactions {
		seen[item.Field] = true
	}
	for _, required := range []string{"intent.before", "intent.intended", "uncached_readbacks"} {
		if !seen[required] {
			t.Errorf("strict manifest redactions omitted %q: %+v", required, redactions)
		}
	}
}
