package changeset

import (
	"context"
	"testing"

	"github.com/dantalabs/northern-lights/internal/demoenv"
)

func TestNewStubReturnsNotImplemented(t *testing.T) {
	svc := New(demoenv.Env{})
	if _, err := svc.Stage(context.Background(), "actor", []Item{{FieldName: "f"}}); err == nil {
		t.Fatal("Stage: want not-implemented error")
	}
	if _, err := svc.Confirm(context.Background(), "actor", "id", "token"); err == nil {
		t.Fatal("Confirm: want not-implemented error")
	}
}
