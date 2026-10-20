# Demo status

Each agent edits only its own lane row, at the end of every work session. Tom reads this file each morning.

Plan day: 1 of 11 (Monday 19 October 2026).

| Lane | Status | Branch | Last commit | Blocked on |
|---|---|---|---|---|
| L0 Scaffold | ready for review (R10 end-to-end test waits for L1-L4) | `claude/demo-l0-scaffold` | 19b03ea | Entra permission mapping for demo tools |
| L1 Change set | waiting for L0 | `codex/demo-l1-change-set` | | L0 merge |
| L2 Section recipe | ready for review (draft PR); scenario 9 proven on L4 demo data; `NL_DEMO_DOCUMENT_ID` overrides the recipe document ID in sandbox mode only (ignored in demo mode) | `claude/demo-l2-section-recipe` | see PR | L1 merge: switch summary to Preview.Summary |
| L3 Section text | RB-02 spike can start now | `codex/demo-l3-section-text` | | RB-02 result for the document writer |
| L4 Data and agent | ready for review (draft PR); live seed done 2026-10-19 (42 fields, 126 cells read back, 0 mismatch, 99.7 s vs 1-minute target) | `claude/demo-l4-data-and-agent` | 7002483 | L2 `workiva_prepare_section` for scenario 5; Tom's RB-03 for scenario 6 |
| L5 Manual tests | RB-01 today | `claude/demo-integration` | | |

## Runbook results

| Runbook | Date | Result | Evidence |
|---|---|---|---|
| RB-01 Baseline smoke | 2026-10-19 | pass (13 tools, 401 on bad key, reads OK; report document ID PENDIENTE for RB-02) | `docs/demo/evidence/2026-10-19-RB-01/` |
| RB-02 Documents API spike | | | |
| RB-03 Demo acts in Copilot | | | |

## Requests to other lanes

Write one line: requesting lane, owning lane, file, what you need and why.

L0 -> owner (no lane owns it), internal/mcpserver/server.go: add requiredPermission entries for the four demo tools (prepare_section and stage_section_text -> workiva.write.preview; confirm_change_set and confirm_section_text -> workiva.write.confirm) and list them in writeTools, because under NL_AUTH_MODE=entra every demo tool call is denied today.

L4 -> L0, internal/workiva/demo_client.go: `demoSpreadsheetsResponse` should list `demo-esg-source` and `demo-esg-report` so act 1 works offline.
L4 -> L0, internal/workiva/demo_esg.go and DemoTransport: add an ESG overlay field (or WrittenCells entries) on DemoTransport so demo_esg.go can drop its package-level registry.
