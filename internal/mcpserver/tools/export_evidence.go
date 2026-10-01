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
			return nil, assurance.EvidenceResponse{}, failMsg("assurance store is not available", "enable and provision Phase 3 assurance")
		}
		if deps.Cfg != nil && !deps.Cfg.AssuranceEnabled {
			return nil, assurance.EvidenceResponse{}, failMsg("assurance evidence export feature is disabled", "set NL_ASSURANCE_ENABLED=true and provision a signed bundle")
		}
		if _, ok := identityForTool(ctx); !ok {
			return nil, assurance.EvidenceResponse{}, failMsg("strong Entra identity is required for evidence export", "authenticate with a validated Entra tid and oid")
		}
		response, err := deps.Assurance.ExportEvidence(ctx, mcpserver.ActorFromContextOrRequest(ctx, request, deps.ActorHeader), mcpserver.AuditIDFromContext(ctx), assurance.EvidenceRequest{SubjectKind: input.SubjectKind, SubjectID: input.SubjectID, Format: input.Format, RedactionProfile: assurance.RedactionProfile(input.RedactionProfile), IncludeAuditChain: input.IncludeAuditChain, RetentionClass: input.RetentionClass, IdempotencyKey: input.IdempotencyKey})
		if err != nil {
			return nil, assurance.EvidenceResponse{}, fail(err, "verify the tenant-owned subject, retention policy, and evidence authorization")
		}
		return nil, response, nil
	})
}

func exportEvidenceInputSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"subject_kind": map[string]any{"type": "string", "enum": []string{"snapshot", "validation_run", "comparison", "transfer"}}, "subject_id": boundedSchema("one tenant-owned subject ID", 128), "format": map[string]any{"type": "string", "enum": []string{"json", "csv"}}, "redaction_profile": map[string]any{"type": "string", "enum": []string{"standard", "strict"}}, "include_audit_chain": map[string]any{"type": "boolean"}, "retention_class": boundedSchema("server-owned retention class", 64), "idempotency_key": boundedSchema("single-use idempotency key", 256),
	}, "required": []string{"subject_kind", "subject_id", "format", "redaction_profile", "include_audit_chain", "retention_class", "idempotency_key"}}
}

func exportEvidenceOutputSchema() map[string]any {
	artifact := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"artifact_id": boundedSchema("artifact ID", 128), "name": boundedSchema("bounded artifact name", 128), "media_type": boundedSchema("media type", 128), "byte_count": map[string]any{"type": "integer"}, "sha256": boundedSchema("artifact SHA-256", 64), "storage_ref": boundedSchema("opaque storage reference", 256)}, "required": []string{"artifact_id", "name", "media_type", "byte_count", "sha256", "storage_ref"}}
	coverage := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"hash_version": map[string]any{"type": "integer"}, "first_seq": map[string]any{"type": "integer"}, "last_seq": map[string]any{"type": "integer"}, "tenant_identity_hashed": map[string]any{"type": "boolean"}}, "required": []string{"hash_version", "first_seq", "last_seq", "tenant_identity_hashed"}}
	integrity := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"scope": boundedSchema("audit scope", 32), "chain_verified": map[string]any{"type": "boolean"}, "hash_version_coverage": map[string]any{"type": "array", "maxItems": 4, "items": coverage}, "tenant_identity_hashed": map[string]any{"type": "boolean"}}, "required": []string{"chain_verified", "hash_version_coverage", "tenant_identity_hashed"}}
	checkpoint := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"verified", "missing", "mismatch", "not_requested"}}, "checkpoint_id": boundedSchema("checkpoint ID", 128), "sequence": map[string]any{"type": "integer"}, "hash": boundedSchema("checkpoint hash", 64), "external_anchor": boundedSchema("external checkpoint anchor", 256)}, "required": []string{"status", "checkpoint_id", "sequence", "hash", "external_anchor"}}
	completeness := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"complete", "incomplete", "unknown"}}, "expected_count": map[string]any{"type": "integer"}, "included_count": map[string]any{"type": "integer"}, "omitted_count": map[string]any{"type": "integer"}, "expected_event_count": map[string]any{"type": "integer"}, "included_event_count": map[string]any{"type": "integer"}, "omission_count": map[string]any{"type": "integer"}, "omissions_recorded": map[string]any{"type": "boolean"}, "terminal_anchor": boundedSchema("terminal anchor state", 32), "terminal_anchor_verified": map[string]any{"type": "boolean"}, "final_row_deletion_detectable": map[string]any{"type": "boolean"}}, "required": []string{"status", "expected_count", "included_count", "omitted_count", "omissions_recorded", "terminal_anchor", "terminal_anchor_verified", "final_row_deletion_detectable"}}
	audit := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"first_seq": map[string]any{"type": "integer"}, "last_seq": map[string]any{"type": "integer"}, "first_hash": boundedSchema("first audit hash", 64), "last_hash": boundedSchema("last audit hash", 64), "integrity": integrity, "checkpoint": checkpoint, "completeness": completeness, "caveats": map[string]any{"type": "array", "maxItems": 16, "items": boundedSchema("audit caveat", 512)}}, "required": []string{"integrity", "checkpoint", "completeness", "caveats"}}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"nl_audit_id": boundedSchema("audit ID", 128), "status": map[string]any{"type": "string", "enum": []string{"completed", "too_large", "denied", "error", "idempotency_replay"}}, "evidence_manifest_id": boundedSchema("manifest ID", 128), "manifest_version": map[string]any{"type": "integer"}, "artifacts": map[string]any{"type": "array", "maxItems": 3, "items": artifact}, "package_hash": boundedSchema("package SHA-256", 64), "audit": audit, "expires_at": boundedSchema("expiry time", 64)}, "required": []string{"nl_audit_id", "status", "evidence_manifest_id", "manifest_version", "artifacts", "package_hash", "audit", "expires_at"}}
}
