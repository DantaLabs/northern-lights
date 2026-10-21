# Run 20261020-0935: L2 section recipe

Spec: `docs/specs/demo-l2-section-recipe.md`. Branch `claude/demo-l2-section-recipe` into `claude/demo-integration`.

## Outcome

**Done.** The owner approved deleting `notImplementedYet` from L0's demo.go in this PR, since no L0 session is active, and golangci-lint now reports 0 issues. The original block is kept below for the record.

Previously **blocked on one owner decision.** All 6 tasks passed. Final review 1 passed at 86781f2. After that, L1 and L3 were merged in from `claude/demo-integration`, and the owner's integration requests were done in ebea697. Final review 2 returned **revise** with one major finding. golangci-lint (`unused`) fails on `notImplementedYet` in `internal/mcpserver/tools/demo.go`. Before the merge, the L2 tool stub was the last thing that called it. `demo.go` belongs to L0, so the lane rules (AGENTS.md rule 1) do not let L2 delete it. STATUS.md now has an L2 -> L0 request to delete the function.

**Block question:** Should the L2 PR delete the dead `notImplementedYet` helper from the L0 file `demo.go`? This is the only change needed for green lint. The alternative is for L0 to land that deletion before this PR merges.

## Integration done in this session (owner request)

1. Merged `origin/claude/demo-integration` (L1 #19 and L3 #18) at 48e43fe. The only conflict was in `docs/demo/STATUS.md`. It kept the L1 and L3 rows from integration and the L2 row from this branch.
2. `internal/recipe/integration_test.go` runs Prepare on the real `changeset.New` with the offline demo client and the audit log. It stages 25 items, and Summary has 25 `awaiting_confirmation` lines. Unit tests keep their fakes.
3. `recipe.New` returns the FactStore that `wireDemoPipeline` passes to `sectiontext.New`. This was already true, and the same integration test now proves it: L3's `Stage` accepts 1.98 GWh and 2.03 GWh from the prepared facts, refuses 7.77, and refuses an analyst who has not prepared the section.
4. `NL_DEMO_DOCUMENT_ID` was implemented by T2. In sandbox mode only, a non-blank value replaces every recipe's `document_id`; demo mode and a nil Cfg keep the YAML value. The committed recipes hold only `demo-doc-annual-report`.
5. `workiva_prepare_section` now returns L1's `Preview.Summary`, and the local copy `changeSetSummary` is deleted. This was the blocker listed in L2's STATUS row.

## Requirement coverage

final-1 checked R1 to R13 and every owner constraint against tests (see `reviews/final-1.json`). final-2 checked them again after the merge and confirmed they are all still satisfied (`reviews/final-2.json`).

## Tasks

| Task | Status | Attempts | Worker | Reviewers (spec/standards) | Policy |
|---|---|---|---|---|---|
| T1 facts and display forms | passed | 2 | sonnet | opus/opus | hint (shadow) |
| T2 recipe loading, sections, document ID override | passed | 2 | sonnet | opus/opus | hint (shadow) |
| T3 fact store | passed | 1 | sonnet | opus/opus | hint (shadow) |
| T4 Prepare | passed | 2 | sonnet | opus/opus | hint (shadow) |
| T5 workiva_prepare_section tool | passed | 1 | sonnet | opus/opus | hint (shadow) |
| T6 scenario 9 on L4 demo data, STATUS | passed | 1 | sonnet | opus/opus | hint (shadow) |

## Critique

- c1 (revise): it had 2 major findings. The first was that there was no end-to-end first slice, so T5 and T6 now test the tool output against the schema on demo data. The second was that the display rules were not pinned exactly for L3's guard, so T1 now pins the exact display forms, including unsigned magnitudes. Its 8 minor findings were folded into the task instructions.
- c2 (accept): 6 minor findings, all resolved in instructions or left as noted.

## Defects attributed to passed tasks

- T5, major (final-2): `notImplementedYet` is unused after the integration merge. T5's diff only caused this together with L1 and L3 removing their callers.
- T6, minor (final-2): the STATUS L2 row was stale after ebea697. It was fixed in the finish commit.

## Open minor findings and nits (final-1, still open)

- T4: the formula path (`CalculatedValue`), a text prior counting as missing, and the `sign_change` guard when current is 0 have no tests.
- T2: two load validations have no tests, duplicate field keys and decimals outside 0..6. `service.storageCtx` is dead code.
- T1 (nit): percent display forms are not thousands-grouped.
- T4 (nit): a redundant sort in `unknownSection`.

## Checks at HEAD

- These pass: `go build ./...`, `go vet ./...` and `gofmt -l .` (which prints nothing).
- `go test -race -count=1` passes for `./internal/recipe/...`, `./internal/mcpserver/tools`, `./cmd/workiva-mcp/...`, `./internal/changeset/...` and `./internal/sectiontext/...`.
- The full `go test ./... -race` ran only at 9a4446d (`checks/T6-a1.log`). CI runs it on the PR.
- `golangci-lint run` reports 1 issue: `notImplementedYet` is unused (see Outcome).

## Replays

No worker replays were due. Two reviewer replays ran: Sonnet reviewer-spec re-ran T1 attempt 1 and T4 attempt 1, the two attempts where Opus spec review had asked for changes. Both returned revise with a major finding, so both are recorded as caught. T1: the materiality equality boundary is untested. T4: R7's percent-scaled min/max check is not proven.

## Stats

```
tasks 6, labeled worker decisions 0 (partial 0, unlabeled 0)
mean regret per decision: rules n/a, hint n/a, always-sonnet n/a
rules under-route None, over-route None
first-attempt pass rate: {}
reviewer escape rate by pair (spec/standards): {"opus/opus": 0.167}
sonnet spec-review catch rate on opus findings: 1.0 (2 replays)
defects by source: {"final": 2}
mode {"worker": "shadow", "reviewer": "shadow"}; promotion worker HOLD, reviewer HOLD
```

## Follow-ups

- L0 deletes `notImplementedYet`, or the owner approves deleting it in this PR.
- Close the final-1 minor test gaps listed above.
- Entra `requiredPermission` entries for the four demo tools are still with the owner (STATUS request from L0).
