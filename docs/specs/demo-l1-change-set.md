# Demo L1: change set

## Goal

Let an analyst approve many field writes with one confirmation. A change set is staged from a list of mapped fields, shown as one preview, confirmed once, written with one Workiva update per sheet, read back and audited per field.

## Non-goals

- Deciding which fields to write. The section recipe (L2) builds the item list.
- Formulas, formats, row insertion or multi-cell ranges beyond what the field mapping already defines.
- Automatic retry after an uncertain outcome.

## Users and scenarios

- Copilot calls `workiva_prepare_section` (L2), which calls `changeset.Service.Stage` and shows the analyst a preview of about 30 changes. The analyst says "approve". Copilot calls `workiva_confirm_change_set` with the change set ID and token. The analyst watches the cells change in Workiva.
- A second analyst tries to confirm the first analyst's change set and is refused.

## Requirements

R1: `Stage` resolves every item's field name through the mapping store and fails with the list of unknown names if any field is unknown; nothing is persisted on failure.
R2: `Stage` rejects an empty list, more than 500 items, or the same field twice.
R3: `Stage` checks every target against the resource allowlist (the same check `workiva_update_field` uses) before reading anything.
R4: `Stage` reads each field's current value without the cache and returns it as `Before` in the preview.
R5: `Stage` persists the change set in the existing SQLite database in tables prefixed `change_set_`, bound to the actor, the `workiva.write.confirm` permission, every item's field ID and coordinates, and an expiry of 10 minutes. Only a SHA-256 digest of the token is stored.
R6: `Confirm` consumes the token once. Unknown, reused, expired, foreign-actor and missing-permission cases each return a distinct error and write nothing.
R7: `Confirm` refuses if any field's mapping changed since staging.
R8: `Confirm` groups items by spreadsheet and sheet and sends one update per sheet with `workiva.NewLiteralEditCellsUpdate`, then waits for each operation through the existing operation polling.
R9: After each operation completes, `Confirm` reads every written field without the cache and sets the item status to `completed` when the read-back equals the staged value and `mismatch` otherwise.
R10: `Confirm` writes one audit row per field with actor, change set ID, field, before, after and operation URL, through the existing hash-chained audit log, and refreshes the read cache for written cells.
R11: If an operation outcome is unknown after submission (transport error, timeout or non-terminal status), the result status is `reconciliation_required`, the affected items are listed, and nothing is retried.
R12: `workiva_confirm_change_set` takes `change_set_id` and `confirm_token`, returns the `Result`, and includes a `summary` string with one line per field in the form `field: before -> after (status)`.
R13: The preview returned by `Stage` also includes that `summary` string so Copilot can show it before approval.

## Interfaces and data

Implements `changeset.Service` from `internal/changeset/changeset.go` (created by L0). Tool input: `{ "change_set_id": string, "confirm_token": string }`. Tool output: `Result` plus `summary` and `nl_audit_id`.

Code to reuse: `stageWrite`, `executeConfirmedWrite`, `executeWrite`, `auditWrite` and `refreshCacheAfterWrite` in `internal/mcpserver/tools/update_field.go`; `UpdateSheetWithRetryAfter` in `internal/workiva/update.go`; `tokenDigest` and `ConsumePendingWriteFor` in `internal/mapping/store.go` for the binding pattern.

## Constraints

- Do not change `workiva_update_field` behavior. Extract shared helpers into `internal/changeset` only by copying, not by editing `update_field.go` (that file is outside this lane).
- Pacing is the rate limiter's job. Do not add sleeps or caps beyond R2.
- Live sandbox tests write only to `Testing MCP` / `Sheet1` and restore the original values.

## Acceptance scenarios

1. (R1, R2) Staging with an unknown field, an empty list, 501 items, or a duplicate field fails and leaves no rows.
2. (R3) Staging a field on a disallowed sheet fails before any Workiva request.
3. (R4, R5, R13) Staging 30 fields across two sheets returns 30 `Before` values, a token, a summary with 30 lines, and stores only the token digest.
4. (R6) Confirming with a reused token, an expired token, another actor, or without `workiva.write.confirm` fails with four distinct errors and sends no update.
5. (R7) Changing one field's range after staging makes confirm fail with no update sent.
6. (R8, R9, R10) Confirming the 30-field change set sends exactly two update requests, reads back 30 values, returns 30 `completed` items and writes 30 audit rows whose chain verifies.
7. (R9) A read-back that differs from the staged value marks that item `mismatch` and the others `completed`.
8. (R11) A transport error after the update is accepted returns `reconciliation_required` and sends no second update.
9. (R12) The MCP tool returns the result and a summary line per field.
10. (R8, R9) Live, opt-in test (`NL_LIVE_SANDBOX=1`): stage and confirm three scratch cells in `Testing MCP` / `Sheet1`, verify read-back, then restore the original values with a second change set.

## Open questions

None

## Approval

Approved: yes
