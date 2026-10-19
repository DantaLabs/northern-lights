# Demo L0 scaffold: run 20261019-0702

Spec: `docs/specs/demo-l0-scaffold.md`. Run branch: `claude/demo-l0-scaffold`, based on `claude/demo-integration` at 83520e7. Outcome: **done** for R1–R9, R11 and R12. R10 is deferred.

## What this PR gives the lanes

- `NL_DEMO_PIPELINE=true` (or YAML `demo_pipeline_enabled: true`) registers `tools.Demo()` after `tools.All()`. That gives 17 tools; with the flag off the server keeps the current 13.
- The four demo tool stubs have their final schemas: `workiva_prepare_section`, `workiva_confirm_change_set`, `workiva_stage_section_text` and `workiva_confirm_section_text`. If a `Deps` field is nil, a tool names the missing dependency. Otherwise it returns "not implemented yet".
- Interface files and stub constructors are in place: `changeset.New`, `recipe.New` and `sectiontext.New`. `main.go` calls each one once (`wireDemoPipeline`), so the lanes only replace stub bodies.
- `demoenv.Env` is the struct passed to every lane constructor.
- Demo transport hooks: `/documents` and `/content` go to `demoDocumentsResponse` (L3), and `demo-esg-*` spreadsheet IDs go to `demoESGResponse` (L4). Both return 404 JSON until the lane fills them in.
- In demo mode, `-mappings` is loaded after the built-in demo mappings. If the file is missing, startup fails.
- `scripts/verify-mcp.sh` reads the expected tool count from `NL_EXPECT_TOOLS`, default 13.

## Things the lanes need to know

- The lanes own these test files and replace them with their implementations:
  - `change_set_test.go` (L1)
  - `prepare_section_test.go` (L2)
  - `section_text_test.go` (L3)
  - `demo_documents_test.go` (L3)
  - `demo_esg_test.go` (L4)
  - the stub `service_test.go` in each lane package
- The L0-owned tests only check behaviour that stays true after the stubs are replaced: tool order, closed and bounded schemas, the missing-dependency message, that each tool reaches its service, and the 13 and 17 tool counts.
- Contract for the lanes: each `New` must succeed in demo mode when `NL_DEMO_SECTIONS_DIR` points at an empty directory. The flag-on server tests depend on this.
- The Go SDK sorts `tools/list` by name. The R2 order is therefore checked on `Demo()` and on `Registry.Names()`, not on the wire.

## Requirement coverage (from final review final-1: pass)

| Req | Evidence |
|---|---|
| R1 | `internal/config/demo_pipeline_test.go` `TestDemoPipelineEnabled` |
| R2 | `tools/demo_test.go` `TestDemoToolsOrder` (Demo() and Registry.Names()) |
| R3, R4 | `cmd/workiva-mcp/demo_pipeline_test.go`: flag off gives tools.All() (13), flag on gives 17; existing 13-tool tests unchanged |
| R5, R6 | `TestDemoToolSchemasAreClosedAndBounded`, `TestDemoToolsNameMissingDependency`, lane-owned stub tests |
| R7 | interface files; `go build`, `go vet` and `go doc` signature checks |
| R8 | `TestDemoTransportRoutesLaneHooks`, `TestDemoDocumentsResponseNotFound`, `TestDemoESGResponseNotFound` |
| R9 | demo-mode mappings tests, including a missing `-mappings` file |
| R10 | **deferred** (T7). Needs its own run after L1–L4 merge (26–27 Oct) |
| R11 | `cmd/workiva-mcp/verify_mcp_script_test.go` `TestVerifyMCPScript` runs the real script with the flag off, with the flag on and `NL_EXPECT_TOOLS=17`, and with the flag on and the default expectation (must fail) |
| R12 | `wireDemoPipeline` plus `TestDemoPipelineServicesReachTools` |

The acceptance scenarios 1–6, 8 and 9 each exist as passing tests. The final check on 78dea3d was all green: `go test ./... -race`, gofmt, `go vet` and golangci-lint (0 issues).

## Tasks

| Task | Covers | Worker | Reviewers (spec/standards) | Attempts | Status |
|---|---|---|---|---|---|
| T1 interfaces, demoenv, stub constructors | R7, R12 | haiku | opus/sonnet | 1 | passed |
| T2 config flag | R1 | haiku | opus/sonnet | 1 | passed |
| T3 Deps fields, Demo(), tool stubs | R2, R5, R6 | sonnet | opus/opus (risky) | 1 | passed |
| T4 demo transport hooks | R8 | haiku | opus/sonnet | 1 | passed |
| T5 main.go wiring, demo-mode mappings | R3, R4, R9, R12 | sonnet | opus/sonnet | 1 | passed |
| T6 verify-mcp.sh, STATUS.md row | R11, R4 | haiku | opus/opus (risky) | 1 | passed |
| T7 end-to-end test | R10 | n/a | n/a | 0 | deferred |

The router was Laya (local) in shadow mode, so the planner's model hints were used. The planner, plan critic and final reviewer ran on Opus.

## Critique

- **c1 (revise).** The major finding: L0 tests would break once the lanes replace their stubs. Fixed by moving the stub assertions into lane-owned test files and adding the empty-sections-dir contract. Minor findings were also fixed:
  - the full suite and golangci-lint added to T6
  - the skip placement in the script test
  - a missing `-mappings` file now fails startup
  - the Entra gap is recorded
- **c2 (revise).** The blocking finding: the Go SDK sorts `tools/list`, so an order check on the wire would always fail. Fixed by comparing name sets on the wire and checking the order on the registry. Minor findings were also fixed: separate 404 messages for each hook, no shared stub helper, and anchored PASS greps.
- **Deviation from the process:** the c2 fixes went in without a third critique, because `plan_critic_rounds` is 2. Before applying them, the orchestrator confirmed the SDK sorting claim in `go-sdk` `featureSet.sortKeys`.

## Minor findings and nits (not fixed; for lanes or later)

- The `blocks` input to `workiva_stage_section_text` has no `minItems: 1`. L3 R3 says 1 to 20 blocks, so L3 should add it.
- The Go output types of the confirm tools have no `Status` field. The schemas already allow one; L1 and L3 add it to the Go types.
- No test checks the exact schema property names. The lanes own those files.
- No test counts the constructor calls. The code calls each constructor once in `wireDemoPipeline`.
- The `changeset.New` doc comment mentions `NL_DEMO_SECTIONS_DIR`, which belongs to L2.
- T5 test code: a hand-written `contains` duplicates `slices.Contains`, and `searchFieldNames` uses unchecked type assertions.
- T6 test code: the name `wantExit` reads backwards, the script subprocess has no timeout, and `NL_EXPECT_TOOLS` validation has no test.
- The two lane hook 404 helpers are identical on purpose: L3 and L4 each own one file.

## Open items

1. **Entra permissions (needs an owner).** `internal/mcpserver/server.go` `requiredPermission` has no entries for the four demo tools. Under `NL_AUTH_MODE=entra`, which RB-03 and Copilot Studio use, every demo tool call is denied. The confirm tools are also missing from `writeTools`. No lane owns `server.go`. Suggested mapping:
   - `prepare_section` and `stage_section_text`: `workiva.write.preview`
   - `confirm_change_set` and `confirm_section_text`: `workiva.write.confirm`
   - all four tools listed in `writeTools`

   This is also recorded under "Requests to other lanes" in `docs/demo/STATUS.md`.
2. **R10 end-to-end test (T7).** Deferred until L1–L4 merge, then planned in a new run on `claude/demo-integration`.

## Execution notes

- Agent `isolation: worktree` created worktrees from a different local checkout of this repo, not from the run's clone. Those workers stopped without changes, and the worktrees were cleaned up. The tasks were then run in task worktrees made inside the run's clone (`.scratch/wt/<task>`), with T1, T2 and T4 running in parallel.
- `new-run` creates `pipeline/<run-id>`. The owner asked for `claude/demo-l0-scaffold`, so the branch was renamed and `state.json` updated.
- The spec's approval line was set to "Approved: yes" on the owner's ship instruction, with R10 noted as deferred.
- No replays: `replay-candidates` returned none.

## Stats

```
tasks 7, labeled worker decisions 0 (partial 0, unlabeled 0)
reviewer escape rate by pair (spec/standards): {"opus/sonnet": 0.0, "opus/opus": 0.0}
defects by source: {}
mode {"worker": "shadow", "reviewer": "shadow"}; promotion worker HOLD, reviewer HOLD
```
