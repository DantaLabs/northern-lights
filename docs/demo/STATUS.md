# Demo status

Each agent edits only its own lane row, at the end of every work session. Tom reads this file each morning.

Plan day: 1 of 11 (Monday 19 October 2026).

| Lane | Status | Branch | Last commit | Blocked on |
|---|---|---|---|---|
| L0 Scaffold | follow-up done; internal race suite, vet, gofmt and lint pass; R10 deferred | `codex/demo-l0-followup` | 1a45416 | Full suite: cmd/workiva-mcp/main_test.go:213 expects 2 spreadsheets, now 4; test edit outside authorized scope. R10 waits for L1-L4 merges |
| L1 Change set | draft PR [#19](https://github.com/DantaLabs/northern-lights/pull/19); R1–R13 done; S1–S10 pass (live restored); local race pass with 30m harness | `codex/demo-l1-change-set` | 652519c | L0 demo-tool Entra permission map (existing request); CI pending |
| L2 Section recipe | ready for review; merged with L1+L3 from `claude/demo-integration`; integration test runs Prepare on the real L1 change set and feeds L3's number guard through the FactStore from `recipe.New`; summary now L1's `Preview.Summary`; `NL_DEMO_DOCUMENT_ID` overrides the recipe document ID in sandbox mode only | `claude/demo-l2-section-recipe` | see PR #20 | nothing; deleted the unused `notImplementedYet` in L0's demo.go (no L0 session active, owner approved) |
| L3 Section text | [draft PR #18](https://github.com/DantaLabs/northern-lights/pull/18): R2-R7/R9-R12 implemented; R8 skipped (document); live scenario 10 PASS 2026-10-20: exact read-back, existing-content deleteText + insertText in one batch, nesting under "2. Environmental information", NL test sections deleted; race suite PASS (-timeout=30m; default 10m timed out in frozen assurance/transfer), vet/gofmt/lint PASS; Sonnet review ready for L3 scope | `codex/demo-l3-section-text` | 70ba164 | L2 integration uses fake recipe.Service/FactStore in tests; Entra demo-tool permission mapping remains with L0 |
| L4 Data and agent | ready for review (draft PR); live seed done 2026-10-19 (42 fields, 126 cells read back, 0 mismatch, 99.7 s vs 1-minute target) | `claude/demo-l4-data-and-agent` | 7002483 | L2 `workiva_prepare_section` for scenario 5; Tom's RB-03 for scenario 6 |
| L5 Manual tests | RB-01 today | `claude/demo-integration` | | |

## Runbook results

| Runbook | Date | Result | Evidence |
|---|---|---|---|
| RB-01 Baseline smoke | 2026-10-19 | pass (13 tools, 401 on bad key, reads OK; report document ID PENDIENTE for RB-02) | `docs/demo/evidence/2026-10-19-RB-01/` |
| RB-02 Documents API spike | 2026-10-19 | pass: document (create, rich-text edit, exact read-back, delete); L3 live 2026-10-20 also verified existing-content replacement in one deleteText/insertText batch and nesting under "2. Environmental information" | `docs/demo/evidence/2026-10-19-RB-02/` |
| RB-03 Demo acts in Copilot | | | |

## Requests to other lanes

Write one line: requesting lane, owning lane, file, what you need and why.
