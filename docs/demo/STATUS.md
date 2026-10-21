# Demo status

Each agent edits only its own lane row, at the end of every work session. Tom reads this file each morning.

Plan day: 1 of 11 (Monday 19 October 2026).

| Lane | Status | Branch | Last commit | Blocked on |
|---|---|---|---|---|
| L0 Scaffold | R10 complete: environmental MCP flow, 25 field writes/read-backs/audit rows, exact section text and both refusals; full race suite (30m timeout), vet, gofmt and lint pass; lane review recommends merge | `claude/demo-l0-e2e` | 10f204e | none; PR CI pending |
| L0 Dry run (Codex) | social offline MCP test/race pass; RB-03a API rehearsal: 41 values + 2 sections and audits match, both guards pass; 268s acts, seed 113.8s; full race (25 packages), vet/build/gofmt/lint pass; lighter code/evidence review complete | `codex/demo-dryrun` ([PR #23](https://github.com/DantaLabs/northern-lights/pull/23)) | 57ab656 | Act 1 snapshot comparison and Act 4 assurance evidence subject; browser rehearsal remains with Tom |
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
| RB-03a HTTP sandbox dry run | 2026-10-21 | partial: 41 writes, 2 sections and audits match; both refusals pass; comparison/export blocked; seed 113.8s | `docs/demo/evidence/2026-10-21-RB-03a/` |

## Requests to other lanes

Write one line: requesting lane, owning lane, file, what you need and why.

- L0 Dry run → L0/L4, `internal/mcpserver/tools/compare_periods.go` and `docs/demo/agent/DEMO_PROMPTS.md`: Act 1 asks for a mapped-field period comparison, but the tool requires immutable snapshot IDs plus a materiality policy; sandbox assurance is disabled and reads return no such subjects. Supply the approved assurance setup/subjects or align the demo call path. Recorded in RB-03a step-5; no out-of-lane implementation change made.
- L0 Dry run → L0/L4, `internal/mcpserver/tools/export_evidence.go` and `docs/demo/agent/DEMO_PROMPTS.md`: Act 4 has only demo change-set/text results, while evidence export accepts snapshot/validation/comparison/transfer subjects and requires provisioned assurance plus trusted Entra identity. Define an authorized export path for the demo without weakening assurance checks. Recorded in RB-03a step-27; no subject ID invented or out-of-lane implementation change made.
