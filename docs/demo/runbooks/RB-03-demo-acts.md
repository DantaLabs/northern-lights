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
3. Build: approve the change set, then approve the section text. Screenshot the Workiva spreadsheet and document after each (`act-3-values.png`, `act-3-section.png`).
4. Guard: run the act 4 prompts (edit the policy paragraph, use an invented number, export evidence). Screenshot each refusal and the export (`act-4-<n>.png`).
5. Repeat: run the act 5 prompt for the social section and approve both steps (`act-5.png`).
6. Stop the recording. Save `scripts/test-session.sh requests 60` output as `requests.txt`. Run `scripts/test-session.sh stop`.

## Checklist

- [ ] Act 1: every answer gives value, unit, period and spreadsheet, sheet and cell; the numbers match the Workiva screen.
- [ ] Act 2: findings appear before the change set and match `configs/demo/data/README.md`.
- [ ] Act 3: one approval wrote every change set field; the report document shows "Environmental disclosures" with the narrative and the unchanged verbatim block.
- [ ] Act 3: the narrative follows `NARRATIVE_STYLE.md` rules 1 to 4 (movement first, only fact numbers, material changes first, percentage points for shares).
- [ ] Act 4: the verbatim edit and the invented number were both refused with the server's message; the evidence export returned a file or link.
- [ ] Act 5: the social section completed without code or configuration changes during the run.
- [ ] Total run time of the five acts is 8 minutes or less.
- [ ] No token or secret is visible in the recording or the screenshots.
