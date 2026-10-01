package assurance

import (
	"context"
	"testing"
)

func TestEvidenceRequiresValidatedPrincipal(t *testing.T) {
	store, _ := openTestStore(t)
	_, err := store.ExportEvidence(context.Background(), "caller", "audit", EvidenceRequest{SubjectKind: "snapshot", SubjectID: "s", IdempotencyKey: "k"})
	if err == nil || !containsDomainCode(err, "strong_identity_required") {
		t.Fatalf("err=%v", err)
	}
}

func containsDomainCode(err error, code string) bool {
	typed, ok := err.(*Error)
	return ok && typed.Code == code
}
