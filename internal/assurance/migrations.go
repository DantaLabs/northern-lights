package assurance

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

const migrationV1 = `
CREATE TABLE assurance_resources (
 tenant_id TEXT NOT NULL, resource_id TEXT NOT NULL, provider TEXT NOT NULL, kind TEXT NOT NULL,
 external_id TEXT NOT NULL, parent_resource_id TEXT NOT NULL DEFAULT '', locator TEXT NOT NULL DEFAULT '',
 provider_revision TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active',
 PRIMARY KEY (tenant_id, resource_id), UNIQUE (tenant_id, provider, kind, external_id)
);
CREATE INDEX idx_assurance_resources_tenant_kind_parent ON assurance_resources(tenant_id, kind, parent_resource_id);
CREATE TABLE assurance_mappings (
 tenant_id TEXT NOT NULL, mapping_id TEXT NOT NULL, resource_id TEXT NOT NULL, stable_field_id TEXT NOT NULL,
 active_revision INTEGER NOT NULL, status TEXT NOT NULL, PRIMARY KEY (tenant_id, mapping_id)
);
CREATE INDEX idx_assurance_mappings_tenant_field ON assurance_mappings(tenant_id, stable_field_id);
CREATE TABLE assurance_mapping_revisions (
 tenant_id TEXT NOT NULL, mapping_id TEXT NOT NULL, revision INTEGER NOT NULL, locator TEXT NOT NULL,
 value_kind TEXT NOT NULL, unit TEXT NOT NULL DEFAULT '', scale TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL,
 created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, mapping_id, revision)
);
CREATE TABLE assurance_relationship_edges (
 tenant_id TEXT NOT NULL, edge_id TEXT NOT NULL, source_resource_id TEXT NOT NULL, target_resource_id TEXT NOT NULL,
 relation TEXT NOT NULL, provenance TEXT NOT NULL, graph_version INTEGER NOT NULL, active INTEGER NOT NULL,
 policy_version TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, edge_id)
);
CREATE INDEX idx_assurance_edges_tenant_source_active ON assurance_relationship_edges(tenant_id, source_resource_id, active);
CREATE TABLE assurance_relationship_discovery_runs (
 tenant_id TEXT NOT NULL, run_id TEXT NOT NULL, scope_digest TEXT NOT NULL, scope_json TEXT NOT NULL,
 status TEXT NOT NULL, completeness TEXT NOT NULL, provider_end_of_scope INTEGER NOT NULL DEFAULT 0,
 page_count INTEGER NOT NULL DEFAULT 0, cursor_count INTEGER NOT NULL DEFAULT 0, error_json TEXT NOT NULL DEFAULT '',
 prior_graph_version INTEGER NOT NULL DEFAULT 0, resulting_graph_version INTEGER NOT NULL DEFAULT 0,
 actor_id TEXT NOT NULL, audit_id TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, terminal_at TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (tenant_id, run_id)
);
CREATE INDEX idx_assurance_discovery_tenant_scope_started ON assurance_relationship_discovery_runs(tenant_id, scope_digest, started_at);
`

const migrationV2 = `
CREATE TABLE assurance_bundle_candidates (
 tenant_id TEXT NOT NULL, bundle_id TEXT NOT NULL, bundle_version INTEGER NOT NULL, schema_version INTEGER NOT NULL,
 bundle_json TEXT NOT NULL, signature_hex TEXT NOT NULL, content_hash TEXT NOT NULL,
 activation_requested INTEGER NOT NULL DEFAULT 0, staged_at TEXT NOT NULL, requested_at TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (tenant_id, bundle_id, bundle_version)
);
CREATE INDEX idx_assurance_candidates_tenant_requested ON assurance_bundle_candidates(tenant_id, activation_requested, requested_at);
CREATE TABLE assurance_active_bundles (
 tenant_id TEXT NOT NULL, singleton INTEGER NOT NULL DEFAULT 1, bundle_id TEXT NOT NULL, bundle_version INTEGER NOT NULL,
 schema_version INTEGER NOT NULL, bundle_json TEXT NOT NULL, signature_hex TEXT NOT NULL, content_hash TEXT NOT NULL,
 activated_at TEXT NOT NULL, PRIMARY KEY (tenant_id, singleton), CHECK(singleton=1)
);
CREATE TABLE assurance_report_definitions (
 tenant_id TEXT NOT NULL, report_id TEXT NOT NULL, name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
 owner TEXT NOT NULL, active_revision INTEGER NOT NULL, status TEXT NOT NULL, PRIMARY KEY (tenant_id, report_id)
);
CREATE TABLE assurance_report_revisions (
 tenant_id TEXT NOT NULL, report_id TEXT NOT NULL, revision INTEGER NOT NULL, name TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL, periods_json TEXT NOT NULL,
 resource_policy_hash TEXT NOT NULL, rule_set_id TEXT NOT NULL DEFAULT '', materiality_policy_id TEXT NOT NULL DEFAULT '',
 export_profiles_json TEXT NOT NULL DEFAULT '[]', retention_class TEXT NOT NULL, effective_from TEXT NOT NULL DEFAULT '',
 effective_to TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL, status TEXT NOT NULL,
 PRIMARY KEY (tenant_id, report_id, revision)
);
CREATE INDEX idx_assurance_report_revisions_tenant_status ON assurance_report_revisions(tenant_id, status, report_id);
CREATE TABLE assurance_report_fields (
 tenant_id TEXT NOT NULL, report_id TEXT NOT NULL, revision INTEGER NOT NULL, field_id TEXT NOT NULL,
 field_order INTEGER NOT NULL, required INTEGER NOT NULL, resource_id TEXT NOT NULL, external_resource_id TEXT NOT NULL,
 subresource_id TEXT NOT NULL, locator TEXT NOT NULL, value_kind TEXT NOT NULL, unit TEXT NOT NULL DEFAULT '',
 scale TEXT NOT NULL DEFAULT '', precision_value INTEGER NOT NULL DEFAULT 0, percent_basis TEXT NOT NULL DEFAULT '',
 timezone TEXT NOT NULL DEFAULT '', mapping_revision INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (tenant_id, report_id, revision, field_id)
);
CREATE INDEX idx_assurance_report_fields_tenant_resource ON assurance_report_fields(tenant_id, external_resource_id, subresource_id);
`

const migrationV3 = `
CREATE TABLE assurance_snapshots (
 tenant_id TEXT NOT NULL, snapshot_id TEXT NOT NULL, idempotency_digest TEXT NOT NULL, report_id TEXT NOT NULL,
 definition_revision INTEGER NOT NULL, period_json TEXT NOT NULL, status TEXT NOT NULL,
 completeness TEXT NOT NULL, mapping_set_hash TEXT NOT NULL, provider_route TEXT NOT NULL,
 content_hash TEXT NOT NULL DEFAULT '', retention_class TEXT NOT NULL, captured_at TEXT NOT NULL DEFAULT '',
 expires_at TEXT NOT NULL DEFAULT '', audit_id TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, snapshot_id),
 UNIQUE (tenant_id, idempotency_digest), CHECK(status IN ('running','completed','partial','failed','expired'))
);
CREATE INDEX idx_assurance_snapshots_tenant_report_period ON assurance_snapshots(tenant_id, report_id, definition_revision);
CREATE TABLE assurance_snapshot_observations (
 tenant_id TEXT NOT NULL, snapshot_id TEXT NOT NULL, observation_id TEXT NOT NULL, field_id TEXT NOT NULL,
 resource_id TEXT NOT NULL, external_resource_id TEXT NOT NULL, locator TEXT NOT NULL,
 typed_value_json TEXT NOT NULL, provider_revision_json TEXT NOT NULL, source_fingerprint TEXT NOT NULL,
 observed_at TEXT NOT NULL, PRIMARY KEY (tenant_id, observation_id),
 UNIQUE (tenant_id, snapshot_id, field_id)
);
CREATE INDEX idx_assurance_observations_tenant_snapshot ON assurance_snapshot_observations(tenant_id, snapshot_id, field_id);
CREATE TABLE assurance_snapshot_failures (
 tenant_id TEXT NOT NULL, snapshot_id TEXT NOT NULL, failure_id TEXT NOT NULL, field_id TEXT NOT NULL,
 code TEXT NOT NULL, message TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, failure_id)
);
CREATE INDEX idx_assurance_failures_tenant_snapshot ON assurance_snapshot_failures(tenant_id, snapshot_id, field_id);
CREATE TRIGGER assurance_snapshot_terminal_immutable BEFORE UPDATE ON assurance_snapshots
 WHEN OLD.status IN ('completed','partial','failed','expired') BEGIN SELECT RAISE(ABORT, 'immutable assurance snapshot'); END;
CREATE TRIGGER assurance_snapshot_no_delete BEFORE DELETE ON assurance_snapshots BEGIN SELECT RAISE(ABORT, 'immutable assurance snapshot'); END;
CREATE TRIGGER assurance_observation_no_update BEFORE UPDATE ON assurance_snapshot_observations BEGIN SELECT RAISE(ABORT, 'immutable assurance observation'); END;
CREATE TRIGGER assurance_observation_no_delete BEFORE DELETE ON assurance_snapshot_observations BEGIN SELECT RAISE(ABORT, 'immutable assurance observation'); END;
CREATE TRIGGER assurance_failure_no_update BEFORE UPDATE ON assurance_snapshot_failures BEGIN SELECT RAISE(ABORT, 'immutable assurance failure'); END;
CREATE TRIGGER assurance_failure_no_delete BEFORE DELETE ON assurance_snapshot_failures BEGIN SELECT RAISE(ABORT, 'immutable assurance failure'); END;
`

const migrationV4 = `
CREATE TABLE assurance_rule_sets (
 tenant_id TEXT NOT NULL, rule_set_id TEXT NOT NULL, revision INTEGER NOT NULL, content_hash TEXT NOT NULL,
 status TEXT NOT NULL, definition_json TEXT NOT NULL, PRIMARY KEY (tenant_id, rule_set_id, revision)
);
CREATE TABLE assurance_rule_set_rules (
 tenant_id TEXT NOT NULL, rule_set_id TEXT NOT NULL, revision INTEGER NOT NULL, rule_id TEXT NOT NULL,
 rule_order INTEGER NOT NULL, rule_json TEXT NOT NULL, PRIMARY KEY (tenant_id, rule_set_id, revision, rule_id)
);
CREATE TABLE assurance_validation_runs (
 tenant_id TEXT NOT NULL, validation_run_id TEXT NOT NULL, snapshot_id TEXT NOT NULL, rule_set_id TEXT NOT NULL,
 rule_set_revision INTEGER NOT NULL, idempotency_digest TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, validation_run_id), UNIQUE (tenant_id, idempotency_digest)
);
CREATE TABLE assurance_validation_results (
 tenant_id TEXT NOT NULL, validation_run_id TEXT NOT NULL, result_id TEXT NOT NULL, rule_id TEXT NOT NULL,
 status TEXT NOT NULL, result_json TEXT NOT NULL, PRIMARY KEY (tenant_id, result_id)
);
CREATE TABLE assurance_comparisons (
 tenant_id TEXT NOT NULL, comparison_id TEXT NOT NULL, current_snapshot_id TEXT NOT NULL, prior_snapshot_id TEXT NOT NULL,
 idempotency_digest TEXT NOT NULL, status TEXT NOT NULL, completeness TEXT NOT NULL, comparison_basis TEXT NOT NULL,
 policy_revision INTEGER NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, comparison_id),
 UNIQUE (tenant_id, idempotency_digest)
);
CREATE TABLE assurance_comparison_items (
 tenant_id TEXT NOT NULL, comparison_id TEXT NOT NULL, comparison_item_id TEXT NOT NULL, field_id TEXT NOT NULL,
 item_json TEXT NOT NULL, PRIMARY KEY (tenant_id, comparison_item_id)
);
`

const migrationV5 = `
CREATE TABLE assurance_transfers (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, source_json TEXT NOT NULL, target_json TEXT NOT NULL,
 before_value_json TEXT NOT NULL, intended_value_json TEXT NOT NULL, policy_hash TEXT NOT NULL, mapping_hash TEXT NOT NULL,
 confirmation_digest TEXT NOT NULL, confirmation_expires_at TEXT NOT NULL, confirmation_consumed_at TEXT NOT NULL DEFAULT '',
 idempotency_digest TEXT NOT NULL, request_id TEXT NOT NULL, correlation_id TEXT NOT NULL,
 provider_request_id TEXT NOT NULL DEFAULT '', operation_reference TEXT NOT NULL DEFAULT '', machine_outcome TEXT NOT NULL,
 execution_state TEXT NOT NULL, execution_lease_id TEXT NOT NULL DEFAULT '', lease_expires_at TEXT NOT NULL DEFAULT '',
 last_heartbeat_at TEXT NOT NULL DEFAULT '', submission_started_at TEXT NOT NULL DEFAULT '', terminal_at TEXT NOT NULL DEFAULT '',
 recovery_reason TEXT NOT NULL DEFAULT '', row_version INTEGER NOT NULL, claim_fence_digest TEXT NOT NULL DEFAULT '',
 terminal_fence_digest TEXT NOT NULL DEFAULT '', startup_quarantine TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, transfer_id)
);
CREATE INDEX idx_assurance_transfers_tenant_state ON assurance_transfers(tenant_id, execution_state, lease_expires_at);
CREATE TABLE assurance_transfer_confirmations (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, confirmation_id TEXT NOT NULL, confirmation_digest TEXT NOT NULL,
 actor_id TEXT NOT NULL, consumed_at TEXT NOT NULL, PRIMARY KEY (tenant_id, confirmation_id)
);
CREATE TABLE assurance_transfer_poll_events (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, event_id TEXT NOT NULL, event_json TEXT NOT NULL,
 observed_at TEXT NOT NULL, PRIMARY KEY (tenant_id, event_id)
);
CREATE TABLE assurance_transfer_readbacks (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, readback_id TEXT NOT NULL, typed_value_json TEXT NOT NULL,
 cache_bypassed INTEGER NOT NULL, observed_at TEXT NOT NULL, PRIMARY KEY (tenant_id, readback_id)
);
`

const migrationV6 = `
CREATE TABLE assurance_visual_acknowledgements (
 tenant_id TEXT NOT NULL, acknowledgement_id TEXT NOT NULL, transfer_id TEXT NOT NULL, confirm_actor_id TEXT NOT NULL,
 ack_actor_id TEXT NOT NULL, token_type TEXT NOT NULL, observation TEXT NOT NULL, ui_location TEXT NOT NULL,
 refreshed INTEGER NOT NULL, override_reason TEXT NOT NULL DEFAULT '', observed_value_json TEXT NOT NULL DEFAULT '',
 observed_digest TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', audit_id TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, acknowledgement_id)
);
CREATE TABLE assurance_reconciliations (
 tenant_id TEXT NOT NULL, reconciliation_id TEXT NOT NULL, transfer_id TEXT NOT NULL, reason TEXT NOT NULL,
 classification TEXT NOT NULL, frozen_intent_json TEXT NOT NULL, evidence_json TEXT NOT NULL,
 disposition TEXT NOT NULL, actor_id TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, reconciliation_id)
);
CREATE TABLE assurance_reconciliation_events (
 tenant_id TEXT NOT NULL, reconciliation_id TEXT NOT NULL, event_id TEXT NOT NULL, event_json TEXT NOT NULL,
 created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, event_id)
);
`

const migrationV7 = `
CREATE TABLE assurance_evidence_manifests (
 tenant_id TEXT NOT NULL, manifest_id TEXT NOT NULL, manifest_version INTEGER NOT NULL, subject_kind TEXT NOT NULL,
 subject_id TEXT NOT NULL, manifest_hash TEXT NOT NULL, manifest_json TEXT NOT NULL, audit_integrity TEXT NOT NULL,
 audit_completeness TEXT NOT NULL, expiry_at TEXT NOT NULL DEFAULT '', legal_hold INTEGER NOT NULL DEFAULT 0,
 idempotency_digest TEXT NOT NULL, PRIMARY KEY (tenant_id, manifest_id), UNIQUE (tenant_id, idempotency_digest)
);
CREATE TABLE assurance_evidence_artifacts (
 tenant_id TEXT NOT NULL, artifact_id TEXT NOT NULL, manifest_id TEXT NOT NULL, artifact_hash TEXT NOT NULL,
 size_bytes INTEGER NOT NULL, media_type TEXT NOT NULL, storage_reference TEXT NOT NULL,
 omissions_json TEXT NOT NULL DEFAULT '[]', redaction_json TEXT NOT NULL DEFAULT '{}', PRIMARY KEY (tenant_id, artifact_id)
);
CREATE TABLE assurance_evidence_subjects (
 tenant_id TEXT NOT NULL, manifest_id TEXT NOT NULL, subject_kind TEXT NOT NULL, subject_id TEXT NOT NULL,
 PRIMARY KEY (tenant_id, manifest_id, subject_kind, subject_id)
);
`

const migrationV8 = `
CREATE TABLE assurance_retention_policies (
 tenant_id TEXT NOT NULL, retention_class TEXT NOT NULL, duration_seconds INTEGER NOT NULL, policy_json TEXT NOT NULL,
 content_hash TEXT NOT NULL, PRIMARY KEY (tenant_id, retention_class)
);
CREATE TABLE assurance_idempotency_records (
 tenant_id TEXT NOT NULL, record_id TEXT NOT NULL, actor_id TEXT NOT NULL, tool TEXT NOT NULL, action TEXT NOT NULL,
 idempotency_digest TEXT NOT NULL, request_digest TEXT NOT NULL, response_schema_version INTEGER NOT NULL DEFAULT 1,
 response_envelope TEXT NOT NULL DEFAULT '', response_hash TEXT NOT NULL DEFAULT '', response_status TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL, owner_nonce TEXT NOT NULL, lease_expires_at TEXT NOT NULL, last_heartbeat_at TEXT NOT NULL,
 execution_started INTEGER NOT NULL DEFAULT 0, entity_reference TEXT NOT NULL DEFAULT '', correlation_id TEXT NOT NULL,
 audit_id TEXT NOT NULL DEFAULT '', provider_request_id TEXT NOT NULL DEFAULT '', operation_reference TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, terminal_at TEXT NOT NULL DEFAULT '', expires_at TEXT NOT NULL, retention_class TEXT NOT NULL,
 PRIMARY KEY (tenant_id, record_id), UNIQUE (tenant_id, actor_id, tool, action, idempotency_digest),
 CHECK(state IN ('reserved','sealed','failed','expired'))
);
CREATE INDEX idx_assurance_idempotency_tenant_state_lease ON assurance_idempotency_records(tenant_id, state, lease_expires_at);
CREATE INDEX idx_assurance_idempotency_tenant_expiry ON assurance_idempotency_records(tenant_id, expires_at);
CREATE TABLE assurance_audit_links (
 tenant_id TEXT NOT NULL, link_id TEXT NOT NULL, entity_kind TEXT NOT NULL, entity_id TEXT NOT NULL,
 audit_id TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '', correlation_id TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, link_id)
);
CREATE INDEX idx_assurance_audit_links_tenant_entity ON assurance_audit_links(tenant_id, entity_kind, entity_id);
CREATE TABLE assurance_checkpoints (
 tenant_id TEXT NOT NULL, checkpoint_id TEXT NOT NULL, checkpoint_kind TEXT NOT NULL, high_water_mark TEXT NOT NULL,
 checkpoint_hash TEXT NOT NULL, signature_hex TEXT NOT NULL DEFAULT '', metadata_json TEXT NOT NULL,
 created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, checkpoint_id)
);
`

// migrationV9 is additive Wave 2 storage. The v1-v8 migrations are already
// deployed and must remain byte-for-byte stable for upgrade safety.
const migrationV9 = `
CREATE TABLE assurance_materiality_policies (
 tenant_id TEXT NOT NULL, policy_id TEXT NOT NULL, revision INTEGER NOT NULL,
 status TEXT NOT NULL, policy_json TEXT NOT NULL, content_hash TEXT NOT NULL,
 PRIMARY KEY (tenant_id, policy_id, revision)
);
CREATE TABLE assurance_export_profiles (
 tenant_id TEXT NOT NULL, profile_id TEXT NOT NULL, revision INTEGER NOT NULL,
 status TEXT NOT NULL, profile_json TEXT NOT NULL, content_hash TEXT NOT NULL,
 PRIMARY KEY (tenant_id, profile_id, revision)
);
CREATE TABLE assurance_legal_holds (
 tenant_id TEXT NOT NULL, hold_id TEXT NOT NULL, subject_kind TEXT NOT NULL,
 subject_id TEXT NOT NULL, reason TEXT NOT NULL, active INTEGER NOT NULL,
 actor_id TEXT NOT NULL, created_at TEXT NOT NULL, released_at TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (tenant_id, hold_id)
);
CREATE INDEX idx_assurance_legal_holds_subject ON assurance_legal_holds(tenant_id, subject_kind, subject_id, active);
CREATE TABLE assurance_evidence_tombstones (
 tenant_id TEXT NOT NULL, tombstone_id TEXT NOT NULL, subject_kind TEXT NOT NULL,
 subject_id TEXT NOT NULL, manifest_id TEXT NOT NULL, reason TEXT NOT NULL,
 purged_at TEXT NOT NULL, PRIMARY KEY (tenant_id, tombstone_id)
);
CREATE INDEX idx_assurance_evidence_tombstones_subject ON assurance_evidence_tombstones(tenant_id, subject_kind, subject_id);
ALTER TABLE assurance_comparisons ADD COLUMN materiality_policy_id TEXT NOT NULL DEFAULT '';
ALTER TABLE assurance_comparisons ADD COLUMN current_report_id TEXT NOT NULL DEFAULT '';
ALTER TABLE assurance_comparisons ADD COLUMN prior_report_id TEXT NOT NULL DEFAULT '';
ALTER TABLE assurance_comparisons ADD COLUMN current_definition_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assurance_comparisons ADD COLUMN prior_definition_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assurance_comparisons ADD COLUMN partial_policy TEXT NOT NULL DEFAULT 'reject';
ALTER TABLE assurance_comparisons ADD COLUMN material_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assurance_validation_runs ADD COLUMN fail_on_warning INTEGER NOT NULL DEFAULT 0;
`

// migrationV10 is additive and records the exact object revisions selected by
// the active signed bundle. Historical object rows remain untouched.
const migrationV10 = `
CREATE TABLE assurance_active_bundle_objects (
 tenant_id TEXT NOT NULL, object_kind TEXT NOT NULL, object_id TEXT NOT NULL,
 object_revision INTEGER NOT NULL, bundle_version INTEGER NOT NULL,
 PRIMARY KEY (tenant_id, object_kind, object_id, object_revision)
);
CREATE INDEX idx_assurance_active_bundle_objects_tenant_kind ON assurance_active_bundle_objects(tenant_id, object_kind, object_id, object_revision);
CREATE TABLE assurance_retention_policy_revisions (
 tenant_id TEXT NOT NULL, retention_class TEXT NOT NULL, revision INTEGER NOT NULL,
 duration_seconds INTEGER NOT NULL, policy_json TEXT NOT NULL, content_hash TEXT NOT NULL,
 PRIMARY KEY (tenant_id, retention_class, revision)
);
`

// migrationV11 makes evidence retention retries and legal holds exact and
// durable. The v8 singleton retention table remains compatibility history;
// revisioned rows are authoritative.
const migrationV11 = `
ALTER TABLE assurance_legal_holds ADD COLUMN manifest_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_assurance_legal_holds_tenant_manifest ON assurance_legal_holds(tenant_id, manifest_id, active);
ALTER TABLE assurance_evidence_tombstones ADD COLUMN state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE assurance_evidence_tombstones ADD COLUMN artifact_disposition_json TEXT NOT NULL DEFAULT '{}';
CREATE UNIQUE INDEX idx_assurance_evidence_tombstones_manifest ON assurance_evidence_tombstones(tenant_id, manifest_id);
CREATE TABLE assurance_evidence_cleanup (
 tenant_id TEXT NOT NULL, cleanup_id TEXT NOT NULL, record_id TEXT NOT NULL, storage_reference TEXT NOT NULL,
 state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 PRIMARY KEY (tenant_id, cleanup_id)
);
CREATE INDEX idx_assurance_evidence_cleanup_record ON assurance_evidence_cleanup(tenant_id, record_id, state);
`

const migrationV12 = `
DROP TRIGGER IF EXISTS assurance_snapshot_terminal_immutable;
DROP TRIGGER IF EXISTS assurance_snapshot_no_delete;
DROP TRIGGER IF EXISTS assurance_observation_no_update;
DROP TRIGGER IF EXISTS assurance_observation_no_delete;
DROP TRIGGER IF EXISTS assurance_failure_no_update;
DROP TRIGGER IF EXISTS assurance_failure_no_delete;
DROP INDEX IF EXISTS idx_assurance_snapshots_tenant_report_period;
ALTER TABLE assurance_snapshots RENAME TO assurance_snapshots_v11;
CREATE TABLE assurance_snapshots (
 tenant_id TEXT NOT NULL, snapshot_id TEXT NOT NULL, idempotency_digest TEXT NOT NULL, report_id TEXT NOT NULL,
 definition_revision INTEGER NOT NULL, period_json TEXT NOT NULL, status TEXT NOT NULL,
 completeness TEXT NOT NULL, mapping_set_hash TEXT NOT NULL, provider_route TEXT NOT NULL,
 content_hash TEXT NOT NULL DEFAULT '', retention_class TEXT NOT NULL, captured_at TEXT NOT NULL DEFAULT '',
 expires_at TEXT NOT NULL DEFAULT '', audit_id TEXT NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, snapshot_id),
 CHECK(status IN ('running','completed','partial','failed','expired'))
);
INSERT INTO assurance_snapshots (tenant_id, snapshot_id, idempotency_digest, report_id, definition_revision, period_json, status, completeness, mapping_set_hash, provider_route, content_hash, retention_class, captured_at, expires_at, audit_id)
 SELECT tenant_id, snapshot_id, idempotency_digest, report_id, definition_revision, period_json, status, completeness, mapping_set_hash, provider_route, content_hash, retention_class, captured_at, expires_at, audit_id
 FROM assurance_snapshots_v11;
DROP TABLE assurance_snapshots_v11;
CREATE INDEX idx_assurance_snapshots_tenant_report_period ON assurance_snapshots(tenant_id, report_id, definition_revision);
CREATE TRIGGER assurance_snapshot_terminal_immutable BEFORE UPDATE ON assurance_snapshots
 WHEN OLD.status IN ('completed','partial','failed','expired') BEGIN SELECT RAISE(ABORT, 'immutable assurance snapshot'); END;
CREATE TRIGGER assurance_snapshot_no_delete BEFORE DELETE ON assurance_snapshots BEGIN SELECT RAISE(ABORT, 'immutable assurance snapshot'); END;
CREATE TRIGGER assurance_observation_no_update BEFORE UPDATE ON assurance_snapshot_observations BEGIN SELECT RAISE(ABORT, 'immutable assurance observation'); END;
CREATE TRIGGER assurance_observation_no_delete BEFORE DELETE ON assurance_snapshot_observations BEGIN SELECT RAISE(ABORT, 'immutable assurance observation'); END;
CREATE TRIGGER assurance_failure_no_update BEFORE UPDATE ON assurance_snapshot_failures BEGIN SELECT RAISE(ABORT, 'immutable assurance failure'); END;
CREATE TRIGGER assurance_failure_no_delete BEFORE DELETE ON assurance_snapshot_failures BEGIN SELECT RAISE(ABORT, 'immutable assurance failure'); END;
`

const migrationV13 = `
ALTER TABLE assurance_validation_runs RENAME TO assurance_validation_runs_v13;
CREATE TABLE assurance_validation_runs (
 tenant_id TEXT NOT NULL, validation_run_id TEXT NOT NULL, snapshot_id TEXT NOT NULL, rule_set_id TEXT NOT NULL,
 rule_set_revision INTEGER NOT NULL, idempotency_digest TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL,
 fail_on_warning INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (tenant_id, validation_run_id)
);
INSERT INTO assurance_validation_runs (tenant_id, validation_run_id, snapshot_id, rule_set_id, rule_set_revision, idempotency_digest, status, created_at, fail_on_warning)
SELECT tenant_id, validation_run_id, snapshot_id, rule_set_id, rule_set_revision, idempotency_digest, status, created_at, fail_on_warning
FROM assurance_validation_runs_v13;
DROP TABLE assurance_validation_runs_v13;

ALTER TABLE assurance_comparisons RENAME TO assurance_comparisons_v13;
CREATE TABLE assurance_comparisons (
 tenant_id TEXT NOT NULL, comparison_id TEXT NOT NULL, current_snapshot_id TEXT NOT NULL, prior_snapshot_id TEXT NOT NULL,
 idempotency_digest TEXT NOT NULL, status TEXT NOT NULL, completeness TEXT NOT NULL, comparison_basis TEXT NOT NULL,
 policy_revision INTEGER NOT NULL, created_at TEXT NOT NULL, materiality_policy_id TEXT NOT NULL DEFAULT '',
 current_report_id TEXT NOT NULL DEFAULT '', prior_report_id TEXT NOT NULL DEFAULT '',
 current_definition_revision INTEGER NOT NULL DEFAULT 0, prior_definition_revision INTEGER NOT NULL DEFAULT 0,
 partial_policy TEXT NOT NULL DEFAULT 'reject', material_count INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (tenant_id, comparison_id)
);
INSERT INTO assurance_comparisons (tenant_id, comparison_id, current_snapshot_id, prior_snapshot_id, idempotency_digest, status, completeness, comparison_basis, policy_revision, created_at, materiality_policy_id, current_report_id, prior_report_id, current_definition_revision, prior_definition_revision, partial_policy, material_count)
SELECT tenant_id, comparison_id, current_snapshot_id, prior_snapshot_id, idempotency_digest, status, completeness, comparison_basis, policy_revision, created_at, materiality_policy_id, current_report_id, prior_report_id, current_definition_revision, prior_definition_revision, partial_policy, material_count
FROM assurance_comparisons_v13;
DROP TABLE assurance_comparisons_v13;

ALTER TABLE assurance_evidence_manifests RENAME TO assurance_evidence_manifests_v13;
CREATE TABLE assurance_evidence_manifests (
 tenant_id TEXT NOT NULL, manifest_id TEXT NOT NULL, manifest_version INTEGER NOT NULL, subject_kind TEXT NOT NULL,
 subject_id TEXT NOT NULL, manifest_hash TEXT NOT NULL, manifest_json TEXT NOT NULL, audit_integrity TEXT NOT NULL,
 audit_completeness TEXT NOT NULL, expiry_at TEXT NOT NULL DEFAULT '', legal_hold INTEGER NOT NULL DEFAULT 0,
 idempotency_digest TEXT NOT NULL, PRIMARY KEY (tenant_id, manifest_id)
);
INSERT INTO assurance_evidence_manifests (tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest)
SELECT tenant_id, manifest_id, manifest_version, subject_kind, subject_id, manifest_hash, manifest_json, audit_integrity, audit_completeness, expiry_at, legal_hold, idempotency_digest
FROM assurance_evidence_manifests_v13;
DROP TABLE assurance_evidence_manifests_v13;
`

const migrationV14 = `
CREATE TABLE transfer_intents (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, actor_id TEXT NOT NULL, permission TEXT NOT NULL,
 intent_json TEXT NOT NULL, state TEXT NOT NULL, token_digest TEXT NOT NULL, idempotency_digest TEXT NOT NULL,
 request_digest TEXT NOT NULL, expires_at TEXT NOT NULL, lease_id TEXT NOT NULL DEFAULT '', lease_expires_at TEXT NOT NULL DEFAULT '',
 claim_fence_digest TEXT NOT NULL DEFAULT '', terminal_fence_digest TEXT NOT NULL DEFAULT '', recovery_reason TEXT NOT NULL DEFAULT '',
 operation_reference TEXT NOT NULL DEFAULT '', operation_completed INTEGER NOT NULL DEFAULT 0, submission_started_at TEXT NOT NULL DEFAULT '',
 submissions INTEGER NOT NULL DEFAULT 0, row_version INTEGER NOT NULL DEFAULT 1,
 machine_outcome TEXT NOT NULL DEFAULT 'not_applicable', machine_outcome_provenance TEXT NOT NULL DEFAULT 'not_applicable',
 visual_state TEXT NOT NULL DEFAULT 'pending',
 PRIMARY KEY(tenant_id,transfer_id), UNIQUE(tenant_id,actor_id,idempotency_digest)
);
CREATE INDEX idx_transfer_intents_tenant_state ON transfer_intents(tenant_id,state,lease_expires_at);
CREATE TABLE transfer_audit_events (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, event_id TEXT NOT NULL, disposition TEXT NOT NULL,
 details TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(tenant_id,event_id)
);
CREATE TABLE transfer_visual_acknowledgements (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, ack_actor_id TEXT NOT NULL, observation TEXT NOT NULL,
 ui_location TEXT NOT NULL, refreshed INTEGER NOT NULL, observed_value TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
 PRIMARY KEY(tenant_id,transfer_id,ack_actor_id,created_at)
);
CREATE TABLE transfer_reconciliations (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, reason TEXT NOT NULL, classification TEXT NOT NULL,
 evidence_ref TEXT NOT NULL, disposition TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(tenant_id,transfer_id,created_at)
);
CREATE TABLE assurance_transfer_fences (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, fence_kind TEXT NOT NULL CHECK(fence_kind IN ('claim','terminal')),
 fence_digest TEXT NOT NULL, binding_json TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(tenant_id,transfer_id,fence_kind)
);
CREATE TABLE assurance_transfer_visual_evidence (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, acknowledgement_id TEXT NOT NULL, confirmer_actor_id TEXT NOT NULL,
 ack_actor_id TEXT NOT NULL, observation TEXT NOT NULL, ui_location TEXT NOT NULL, refreshed INTEGER NOT NULL,
 observed_value_json TEXT NOT NULL DEFAULT '', observed_digest TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
 PRIMARY KEY(tenant_id,acknowledgement_id)
);
CREATE TABLE assurance_transfer_reconciliation_evidence (
 tenant_id TEXT NOT NULL, transfer_id TEXT NOT NULL, reconciliation_id TEXT NOT NULL, reason TEXT NOT NULL,
 classification TEXT NOT NULL, evidence_ref TEXT NOT NULL DEFAULT '', disposition TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(tenant_id,reconciliation_id)
);
CREATE INDEX idx_assurance_transfer_recon_tenant_state ON assurance_transfer_reconciliation_evidence(tenant_id,transfer_id,disposition);
`

const migrationV15 = `
CREATE TABLE assurance_transfer_route_revisions (
 tenant_id TEXT NOT NULL, route_id TEXT NOT NULL, revision INTEGER NOT NULL CHECK(revision > 0),
 content_hash TEXT NOT NULL, route_json TEXT NOT NULL,
 PRIMARY KEY(tenant_id, route_id, revision)
);
CREATE INDEX idx_transfer_routes_tenant_id ON assurance_transfer_route_revisions(tenant_id, route_id, revision);
`

const migrationV16 = `
CREATE TABLE assurance_conversion_policy_revisions (
 tenant_id TEXT NOT NULL, policy_id TEXT NOT NULL, revision INTEGER NOT NULL CHECK(revision > 0),
 content_hash TEXT NOT NULL, policy_json TEXT NOT NULL,
 PRIMARY KEY(tenant_id,policy_id,revision)
);
CREATE INDEX idx_conversion_policies_tenant_id ON assurance_conversion_policy_revisions(tenant_id,policy_id,revision);
`

var migrations = []string{migrationV1, migrationV2, migrationV3, migrationV4, migrationV5, migrationV6, migrationV7, migrationV8, migrationV9, migrationV10, migrationV11, migrationV12, migrationV13, migrationV14, migrationV15, migrationV16}

// Migrate installs the complete contiguous assurance schema family.
func Migrate(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("assurance: database is required")
	}
	if err := sqlitedb.Migrate(ctx, db, "assurance", migrations); err != nil {
		return fmt.Errorf("assurance: migrate: %w", err)
	}
	return nil
}

func assuranceTables() []string {
	return []string{
		"assurance_resources", "assurance_mappings", "assurance_mapping_revisions", "assurance_relationship_edges", "assurance_relationship_discovery_runs",
		"assurance_bundle_candidates", "assurance_active_bundles", "assurance_report_definitions", "assurance_report_revisions", "assurance_report_fields",
		"assurance_snapshots", "assurance_snapshot_observations", "assurance_snapshot_failures",
		"assurance_rule_sets", "assurance_rule_set_rules", "assurance_validation_runs", "assurance_validation_results", "assurance_comparisons", "assurance_comparison_items",
		"assurance_transfers", "assurance_transfer_confirmations", "assurance_transfer_poll_events", "assurance_transfer_readbacks",
		"assurance_visual_acknowledgements", "assurance_reconciliations", "assurance_reconciliation_events",
		"assurance_evidence_manifests", "assurance_evidence_artifacts", "assurance_evidence_subjects",
		"assurance_retention_policies", "assurance_idempotency_records", "assurance_audit_links", "assurance_checkpoints",
		"assurance_materiality_policies", "assurance_export_profiles", "assurance_legal_holds", "assurance_evidence_tombstones",
		"assurance_active_bundle_objects", "assurance_retention_policy_revisions",
		"assurance_evidence_cleanup", "assurance_transfer_fences", "assurance_transfer_visual_evidence", "assurance_transfer_reconciliation_evidence",
		"transfer_intents", "transfer_audit_events", "transfer_visual_acknowledgements", "transfer_reconciliations",
		"assurance_transfer_route_revisions", "assurance_conversion_policy_revisions",
	}
}
