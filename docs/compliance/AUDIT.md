# Audit Log

## Design

The audit log is an application-level append-only SQLite table with one
SHA-256 hash chain per tenant. Tenant chains may be interleaved by global
sequence number; each row links to the previous row for its own tenant.
Modifying any row, or deleting a row that has a same-tenant successor, breaks
that tenant chain at the affected sequence number. Deleting only the final row
of a tenant chain is not detectable by the chain verifier.

```
tenant row N: hash = sha256(previous_tenant_hash || tenant_id || ts || actor || tool || action || target || before || after || workiva_op_url || audit_id)
tenant row 0: hash = sha256("GENESIS" || tenant_id || ts || actor || tool || action || target || before || after || workiva_op_url || audit_id)
```

The hash uses the stored datetime format (`2006-01-02 15:04:05` UTC),
not the RFC 3339 representation, so the chain is stable across reads.
Rows migrated from the pre-tenant schema retain and verify their original v1
hashes under `legacy-api-key`; new rows use the tenant-bound v2 hash.

Known limitation: the v1 hash predates tenant binding, so `tenant_id` is not
hash-protected on any migrated v1 row. An attacker can relabel an entire
all-v1 chain wholesale to another tenant without detection: its internal
`prev_hash` links still agree and none of its hashes cover the tenant. A
partial relabel of an all-v1 chain splits those links across tenant chains and
fails verification. Every v2 row binds its tenant into the hash; relabeling a
v2 row fails hash verification, and leaving a v2 successor behind while
relabeling its v1 prefix breaks that tenant chain's `prev_hash` link.

## What is logged

| Field | Description |
|---|---|
| seq | Auto-incrementing sequence number |
| ts | UTC timestamp |
| tenant_id | Verified Entra `tid`, or `legacy-api-key` in API-key mode |
| hash_version | `1` for migrated pre-tenant rows; `2` for tenant-bound rows |
| actor | Verified Entra audit actor, or the sanitized API-key actor header |
| tool | The MCP tool name (e.g. `workiva_update_field`) |
| action | What happened (`call`, `read`, `write`, `sync`, `init`) |
| target | Identifier (e.g. `spreadsheetId/sheetId/B3:D10` or field name) |
| before_json | Cell value or state before the mutation (writes only) |
| after_json | Cell value or state after the mutation (writes only) |
| workiva_op_url | The Workiva operationLocation URL for cross-referencing with Workiva's own file history |

## Verifying the chain

```bash
./workiva-mcp audit verify -db /path/to/northern-lights.db
```

Verify walks the entire `audit_log` table in sequence order and recomputes
every row of every tenant chain in a single pass; it is not limited to one
tenant. Exit code 0 means every tenant chain is intact. Non-zero exit with
the first broken sequence number means tampering was detected.

## Exporting for auditors

```bash
./workiva-mcp audit export -db /path/to/northern-lights.db -format jsonl
```

Export likewise spans all tenant chains: every row of the table is emitted
in sequence order, one JSON object per line, suitable for ingestion into
SIEM systems, compliance dashboards, or archival.

## Retention guidance

EU financial reporting norms (ESRS, CSRD) require document retention of
at least 10 years. The audit log does not auto-expire. Operators should
include the database file in their backup and retention policies.

## Integration with Workiva's own history

Every Workiva field update through `workiva_update_field` stores the Workiva
`operationLocation` URL in the `workiva_op_url` column. An auditor can follow
this URL (authenticated) to see the exact Workiva file revision that resulted
from the mutation.

The update tool's complete outcome and reconciliation contract is:

| Status | Certainty and returned reconciliation metadata | Retry or restaging guidance |
|---|---|---|
| `awaiting_confirmation` | No mutation was submitted. The response contains the target, current `before` value, intended `after_preview`, and single-use `confirm_token`. | It is safe to abandon the preview. Confirm it after approval or restage for a new preview. |
| `written` | Workiva reported completion. The response contains the target, `before`, `after`, and `workiva_op_url`. | Do not retry. Restage only for a new intentional change. |
| `written_audit_failed` | Workiva reported completion but the rich local audit append failed. The response contains the target, `before`, `after`, and `workiva_op_url`. | Do not retry or restage the same mutation. Use Workiva history and preserve separate reconciliation evidence. |
| `write_rejected` | The HTTP response establishes non-acceptance. The response contains the target, `before`, intended `after_preview`, and an operation URL if available. | Correct the cause and restage before retrying. |
| `write_outcome_unknown` | Submission or operation polling did not establish a final result; the mutation may have taken effect. The response contains the target, `before`, intended `after_preview`, and `workiva_op_url` when known. | Do not retry or restage until Workiva history, the target, and any operation URL have been reconciled. |
| `write_failed` | Workiva accepted the request and reported a terminal failed operation. The response contains the target, `before`, intended `after_preview`, and `workiva_op_url`. | Inspect and reconcile the terminal failure, correct its cause, then restage. |

`write_failed` is a terminal result reported by the operation. It is distinct
from `write_outcome_unknown`, where either submission or polling failed to
prove whether the write took effect. HTTP 5xx responses are treated as
unknown because they do not establish non-acceptance.

Confirmation consumption happens before the final Workiva write: presenting a
valid `confirm_token` spends it, and only then does the tool re-read the live
value and submit the mutation. If the write fails after consumption but
before the mutation is submitted (for example the live before-value read
fails), the tool returns an error that explicitly states the confirmation
token was consumed and that the write must be restaged. The token cannot be
reused; re-presenting it is rejected as unknown. Restage with a fresh
preview call to obtain a new token.

If a Workiva update completes but its rich audit append fails, the tool returns
`written_audit_failed` with the exact target, before/after values, and
`workiva_op_url`. Do not retry the write. Reconcile it against Workiva file
history, then preserve a separate reconciliation record with the response and source
evidence. Restore audit availability before further mutations; do not edit
existing chain rows or claim that chain verification proves completeness.

If local mapping upserts complete but their rich audit append fails,
`workiva_sync_mapping` returns `synced_audit_failed` with spreadsheet ID, sheet
ID, field count, and field names. Do not retry blindly. Compare those mappings
with the source sheet and add the missing audit evidence after restoring the
audit database. Preserve the recovery response as evidence; audit verification
checks the surviving chain, not whether a failed append ever occurred.
