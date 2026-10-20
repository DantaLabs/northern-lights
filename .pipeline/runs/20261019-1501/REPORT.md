# Demo L4: data and agent (run 20261019-1501)

Spec: `docs/specs/demo-l4-data-and-agent.md`. Branch: `claude/demo-l4-data-and-agent`, from `claude/demo-integration`.

Outcome: **done**. R1 to R11 are implemented, and the final review (final-1) passed with no blocking or major findings. Two acceptance scenarios wait on other lanes: scenario 5 needs L2's prepare tool, and scenario 6 is Tom's RB-03 run.

## What this gives the demo

- **Demo company:** a fictional company, "Demo Harbour Components Ltd" (`configs/demo/company.md`).
- **Data:** synthetic ESG data for 2024 and 2025, with 26 environmental and 16 social fields, in the KPMG ESG template's order and labels (`configs/demo/data/`).
- **Planted findings:** exactly four (`configs/demo/data/README.md`):
  - one missing value
  - an environmental change above 10%
  - a social change above 10%
  - one value below its minimum
- **Section recipes:** `esg-environmental` and `esg-social`, written to L2's recipe spec. Each has one verbatim block written for the demo.
- **Mappings:**
  - `mappings-demo.yaml` for offline demo mode.
  - `mappings-sandbox.example.yaml` with placeholder IDs. The real `mappings-sandbox.yaml` is git-ignored, because the repository is public, and `configs/demo/make_sandbox_mappings.py` generates it locally.
- **Offline demo data:** in demo mode the server serves the ESG CSV data for `demo-esg-*` spreadsheets (`internal/workiva/demo_esg.go`).
- **Seed command, `nl-demo-seed`:** plan, `--dry-run` (sends nothing), live apply with read-back, and `--reset`. `--reset` clears the 2025 target cells and deletes only sections whose titles exactly match a recipe title.
- **Live seed, run on 2026-10-19 against the sandbox with Tom's approval:**
  - 84 writes and 42 clears.
  - No section had a recipe title, so nothing was deleted.
  - Read-back: 126 cells ok, 0 mismatch.
  - It took 99.7 s.
- **Copilot agent docs:** `docs/demo/agent/INSTRUCTIONS.md`, `NARRATIVE_STYLE.md` and `DEMO_PROMPTS.md`, covering acts 1 to 5 with what a correct answer contains.
- **IT brief:** `docs/demo/IT_BRIEF.md`, 877 words. It covers data flow, identity, writes, audit trail and open risks.

## Sandbox facts found by this run (`configs/demo/README.md`)

- Clearing a cell needs the empty string: Workiva rejects `null` with HTTP 400.
- The template's totals are formulas. Source values therefore go into its empty input cells, and report values into an empty block in columns I to L of `CY Data Output` and `PY Data Output`.

## Requirement coverage (final-1)

| Requirement | Evidence |
|---|---|
| R1 | `company.md`, checked by T3's acceptance commands |
| R2, R3, R4 | `cmd/nl-demo-seed` `TestDataConsistency`: totals, shares between 0 and 100% in both years, changes within ±30%, exactly the four planted findings |
| R5 | `TestRecipesMatchSchemaAndMappings`. Mirrors L2's spec, so it is only partly proven until L2 merges. |
| R6 | Per-field spreadsheet and sheet assertions for every source, target and prior field; the example file holds placeholders only |
| R7 | `internal/workiva` `TestDemoESG*` (10 tests), plus `TestDemoReadReturnsCSVValue` (scenario 3) |
| R8 | Plan, dry run (scenario 4), apply and reset tests against fakes, plus the live seed |
| R9, R10 | Agent docs checked by grep. Scenario 6 (RB-03) is deferred. |
| R11 | `IT_BRIEF.md`: 900-word cap, required headings, every risk has a mitigation or says open |

Final checks on 7002483 all pass:
- `go test ./... -race -timeout 40m` (see the waiver below)
- gofmt, go vet, golangci-lint
- `go build ./...`

## Tasks

| Task | Worker | Reviewers | Attempts | Status |
|---|---|---|---|---|
| T1 sandbox discovery, write probe, field list, example mapping | sonnet | opus/opus | 1 | passed |
| T2 ESG demo transport | sonnet | opus/opus | 1 | passed |
| T3 company, data, recipes, verbatim, demo mapping | sonnet | opus/opus | 1 | passed |
| T4 seed loaders and data tests | sonnet | opus/opus | 2 | passed |
| T5 seed plan and dry run | sonnet | opus/opus | 1 | passed |
| T6 live apply, read-back, section cleanup | sonnet | opus/opus | 1 | passed (owner waiver, see below) |
| T7 agent instructions, style, demo prompts | sonnet | opus/opus | 3 | passed |
| T8 IT and risk brief | sonnet | opus/opus | 1 | passed |
| T9 live seed and redacted record | sonnet | opus/opus | 1 | passed |

The router was Laya (local, shadow mode, with fallback). Every task matched a risky keyword, so all reviews ran on Opus.

Retries:
- **T4 attempt 1:** three `//nolint:gosec` comments, which AGENTS.md rule 3 forbids, and R6 checked only three fields.
- **T7 attempt 1:** the prompts claimed four planted findings for Act 2.
- **T7 attempt 2:** the style conventions sat outside the part pasted into Copilot Studio.

## Waiver: T6 full-suite acceptance (approved by Tom, 2026-10-20)

`go test ./... -race` timed out after 600 s in `internal/assurance`. That package is frozen and this run did not touch it.

- On the unchanged integration branch the package alone takes 613 to 644 s under `-race` on this machine.
- It passes with `-timeout 40m`, and GitHub CI passes it.
- No test or timeout was changed.

Details are in `checks/T6-a1-waiver.md`.

## Critique

- **c1 (revise):**
  - read numbers as exact decimals
  - move the live write probe forward to T1
  - keep evidence out of L5's folder
  - drop the copy of the real-ID mapping into the run checkout
  - merge tasks, from 12 to 9
- **c2 (revise):** T9 may fix its own tests if the live conventions differ; planted material changes go on fields that are not percentages; stopping becomes a block question; leaks are checked with grep.

The c2 fixes went in without a third critique, because `plan_critic_rounds` is 2.

## Defects found by the final review in passed tasks (minor)

- **T6:** read-back makes one API call per cell, 126 in total, which is why the seed missed the one-minute target. Reading one range per sheet would cut that to about 4 calls.
- **T7:** prompt 1.2 cites a `CY Data Output` cell that `--reset` leaves empty during act 1.
- **T1 and T3:** the `configs/demo/README.md` file list is out of date. The README also says the seed fills the waste type and unit cells, which it does not.

Nits:
- the overlay map in `demo_esg.go` is never cleaned up (T2)
- listing report sections has no page cap (T6)

## Follow-ups and open items

1. Scenario 5: after L2 merges, run `nl-demo-seed --reset` and then `workiva_prepare_section esg-environmental` in the sandbox.
2. Scenario 6: Tom's RB-03. Prompt 4.3 assumes an evidence subject exists, but `workiva_export_evidence` cannot export a section directly.
3. The IT brief describes L1 and L3 controls (the 10-minute token, read-back) from their specs. Check it again after those lanes merge.
4. `NARRATIVE_STYLE.md` rules 2 and 3 say `Display`/`Material`; the tool output uses `display`/`material`.
5. Requests in STATUS.md:
   - L2: a way to use the real report document ID without committing it.
   - L0: list the `demo-esg-*` spreadsheets in demo mode, and add an overlay field on DemoTransport.
6. Still open from L0: the Entra permission mapping for the demo tools.
7. `check_no_sandbox_ids.py` only finds IDs in JSON-style keys. It needs the RB-01 local JSON files to be effective.
8. The `make_sandbox_mappings.py` safety checks (git-ignore guard, placeholder errors) have no tests.

## Execution notes

- Task worktrees were made under `.scratch/wt/` inside the run checkout and removed after each merge, because agent worktree isolation targets the wrong repository here.
- One stray `__pycache__` showed up in a task worktree; `configs/demo/.gitignore` now ignores it.
- A usage limit paused the run between T4/T7 attempt 1 and the retries. It resumed from the committed checkpoint.
- One spec verdict used severities the validator rejects. It was treated as revise, as the rules say.

## Stats

```
tasks 9, labeled worker decisions 0 (partial 0, unlabeled 0)
reviewer escape rate by pair (spec/standards): {"opus/opus": 0.0}
sonnet spec-review catch rate on opus findings: 1.0 (2 replays)
defects by source: {"final": 4}
mode {"worker": "shadow", "reviewer": "shadow"}; promotion worker HOLD, reviewer HOLD
```
