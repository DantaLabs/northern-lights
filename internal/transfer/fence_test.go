package transfer

import (
	"context"
	"testing"
)

func TestFakeFenceCreateOnlyAndFailureClosed(t *testing.T) {
	f := &FakeFence{Claims: map[string]string{}, Terminals: map[string]string{}}
	body := []byte(`{"transfer":"x","intent":"frozen"}`)
	f.Fail = "claim"
	if _, err := f.CreateClaim(context.Background(), "x", body); err == nil {
		t.Fatal("expected claim fence failure")
	}
	if len(f.Claims) != 0 {
		t.Fatal("failed claim fence was persisted")
	}
	f.Fail = ""
	d, err := f.CreateClaim(context.Background(), "x", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.VerifyClaim(context.Background(), "x", d); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateClaim(context.Background(), "x", []byte(`different`)); err == nil {
		t.Fatal("mismatched existing claim fence adopted")
	}
	if err := f.VerifyClaim(context.Background(), "x", d); err != nil {
		t.Fatal("existing fence overwritten", err)
	}
	f.Fail = "terminal"
	if _, err := f.CreateTerminal(context.Background(), "x", body); err == nil {
		t.Fatal("expected terminal fence failure")
	}
	if len(f.Terminals) != 0 {
		t.Fatal("failed terminal fence was persisted")
	}
	f.Fail = ""
	td, err := f.CreateTerminal(context.Background(), "x", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.VerifyTerminal(context.Background(), "x", td); err != nil {
		t.Fatal(err)
	}
}
