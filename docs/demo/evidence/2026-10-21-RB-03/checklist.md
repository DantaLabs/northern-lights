# RB-03 — 2026-10-21

Result: **FAIL**. Base `a060abb`, branch `codex/demo-rb03`. Tom explicitly authorised browser and Copilot Studio testing for this run. Testing stopped at Act 1 prompt 1.1 after three fix rounds, as requested. Acts 2–5 were not attempted.

## Checklist

- **FAIL — Act 1:** final energy answer gives 1,978 and 2,030, correct periods, direction and named spreadsheet/sheet/cell citations, but omits MWh. Prompts 1.2–1.4 and the independent Workiva cell comparison were not shown. Evidence: `act-1-1.png`, `act-1-1.json`, `prompt-1.1.txt`, `step-2.json`.
- **NOT SHOWN — Act 2:** stopped before section preparation; no findings or change-set preview. Evidence: `step-2.json`, `requests.txt`.
- **NOT SHOWN — Act 3 writes and document:** no approvals or confirmations were run. Evidence: `step-2.json` (zero confirmation write audit rows), `requests.txt`.
- **NOT SHOWN — Act 3 narrative style rules 1–4:** no narrative was drafted. Evidence: `step-2.json`.
- **NOT SHOWN — Act 4:** neither guard prompt, audit prompt nor evidence export was run. Evidence: `step-2.json`, `requests.txt`.
- **NOT SHOWN — Act 5:** no social-section preparation, approval, narrative or UI comparison. Evidence: `step-2.json`, `requests.txt`.
- **NOT SHOWN — Five-act duration ≤8 minutes:** final Act 1 attempt stopped after prompt 1.1; prompt-to-settled-answer time was 34.5 seconds, including polling latency. Act 1 from its initial prompt through all fix rounds to the final answer took 603.1 seconds (10 minutes 3.1 seconds). The rest have no duration. Evidence: `step-2.json`.
- **NOT SHOWN — No secret in recording/screenshots:** committed screenshots and textual evidence were reviewed and scanned; no recording was made, so the full recording criterion is not established. Evidence: `redaction-scan.json`, `step-2.json`, `step-1.png`, `act-1-1.png`.

## Setup and bounded fixes

`go build ./...` passed. A temporary local preflight server exposed 17 tools and passed all seven MCP checks, including bad-key rejection. It was stopped before the browser run. The synthetic reset completed in **111.4 seconds**, with 126 read-backs and zero mismatches (`step-0.json`). The real report document was found by a read-only Workiva API listing, with exactly one name match; its ID and all real mappings remain in ignored local files.

The main server used API-key authentication, live Workiva, `NL_DEMO_PIPELINE=true`, and the prepared SQLite database throughout the browser attempts. Copilot listed all 17 tools after refresh (`step-1.json`, `step-1.png`). The start script initially failed its tunnel verification; a fresh seven-check verification passed without restarting the server. Kimi reused the existing signed-in Edge browser. Playwright browser execution was not shown. Workiva sign-in was completed by Tom; no password was handled by the agent.

The baseline searched descriptions, while the mapping search matches field names and aliases. Fix 1 added short keys, but the editor did not persist the programmatic fill (`act-1-1-failed-round0.json`, `prompt-1.1-failed-round1.txt`). Fix 2 used editor input that persisted and clarified current-year source fields versus cleared report targets; values and units were right but citations were raw IDs (`prompt-1.1-failed-round2-citations.txt`). Fix 3 resolved IDs through the spreadsheet catalog; the final answer then omitted the unit (`prompt-1.1.txt`). No fourth fix was attempted.

A message sent before one new test session had reconnected disappeared before reaching the agent. It produced no answer or tool calls and was resent once after the greeting appeared. This was a browser setup issue, not an additional fix round.

Only demo agent instructions and run documentation changed. No Go code, frozen packages, tests, checks or narrative style rules changed. No browser-stage Workiva writes occurred after the authorised seed. All raw snapshots, raw ID-bearing screenshots, local credentials, tunnel details and the database remain ignored. The unresolved requirement is a consistently explicit unit in each answer; later acts and their UI read-backs remain untested in this run.
