# Lanes

Five lanes run in parallel after L0 merges. Each lane owns its files outright, so two agents never edit the same file and merges do not conflict. Ownership is exclusive: if a file is not listed for your lane, do not edit it.

## Branches and merge order

All lane branches start from `claude/demo-integration` and open pull requests into it. Tom merges. `claude/demo-integration` goes to `main` after the demo.

1. L0 merges first (target: Tuesday 20 October, midday). It creates every shared file and every interface, so the other lanes only fill in their own packages.
2. L1, L2, L3 and L4 merge in any order once their CI is green.
3. L0's author adds the end-to-end demo-mode test after L1 to L4 have merged (Monday 26 to Tuesday 27 October).

## Lane table

| Lane | Agent | Branch | Spec | Owns |
|---|---|---|---|---|
| L0 Scaffold | Claude Code | `claude/demo-l0-scaffold` | `docs/specs/demo-l0-scaffold.md` | Shared touch points and interface files (list below), `internal/demoe2e/` |
| L1 Change set | Codex | `codex/demo-l1-change-set` | `docs/specs/demo-l1-change-set.md` | `internal/changeset/`, `internal/mcpserver/tools/change_set.go` and its tests |
| L2 Section recipe | Claude Code | `claude/demo-l2-section-recipe` | `docs/specs/demo-l2-section-recipe.md` | `internal/recipe/`, `internal/mcpserver/tools/prepare_section.go` and its tests |
| L3 Section text | Codex | `codex/demo-l3-section-text` | `docs/specs/demo-l3-section-text.md` | `internal/sectiontext/`, `internal/workiva/documents.go`, `internal/workiva/demo_documents.go`, `internal/workivaprovider/documents.go`, `internal/mcpserver/tools/section_text.go`, and their tests |
| L4 Data and agent | Claude Code | `claude/demo-l4-data-and-agent` | `docs/specs/demo-l4-data-and-agent.md` | `configs/demo/`, `cmd/nl-demo-seed/`, `docs/demo/agent/`, `docs/demo/IT_BRIEF.md`, `internal/workiva/demo_esg.go` and its test |
| L5 Manual tests | Tom, with a reviewer agent | commits on `claude/demo-integration` | runbooks | `docs/demo/runbooks/`, `docs/demo/evidence/` |

Every lane also edits its own row in `docs/demo/STATUS.md`.

## Shared touch points (L0 only)

- `cmd/workiva-mcp/main.go`: registers `tools.Demo()` when `NL_DEMO_PIPELINE=true`.
- `internal/config/config.go`: the `DemoPipelineEnabled` flag and its environment variable.
- `internal/mcpserver/tools/demo.go`: the `Demo()` list with the four tool constructors.
- `scripts/verify-mcp.sh`: expected tool count from `NL_EXPECT_TOOLS`.
- `internal/mcpserver/registry.go`: any new field on `Deps` (for example `ChangeSets`, `Recipes`, `SectionText`).
- `internal/workiva/demo_client.go`: one hook that sends `/documents` and `/content` paths to `demoDocumentsResponse` in `demo_documents.go` (L3) and `demo-esg-` spreadsheet IDs to `demoESGResponse` in `demo_esg.go` (L4).
- Interface files, created by L0 and then owned by the lane named: `internal/changeset/changeset.go` (L1), `internal/recipe/types.go` (L2), `internal/sectiontext/types.go` (L3), `internal/workivaprovider/documents.go` (L3).

After L0 merges, a lane may add to its interface file but must not change or remove what L0 defined without a request in `STATUS.md`.

## Dependencies between lanes

| Needs | From | Until it lands, use |
|---|---|---|
| L2 stages a change set | L1 `changeset.Service` | A fake `changeset.Service` in L2's tests |
| L3 checks numbers against narrative facts | L2 `recipe.FactStore` | A fake `FactStore` in L3's tests |
| L2 and L3 need the section YAML and verbatim text | L4 `configs/demo/` | Fixtures under each package's `testdata/` |
| RB-03 needs everything | All lanes | Nothing; RB-03 runs on 28 October |

## Agent setup

Claude Code: open the repository on the lane branch, then run "ship docs/specs/demo-<lane>.md" once Tom has approved the spec. The pipeline planner splits the spec into tasks; reviewers run on Sonnet.

Codex: start each session with "Read AGENTS.md, docs/demo/DEMO.md, docs/demo/LANES.md and docs/specs/demo-<lane>.md. Implement the requirements in order. Each acceptance scenario becomes a test." Use one Codex session per lane, on its own worktree.

Code review: every pull request gets one review from a lighter agent (Sonnet or Haiku) with the prompt in `docs/demo/runbooks/README.md`, section "Code review prompt". Tom reads the review, not the full diff.
