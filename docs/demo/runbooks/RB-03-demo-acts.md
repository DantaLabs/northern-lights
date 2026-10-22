# RB-03 Demo acts in Copilot

Run by Tom in the browser on Wednesday 28 October, then before each rehearsal. Uses the prompts in `docs/demo/agent/DEMO_PROMPTS.md`.

## Setup (15 minutes)

1. On `claude/demo-integration`, run `nl-demo-seed --reset` against the sandbox.
2. Start the server with `NL_DEMO_PIPELINE=true` and the sandbox mappings (`scripts/test-session.sh start` with those variables in `deployments/.env`).
3. In Copilot Studio, refresh the MCP tool so the agent sees 17 tools. Paste the current `docs/demo/agent/INSTRUCTIONS.md` and the narrative style rules into the agent instructions.
4. Open Teams with the agent on the left half of the screen and the Workiva report spreadsheet and report document on the right half. Start screen recording.

## Acts

1. Ask: run the act 1 prompts. Screenshot each answer (`act-1-<n>.png`).
2. Catch: run the act 2 prompt. Screenshot the findings (`act-2.png`).
3. Build: approve the change set, ask for the narrative, and give the synthetic reason for the business travel rise from prompt 3.2a before approving the section text. Screenshot the Workiva spreadsheet and document after each (`act-3-values.png`, `act-3-section.png`).
4. Guard: run the act 4 prompts (edit the policy paragraph, use an invented number, show the audit trail). Screenshot each refusal and the audit rows (`act-4-<n>.png`).
5. Repeat: run the act 5 prompt for the social section and approve both steps (`act-5.png`).
6. Stop the recording. Save `scripts/test-session.sh requests 60` output as `requests.txt`. Run `scripts/test-session.sh stop`.

## Checklist

- [ ] Act 1: each comparison reads the current and prior cells with workiva_get_field or workiva_read_range, gives both values with units, periods and separate spreadsheet/sheet/cell citations, and states the direction in words without computing a percentage; the numbers match the Workiva screen. **FAIL (2026-10-21):** act-1-1.png; prompt-1.1.txt in `docs/demo/evidence/2026-10-21-RB-03/`.
- [ ] Act 2: findings appear before the change set and match `configs/demo/data/README.md`. **NOT SHOWN (2026-10-21):** checklist.md; step-2.json in `docs/demo/evidence/2026-10-21-RB-03/`.
- [ ] Act 3: one approval wrote every change set field; the report document shows "Environmental disclosures" with the narrative and the unchanged verbatim block. **NOT SHOWN (2026-10-21):** checklist.md; step-2.json in `docs/demo/evidence/2026-10-21-RB-03/`.
- [ ] Act 3: the narrative follows `NARRATIVE_STYLE.md` rules 1 to 4 (movement first, only fact numbers, material changes first, percentage points for shares). **NOT SHOWN (2026-10-21):** checklist.md; step-2.json in `docs/demo/evidence/2026-10-21-RB-03/`.
- [ ] Act 4: the verbatim edit and the invented number were both refused with the server's message; workiva_audit_trail (`limit: 100`) shows the 25 environmental field-write rows and one completed section row matched to confirmation audit IDs, with actor, timestamp, target, before/after or section details, and Workiva operation URL. Repeating the audit prompt after Act 5 shows all 41 field writes and both section rows for the current run. **NOT SHOWN (2026-10-21):** checklist.md; step-2.json in `docs/demo/evidence/2026-10-21-RB-03/`.
- [ ] Act 5: the social section completed without code or configuration changes during the run. **NOT SHOWN (2026-10-21):** checklist.md; step-2.json in `docs/demo/evidence/2026-10-21-RB-03/`.
- [ ] Total run time of the five acts is 8 minutes or less. **NOT SHOWN (2026-10-21):** checklist.md; step-2.json in `docs/demo/evidence/2026-10-21-RB-03/`.
- [ ] No token or secret is visible in the recording or the screenshots. **NOT SHOWN (2026-10-21):** checklist.md; step-2.json in `docs/demo/evidence/2026-10-21-RB-03/`.
