package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

type exportEvidenceTool struct{}

func ExportEvidence() mcpserver.Tool    { return exportEvidenceTool{} }
func (exportEvidenceTool) Name() string { return "workiva_export_evidence" }
func (exportEvidenceTool) Description() string {
	return "Exports exactly one authorized assurance subject as a manifest-v2 package with authoritative canonical JSON and an optional safe CSV derivative."
}

type exportEvidenceInput struct {
	SubjectKind       string `json:"subject_kind"`
	SubjectID         string `json:"subject_id"`
	Format            string `json:"format"`
	RedactionProfile  string `json:"redaction_profile"`
	IncludeAuditChain bool   `json:"include_audit_chain"`
	RetentionClass    string `json:"retention_class"`
	IdempotencyKey    string `json:"idempotency_key"`
}

func (exportEvidenceTool) RegisterSDK(server *mcp.Server, deps mcpserver.Deps) {
	mcp.AddTool(server, &mcp.Tool{Name: "workiva_export_evidence", Description: exportEvidenceTool{}.Description(), InputSchema: exportEvidenceInputSchema(), OutputSchema: exportEvidenceOutputSchema()}, func(ctx context.Context, request *mcp.CallToolRequest, input exportEvidenceInput) (*mcp.CallToolResult, assurance.EvidenceResponse, error) {
		if deps.Assurance == nil {
			err := failMsgWithContext(ctx, "assurance store is not available", "enable and provision Phase 3 assurance")
			return structuredToolResult(ctx, err, ""), evidenceErrorResponse(ctx, err), nil
		}
		if deps.Cfg != nil && !deps.Cfg.AssuranceEnabled {
			err := failMsgWithContext(ctx, "assurance evidence export feature is disabled", "set NL_ASSURANCE_ENABLED=true and provision a signed bundle")
			return structuredToolResult(ctx, err, ""), evidenceErrorResponse(ctx, err), nil
		}
		if _, ok := identityForTool(ctx); !ok {
			err := &assurance.Error{Code: "strong_identity_required", Message: "strong Entra identity is required for evidence export"}
			return structuredToolResult(ctx, err, ""), evidenceErrorResponse(ctx, err), nil
		}
		ctx = assurance.WithRequestID(ctx, mcpserver.RequestIDFromContext(ctx))
		response, err := deps.Assurance.ExportEvidence(ctx, mcpserver.ActorFromContextOrRequest(ctx, request, deps.ActorHeader), mcpserver.AuditIDFromContext(ctx), assurance.EvidenceRequest{SubjectKind: input.SubjectKind, SubjectID: input.SubjectID, Format: input.Format, RedactionProfile: assurance.RedactionProfile(input.RedactionProfile), IncludeAuditChain: input.IncludeAuditChain, RetentionClass: input.RetentionClass, IdempotencyKey: input.IdempotencyKey})
		if err != nil {
			return structuredToolResult(ctx, err, "verify the tenant-owned subject, retention policy, and evidence authorization"), evidenceErrorResponse(ctx, err), nil
		}
		return nil, response, nil
	})
}

func evidenceErrorResponse(ctx context.Context, err error) assurance.EvidenceResponse {
	_, status := structuredFailure(err, mcpserver.AuditIDFromContext(ctx))
	return assurance.EvidenceResponse{
		NLAuditID: mcpserver.AuditIDFromContext(ctx), Status: status, ManifestVersion: 1,
		EvidenceManifestID: "not_created", PackageHash: "unknown", ExpiresAt: "unknown",
		Artifacts: []assurance.EvidenceArtifact{}, Audit: assurance.AuditManifest{
			Integrity:    assurance.AuditIntegrity{Scope: "unknown", ChainVerified: false, HashVersionCoverage: []assurance.HashVersionCoverage{}, TenantIdentityHashed: false},
			Checkpoint:   assurance.AuditCheckpoint{Status: "not_requested"},
			Completeness: assurance.AuditCompleteness{Status: "unknown", OmissionsRecorded: false, TerminalAnchor: "unknown"},
			Caveats:      []string{},
		},
	}
}

func exportEvidenceInputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"subject_kind": boundedEnum("snapshot", "validation_run", "comparison", "transfer"), "subject_id": boundedSchema("one tenant-owned subject ID", 128), "format": boundedEnum("json", "csv"), "redaction_profile": boundedEnum("standard", "strict"), "include_audit_chain": map[string]any{"type": "boolean"}, "retention_class": boundedSchema("server-owned retention class", 64), "idempotency_key": boundedSchema("single-use idempotency key", 256),
	}, "required": []string{"subject_kind", "subject_id", "format", "redaction_profile", "include_audit_chain", "retention_class", "idempotency_key"}}
}

func exportEvidenceOutputSchema() map[string]any {
	artifact := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"artifact_id": boundedSchema("artifact ID", 128), "name": boundedSchema("bounded artifact name", 128), "media_type": boundedSchema("media type", 128), "byte_count": boundedInteger(0, 1<<30), "sha256": boundedSchema("artifact SHA-256", 64), "storage_ref": boundedSchema("opaque storage reference", 512)}, "required": []string{"artifact_id", "name", "media_type", "byte_count", "sha256", "storage_ref"}}
	coverage := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"hash_version": boundedInteger(1, 2), "first_seq": boundedInteger(0, 2147483647), "last_seq": boundedInteger(0, 2147483647), "tenant_identity_hashed": map[string]any{"type": "boolean"}}, "required": []string{"hash_version", "first_seq", "last_seq", "tenant_identity_hashed"}}
	integrity := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"scope": boundedSchema("audit scope", 32), "chain_verified": map[string]any{"type": "boolean"}, "hash_version_coverage": map[string]any{"type": "array", "maxItems": 4, "items": coverage}, "tenant_identity_hashed": map[string]any{"type": "boolean"}}, "required": []string{"chain_verified", "hash_version_coverage", "tenant_identity_hashed"}}
	checkpoint := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"status": boundedEnum("verified", "missing", "mismatch", "unverified", "not_requested"), "checkpoint_id": boundedSchema("checkpoint ID", 128), "sequence": boundedInteger(0, 2147483647), "hash": boundedSchema("checkpoint hash", 64), "external_anchor": boundedSchema("external checkpoint anchor", 256)}, "required": []string{"status"}}
	completeness := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"status": boundedEnum("complete", "incomplete", "unknown"), "expected_count": boundedInteger(0, 2147483647), "included_count": boundedInteger(0, 2147483647), "omitted_count": boundedInteger(0, 2147483647), "expected_event_count": boundedInteger(0, 2147483647), "included_event_count": boundedInteger(0, 2147483647), "omission_count": boundedInteger(0, 2147483647), "omissions_recorded": map[string]any{"type": "boolean"}, "terminal_anchor": boundedSchema("terminal anchor state", 32), "terminal_anchor_verified": map[string]any{"type": "boolean"}, "final_row_deletion_detectable": map[string]any{"type": "boolean"}}, "required": []string{"status", "expected_count", "included_count", "omitted_count", "omissions_recorded", "terminal_anchor", "terminal_anchor_verified", "final_row_deletion_detectable"}}
	audit := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"first_seq": boundedInteger(0, 2147483647), "last_seq": boundedInteger(0, 2147483647), "first_hash": boundedSchema("first audit hash", 64), "last_hash": boundedSchema("last audit hash", 64), "integrity": integrity, "checkpoint": checkpoint, "completeness": completeness, "caveats": map[string]any{"type": "array", "maxItems": 16, "items": boundedSchema("audit caveat", 512)}}, "required": []string{"integrity", "checkpoint", "completeness", "caveats"}}
	optionalString := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"nl_audit_id": boundedSchema("audit ID", 128), "status": boundedEnum("completed", "too_large", "denied", "error", "idempotency_replay", "idempotency_in_progress", "idempotency_conflict"), "error": structuredErrorSchema(), "evidence_manifest_id": optionalString(128), "manifest_version": boundedInteger(1, 2), "artifacts": map[string]any{"type": "array", "maxItems": 3, "items": artifact}, "package_hash": optionalString(64), "audit": audit, "expires_at": optionalString(64)}, "required": []string{"nl_audit_id", "status"}}
}
