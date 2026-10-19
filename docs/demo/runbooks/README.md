# Runbooks and evidence

A runbook is a test with numbered steps, a checklist and an evidence folder. Agents run the ones that only need the API and Tom's local `.env`; Tom runs the ones that need a browser (Copilot Studio, Teams, Workiva UI), because browser automation against those sites gets blocked by bot protection and costs more time than it saves.

| Runbook | Who runs it | Needs | Time |
|---|---|---|---|
| RB-01 Baseline smoke | Any agent on Tom's machine | `deployments/.env` with sandbox credentials | 20 minutes |
| RB-02 Documents API spike | L3 agent on Tom's machine | Same | 1 to 2 hours |
| RB-03 Demo acts in Copilot | Tom | Copilot Studio test agent, Teams, Workiva UI | 45 minutes |

## Evidence convention

1. Create `docs/demo/evidence/<YYYY-MM-DD>-<RB-id>/` (for example `2026-10-20-RB-01/`).
2. Copy the runbook's checklist into `checklist.md` in that folder and tick each item, adding the file name that proves it.
3. Save screenshots as `step-<n>.png`, tool outputs as `step-<n>.json`, and the redacted request log as `requests.txt`.
4. Remove tokens, API keys and client secrets before committing. Actor emails of test users may stay.
5. Commit the folder on `claude/demo-integration` with the message "Evidence: <RB-id> <date> <pass or fail>" and update the "Runbook results" table in `docs/demo/STATUS.md`.

## Evidence review prompt

Run a light agent (Haiku or Sonnet) with this prompt after each evidence commit:

> Read docs/demo/runbooks/<RB-id>.md and the folder docs/demo/evidence/<folder>/. For each checklist item, answer PASS, FAIL or NOT SHOWN, with the evidence file that supports it and one sentence of reasoning. Flag any token, key or secret visible in the files. Do not run anything. End with the list of FAIL and NOT SHOWN items.

## Code review prompt

Run a light agent (Sonnet) on every lane pull request:

> Review this pull request against docs/specs/<lane spec>.md and AGENTS.md. Report: (1) requirements not met or not tested, by R number; (2) edits outside the lane's files in docs/demo/LANES.md; (3) changes to existing tests or lint configuration; (4) bugs you can point to in the diff, with file and line. No style comments. End with "merge" or "do not merge" and the reason in one sentence.
