package tools

import "testing"

func TestTransferContractAcceptsTheSixBoundedPhaseShapes(t *testing.T) {
	for _, phase := range []string{"stage", "confirm", "acknowledge", "reconcile", "bulk_stage", "bulk_confirm"} {
		t.Run(phase, func(t *testing.T) {
			input, ignored, err := validateTransferContract(transferValidArguments(phase))
			if err != nil {
				t.Fatalf("validateTransferContract: %v", err)
			}
			if input.Phase != phase || len(ignored) != 0 {
				t.Fatalf("validated phase=%q ignored=%#v", input.Phase, ignored)
			}
		})
	}
}

func TestTransferContractRejectsForbiddenFieldsAndTypedValueMismatch(t *testing.T) {
	args := transferValidArguments("confirm")
	args["source"] = map[string]any{"mapping_id": "not-accepted-in-confirm"}
	if _, _, err := validateTransferContract(args); err == nil || err.(transferContractError).Code != "forbidden_field" {
		t.Fatalf("forbidden confirm field error=%v", err)
	}

	args = transferValidArguments("acknowledge")
	args["observation"] = "matches"
	args["refreshed"] = true
	args["ui_location"] = map[string]any{"resource_id": "resource-opaque", "locator": "Sheet 1!B2"}
	args["observed_value"] = map[string]any{"kind": "boolean", "number": "not-boolean"}
	if _, _, err := validateTransferContract(args); err == nil || err.(transferContractError).Code != "invalid_typed_value" {
		t.Fatalf("typed value mismatch error=%v", err)
	}
}

func TestTransferContractReconcileClassificationAndBulkCaps(t *testing.T) {
	for _, action := range []string{"classify", "close"} {
		args := transferValidArguments("reconcile")
		args["action"] = action
		args["classification"] = "still_unknown"
		args["note"] = "operator reviewed evidence"
		if _, _, err := validateTransferContract(args); err != nil {
			t.Fatalf("%s classification rejected: %v", action, err)
		}
	}
	args := transferValidArguments("reconcile")
	args["action"] = "inspect_operation"
	args["classification"] = "still_unknown"
	if _, _, err := validateTransferContract(args); err == nil || err.(transferContractError).Code != "forbidden_field" {
		t.Fatalf("inspect_operation classification err=%v", err)
	}
	args = transferValidArguments("bulk_stage")
	args["max_rows"] = 100
	args["max_cells"] = 100
	if _, _, err := validateTransferContract(args); err != nil {
		t.Fatalf("bounded bulk_stage caps rejected: %v", err)
	}
}

func TestTransferContractCapsOpaqueIDsAndDoesNotEchoSecrets(t *testing.T) {
	args := transferValidArguments("confirm")
	args["transfer_id"] = " transfer-with-surrounding-space "
	_, _, err := validateTransferContract(args)
	if err == nil || err.(transferContractError).Code != "opaque_id_invalid" {
		t.Fatalf("opaque ID error=%v", err)
	}

	args = transferValidArguments("stage")
	args["idempotency_key"] = "key"
	args["source"] = map[string]any{"mapping_id": "mapping-opaque"}
	args["confirmation_token"] = "secret-token"
	_, _, err = validateTransferContract(args)
	if err == nil || err.(transferContractError).Code != "forbidden_field" {
		t.Fatalf("raw token handling error=%v", err)
	}
}
