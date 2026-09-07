package mapping

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreateAndConsumePendingWriteRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	w := PendingWrite{
		Token:     "tok-1",
		FieldID:   42,
		FieldName: "scope2_energy_kwh",
		Value:     "5678",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.CreatePendingWrite(ctx, w); err != nil {
		t.Fatalf("CreatePendingWrite: %v", err)
	}

	got, err := s.ConsumePendingWrite(ctx, "tok-1", 5*time.Minute)
	if err != nil {
		t.Fatalf("ConsumePendingWrite: %v", err)
	}
	if got == nil {
		t.Fatal("ConsumePendingWrite returned nil, want the stored write")
	}
	if got.FieldID != 42 || got.FieldName != "scope2_energy_kwh" || got.Value != "5678" {
		t.Errorf("consumed write = %+v, want %+v", *got, w)
	}

	// The token is single use: a second consume finds nothing.
	again, err := s.ConsumePendingWrite(ctx, "tok-1", 5*time.Minute)
	if err != nil {
		t.Fatalf("second ConsumePendingWrite: %v", err)
	}
	if again != nil {
		t.Errorf("second consume = %+v, want nil (single use)", *again)
	}
}

func TestConsumePendingWriteUnknownToken(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	got, err := s.ConsumePendingWrite(ctx, "nope", 5*time.Minute)
	if err != nil {
		t.Fatalf("ConsumePendingWrite: %v", err)
	}
	if got != nil {
		t.Errorf("consume of unknown token = %+v, want nil", *got)
	}
}

func TestConsumePendingWriteRejectsExpiredToken(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	w := PendingWrite{
		Token:     "tok-old",
		FieldID:   1,
		FieldName: "old_field",
		Value:     "x",
		CreatedAt: time.Now().Add(-10 * time.Minute).UTC(),
	}
	if err := s.CreatePendingWrite(ctx, w); err != nil {
		t.Fatalf("CreatePendingWrite: %v", err)
	}

	got, err := s.ConsumePendingWrite(ctx, "tok-old", 5*time.Minute)
	if !errors.Is(err, ErrPendingWriteExpired) {
		t.Fatalf("err = %v, want ErrPendingWriteExpired", err)
	}
	if got != nil {
		t.Errorf("expired consume = %+v, want nil", *got)
	}

	// The expired row was deleted by the consume, so a later call with a
	// generous TTL still finds nothing.
	later, err := s.ConsumePendingWrite(ctx, "tok-old", time.Hour)
	if err != nil {
		t.Fatalf("ConsumePendingWrite after expiry cleanup: %v", err)
	}
	if later != nil {
		t.Errorf("consume after expiry = %+v, want nil", *later)
	}
}
