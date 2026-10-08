package transfer

import (
	"context"
	"testing"
	"time"
)

func TestReadbackCannotVerifyBeforeKnownCompletedOperation(t *testing.T) {
	s, _ := testStore(t)
	i := testIntent()
	if _, err := s.Stage(context.Background(), i, "token", "key", "req", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(context.Background(), Claim{TenantID: i.TenantID, ActorID: i.ActorID, Permission: i.Permission, TransferID: i.ID, Token: "token", LeaseID: "lease", Now: time.Now(), LeaseUntil: time.Now().Add(time.Minute)}); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := s.RecordReadback(context.Background(), i.TenantID, i.ID, "premature", i.Intended, true, time.Now()); err == nil {
		t.Fatal("equal read-back before provider submission was accepted")
	}
	got, err := s.Get(context.Background(), i.TenantID, i.ID)
	if err != nil || got.MachineOutcome == "api_verified" {
		t.Fatalf("premature read-back produced verification: %+v %v", got, err)
	}
}

func TestReconciliationRequiresPersistedReadbackNotCallerBoolean(t *testing.T) {
	s, _ := testStore(t)
	i := testIntent()
	if _, err := s.Stage(context.Background(), i, "token", "key", "req", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Claim(context.Background(), Claim{TenantID: i.TenantID, ActorID: i.ActorID, Permission: i.Permission, TransferID: i.ID, Token: "token", LeaseID: "lease", Now: time.Now(), LeaseUntil: time.Now().Add(-time.Second)}); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	if err := s.QuarantineExpired(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyReconciliation(context.Background(), i.TenantID, i.ID, "confirmed_applied", i.Intended, true, time.Now()); err == nil {
		t.Fatal("caller boolean alone confirmed applied")
	}
}
