package transfer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/identity"
)

func b1TrustedConfirmContext(tenant string) context.Context {
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: tenant, ObjectID: "actor-1",
		Permissions: []identity.Permission{identity.PermissionWorkivaWritePreview, identity.PermissionWorkivaWriteConfirm},
	})
}

func TestB1ConfirmNeedsTrustedPrincipal(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, f)
	_, err := svc.Confirm(context.Background(), ConfirmRequest{
		TenantID: tenant, ActorID: actor, Permission: permission,
		TransferID: staged.TransferID, Token: staged.Token, Now: time.Now(),
		IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	if err == nil || f.posts.Load() != 0 {
		t.Fatalf("unauthenticated confirm err=%v POSTs=%d", err, f.posts.Load())
	}
}

func TestB1StageNeedsTrustedPreviewCapability(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	ctx := identity.ContextWithPrincipal(context.Background(), identity.Principal{
		TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "actor-1",
	})
	if _, err := svc.Stage(ctx, StageRequest{RouteID: "energy-copy", IdempotencyDigest: "b1-stage-denied", RequestDigest: "req", Now: time.Now()}); err == nil {
		t.Fatal("stage without preview capability was accepted")
	}
	if f.posts.Load() != 0 {
		t.Fatalf("denied stage submitted %d provider writes", f.posts.Load())
	}
}

func TestB1ConfirmNeedsTrustedConfirmCapability(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	staged, tenant, actor, permission := stageForConfirm(t, svc, f)
	_, err := svc.Confirm(f.ctx, ConfirmRequest{
		TenantID: tenant, ActorID: actor, Permission: permission,
		TransferID: staged.TransferID, Token: staged.Token, Now: time.Now(),
		IdempotencyDigest: "test-confirm-key", RequestDigest: "test-confirm-request"})
	if err == nil || f.posts.Load() != 0 {
		t.Fatalf("preview-only confirm err=%v POSTs=%d", err, f.posts.Load())
	}
}

func TestB1TransferIDDoesNotContainConfirmationTokenPrefix(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	staged, tenant, _, _ := stageForConfirm(t, svc, f)
	var raw string
	if err := svc.store.db.QueryRow(`SELECT intent_json FROM transfer_intents WHERE tenant_id=? AND transfer_id=?`, tenant, staged.TransferID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(staged.Token) >= 24 && strings.Contains(raw, staged.Token[:24]) {
		t.Fatal("persisted intent contains a raw confirmation-token prefix")
	}
}

func TestB1DecodeScalarSupportsTextBooleanAndDecimal(t *testing.T) {
	tests := []struct {
		raw  string
		want any
	}{
		{raw: `"ordinary text"`, want: "ordinary text"},
		{raw: `true`, want: true},
		{raw: `false`, want: false},
		{raw: `2.0`, want: json.Number("2")},
	}
	for _, tt := range tests {
		got, err := decodeScalar(tt.raw)
		if err != nil {
			t.Errorf("decodeScalar(%s): %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("decodeScalar(%s)=%#v, want %#v", tt.raw, got, tt.want)
		}
	}
}

func TestB1CanonicalProviderDecimalEquivalence(t *testing.T) {
	for _, input := range []string{"2", "2.0", "2.00", "002.000"} {
		got, err := canonicalProviderValue(assurance.ProviderValue{Value: json.Number(input)})
		if err != nil {
			t.Errorf("canonicalProviderValue(%s): %v", input, err)
			continue
		}
		if got != "2" {
			t.Errorf("canonicalProviderValue(%s)=%s, want 2", input, got)
		}
	}
}

func TestB1StagePersistsCanonicalTypedIntent(t *testing.T) {
	svc, f, _ := newServiceFixture(t)
	staged, tenant, _, _ := stageForConfirm(t, svc, f)
	got, err := svc.store.Get(context.Background(), tenant, staged.TransferID)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"before": got.Intent.Before, "intended": got.Intent.Intended} {
		var value map[string]any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatalf("%s intent is not JSON: %v", name, err)
		}
		if value["kind"] != "number" || value["formula"] != false {
			t.Errorf("%s intent=%s, want canonical typed number", name, raw)
		}
	}
}

func TestB1CanonicalIntentPreservesTextBooleanAndDecimalKinds(t *testing.T) {
	tests := []struct {
		name  string
		input assurance.ProviderValue
		field assurance.FieldDefinition
		check func(t *testing.T, raw string)
	}{
		{
			name:  "text",
			input: assurance.ProviderValue{Value: "ordinary text"},
			field: assurance.FieldDefinition{Kind: assurance.ValueText},
			check: func(t *testing.T, raw string) {
				if raw != `{"formula":false,"kind":"text","text":"ordinary text"}` {
					t.Fatalf("canonical text=%s", raw)
				}
			},
		},
		{
			name:  "false",
			input: assurance.ProviderValue{Value: false},
			field: assurance.FieldDefinition{Kind: assurance.ValueBoolean},
			check: func(t *testing.T, raw string) {
				if raw != `{"boolean":false,"formula":false,"kind":"boolean"}` {
					t.Fatalf("canonical false=%s", raw)
				}
			},
		},
		{
			name:  "decimal",
			input: assurance.ProviderValue{Value: json.Number("2.00")},
			field: assurance.FieldDefinition{Kind: assurance.ValueNumber},
			check: func(t *testing.T, raw string) {
				if raw != `{"formula":false,"kind":"number","number":"2"}` {
					t.Fatalf("canonical decimal=%s", raw)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typed, err := assurance.NormalizeProviderValue(tt.input, tt.field)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := canonicalTypedValue(typed)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, raw)
		})
	}
}

func TestB1ParseCellLocatorRejectsCheckedArithmeticOverflow(t *testing.T) {
	for _, raw := range []string{"A18446744073709551617", "AAAAAAAAAAAAA1", "A0", "A1:B2"} {
		if column, row, err := parseCellLocator(raw); err == nil {
			t.Errorf("parseCellLocator(%s)=%d,%d accepted", raw, column, row)
		}
	}
}
