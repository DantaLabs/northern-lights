# Demo status

Each agent edits only its own lane row, at the end of every work session. Tom reads this file each morning.

Plan day: 1 of 11 (Monday 19 October 2026).

| Lane | Status | Branch | Last commit | Blocked on |
|---|---|---|---|---|
| L0 Scaffold | not started | `claude/demo-l0-scaffold` | | Tom approves the spec |
| L1 Change set | waiting for L0 | `codex/demo-l1-change-set` | | L0 merge |
| L2 Section recipe | waiting for L0 | `claude/demo-l2-section-recipe` | | L0 merge |
| L3 Section text | RB-02 spike can start now | `codex/demo-l3-section-text` | | RB-02 result for the document writer |
| L4 Data and agent | can start now (YAML, data, instructions) | `claude/demo-l4-data-and-agent` | | Sandbox names from RB-01 |
| L5 Manual tests | RB-01 today | `claude/demo-integration` | | |

## Runbook results

| Runbook | Date | Result | Evidence |
|---|---|---|---|
| RB-01 Baseline smoke | | | |
| RB-02 Documents API spike | | | |
| RB-03 Demo acts in Copilot | | | |

## Requests to other lanes

Write one line: requesting lane, owning lane, file, what you need and why.
