# Plan: L4 data, section recipes and the Copilot agent

Run `20261019-1501` on branch `claude/demo-l4-data-and-agent`. Spec: `docs/specs/demo-l4-data-and-agent.md` (approved by Tom on 2026-10-19).

## Overview

Nine tasks in L4-owned files only (revised after critique c1). The first slice is a fail-fast discovery of the sandbox layout plus a live write probe on the `Testing MCP` scratch range (T1), run in parallel with the demo-mode transport (T2) and the IT brief (T8). Data, section recipes and demo mapping follow in one task (T3), then the nl-demo-seed command: loaders and data tests (T4), plan and dry run (T5), live apply (T6). T7 updates the agent documents. T9 runs the live seed and records a redacted summary in configs/demo/README.md and the L4 STATUS row.

## Decisions shared across tasks

- Section short names `env` and `soc`; mapped fields `src_<sec>_<key>`, `rpt_<sec>_<key>_2025`, `rpt_<sec>_<key>_2024`; mandatory keys `energy_total` and `employees_total`.
- Demo layout: `demo-esg-source` / `demo-esg-source-2025`, `demo-esg-report` / `demo-esg-report-cy` and `demo-esg-report-py`; header row then CSV rows in A:D, range `D<row>`; data from `NL_DEMO_ESG_DATA`.
- Public repository: `configs/demo/mappings-sandbox.yaml` is git-ignored; the committed example uses the RB-01 placeholders `<id-NN>` and `<document-id>`; `configs/demo/check_no_sandbox_ids.py` scans every committed file for real IDs.
- Mapping files carry a top-level `document_id` for the seed command (ignored by the server loader). Committed recipes use `document_id: demo-doc-annual-report`.
- Seed output never prints IDs: lines `write`, `clear`, `delete-section`, `planned:`, `read-back:`, `done:`.
- `docs/architecture.md` and `docs/adr/` are not L4-owned, so they are not edited; see assumptions.

## Tasks

| ID | Title | Kind | Tier | Covers | Depends on |
|---|---|---|---|---|---|
| T1 | Discover the sandbox layout, probe live write semantics, commit field list | data_extract | code | R2, R6 | none |
| T2 | Serve the ESG CSV data for demo-esg spreadsheets in demo mode, tests first | feature | code | R7 | none |
| T3 | Write the company, synthetic ESG data, section recipes and demo mapping | config | code | R1, R2, R3, R4, R5, R6 | T1 |
| T4 | Add nl-demo-seed loaders and data tests for consistency, recipes and demo reads | test | code | R2, R3, R4, R5, R6, R7 | T2, T3 |
| T5 | Build the seed plan and dry-run mode of nl-demo-seed, tests first | feature | code | R8 | T4 |
| T6 | Implement live apply with read-back and report section cleanup, tests first | feature | code | R8 | T5 |
| T7 | Update the Copilot agent instructions, style guide and demo prompts | docs | code | R9, R10 | T3 |
| T8 | Write the two-page IT and risk brief | docs | code | R11 | none |
| T9 | Run the live sandbox seed and record the redacted result in README and STATUS | other | code | R8 | T6 |

### T1: Discover the sandbox layout, probe live write semantics, commit field list

Kind `data_extract`, tier `code`, covers R2, R6, depends on nothing.

Owns: `configs/demo/.gitignore`, `configs/demo/README.md`, `configs/demo/check_no_sandbox_ids.py`, `configs/demo/mappings-sandbox.example.yaml`, `configs/demo/data/esg-2025.csv`, `configs/demo/data/esg-2024.csv`

Read the layout of the sandbox spreadsheets through the existing Workiva client, decide for every field the demo uses its key, label, unit and exact cells, and confirm the live write semantics the seed command will rely on. Requirement R2 (labels and order follow the KPMG ESG template) and the sandbox half of R6 depend on this. Fail fast: this is the riskiest step of the lane.

Acceptance:

- `git check-ignore -q configs/demo/mappings-sandbox.yaml`
- `! git check-ignore -q configs/demo/mappings-sandbox.example.yaml`
- `test $(grep -cE 'name: *src_env_[a-z0-9_]+' configs/demo/mappings-sandbox.example.yaml) -ge 20`
- `test $(grep -cE 'name: *src_soc_[a-z0-9_]+' configs/demo/mappings-sandbox.example.yaml) -ge 12`
- `grep -qE 'name: *src_env_energy_total\b' configs/demo/mappings-sandbox.example.yaml && grep -qE 'name: *src_soc_employees_total\b' configs/demo/mappin ... (see plan.json)`
- `grep -qE '^document_id: *"<document-id>"' configs/demo/mappings-sandbox.example.yaml`
- `! grep -E '^ *-? *id:' configs/demo/mappings-sandbox.example.yaml | grep -vqE '"<id-[0-9]{2}>"'`
- `! grep -E 'field_type:' configs/demo/mappings-sandbox.example.yaml | grep -vqE 'field_type: *(number|percent)\b'`
- `python3 -I -c ' ... (see plan.json)`
- `grep -qx '## Live write semantics' configs/demo/README.md && grep -qxE 'clear-value: (null|empty-string)' configs/demo/README.md && grep -qxE 'percent ... (see plan.json)`
- `grep -q 'Environmental disclosures' configs/demo/README.md && grep -q 'demo-esg-source' configs/demo/README.md`
- `python3 -I configs/demo/check_no_sandbox_ids.py "$HOME/northern-lights-workspace/versions/demo-l0/docs/demo/evidence/2026-10-19-RB-01/step-3.local.jso ... (see plan.json)`

### T2: Serve the ESG CSV data for demo-esg spreadsheets in demo mode, tests first

Kind `feature`, tier `code`, covers R7, depends on nothing.

Owns: `internal/workiva/demo_esg.go`, `internal/workiva/demo_esg_test.go`

R7: in demo mode the Workiva demo transport serves the ESG CSV data for spreadsheet IDs starting with `demo-esg-`, reading the directory named by `NL_DEMO_ESG_DATA` (default `configs/demo/data`). L0 created internal/workiva/demo_esg.go as a 404 stub and internal/workiva/demo_esg_test.go with TestDemoESGResponseNotFound; L4 owns both. Write the tests first, watch them fail against the stub, then implement. demo_client.go belongs to lane L0: do not edit it.

Acceptance:

- `go test ./internal/workiva/ -race -count=1`
- `go vet ./internal/workiva/`
- `test -z "$(gofmt -l internal/workiva)"`
- `golangci-lint run ./internal/workiva/...`
- `grep -q NL_DEMO_ESG_DATA internal/workiva/demo_esg.go`
- `! grep -q 'demo ESG fixture not served yet' internal/workiva/demo_esg_test.go`
- `for n in SheetsList SourceReadsCurrentYear PriorPeriodAndTarget WriteThenReadBack ValuesEndpoint UnknownIDs MissingDataDir DefaultDataDir KeepsVSMERou ... (see plan.json)`
- `! grep -q 'fmt.Sprint' internal/workiva/demo_esg_test.go && grep -q 'GetSheetDataTyped' internal/workiva/demo_esg_test.go`

### T3: Write the company, synthetic ESG data, section recipes and demo mapping

Kind `config`, tier `code`, covers R1, R2, R3, R4, R5, R6, depends on T1.

Owns: `configs/demo/company.md`, `configs/demo/data/esg-2025.csv`, `configs/demo/data/esg-2024.csv`, `configs/demo/data/totals.csv`, `configs/demo/data/README.md`, `configs/demo/sections/esg-environmental.yaml`, `configs/demo/sections/esg-social.yaml`, `configs/demo/sections/verbatim/env-policy.txt`, `configs/demo/sections/verbatim/social-policy.txt`, `configs/demo/mappings-demo.yaml`

Write the data the demo runs on (R1 to R4), the two section recipes with their verbatim blocks (R5) and the demo-mode mapping file (R6), in one task so the planted findings, which depend on both the values and the recipes' `required`/`min`, are verified together. T1 already wrote configs/demo/data/esg-2025.csv and esg-2024.csv with every field's key, label and unit in template order and empty values, and documented the fields in configs/demo/README.md. Do not change keys, labels, units or order; only fill the `value` column. Everything is invented: no real company names, no real people, no KPMG text.

Acceptance:

- `grep -q Demo configs/demo/company.md && grep -q 2024 configs/demo/company.md && grep -q 2025 configs/demo/company.md`
- `v=$(python3 -I -c 'import csv; print([r["value"] for r in csv.DictReader(open("configs/demo/data/esg-2025.csv")) if r["key"]=="employees_total"][0])') ... (see plan.json)`
- `python3 -I -c ' ... (see plan.json)`
- `test $(grep -cE '^- '?(missing|material_change|out_of_range)' configs/demo/data/README.md) -eq 4`
- `grep -qx 'materiality_pct: 10' configs/demo/sections/esg-environmental.yaml && grep -qx 'materiality_pct: 10' configs/demo/sections/esg-social.yaml`
- `for f in configs/demo/sections/verbatim/env-policy.txt configs/demo/sections/verbatim/social-policy.txt; do test -s $f && head -1 $f | grep -qv '^#' | ... (see plan.json)`
- `python3 -I -c ' ... (see plan.json)`
- `mkdir -p .scratch/l4-smoke && rm -f .scratch/l4-smoke/* && go build -o .scratch/l4-smoke/mcp ./cmd/workiva-mcp && (NL_DEMO_MODE=true NL_DEMO_PIPELINE= ... (see plan.json)`
- `python3 -I configs/demo/check_no_sandbox_ids.py "$HOME/northern-lights-workspace/versions/demo-l0/docs/demo/evidence/2026-10-19-RB-01/step-3.local.jso ... (see plan.json)`

### T4: Add nl-demo-seed loaders and data tests for consistency, recipes and demo reads

Kind `test`, tier `code`, covers R2, R3, R4, R5, R6, R7, depends on T2, T3.

Owns: `cmd/nl-demo-seed/main.go`, `cmd/nl-demo-seed/data.go`, `cmd/nl-demo-seed/data_test.go`

Create the cmd/nl-demo-seed package with the loaders the seed command needs and the acceptance-scenario tests for the data: scenario 1 (R2, R3, R4), the L4 half of scenario 2 (R5, R6: a strict mirror of the L2 schema; full proof waits for L2) and scenario 3 (R7). The data files exist (T1, T3) and internal/workiva/demo_esg.go serves them (T2); these tests lock them in.

Acceptance:

- `mkdir -p .scratch && go test ./cmd/nl-demo-seed/ -race -count=1 -v > .scratch/t4.log 2>&1; for n in TestDataConsistency TestRecipesMatchSchemaAndMappi ... (see plan.json)`
- `go build -o /dev/null ./cmd/nl-demo-seed`
- `go vet ./cmd/nl-demo-seed/`
- `test -z "$(gofmt -l cmd internal)"`
- `golangci-lint run ./cmd/nl-demo-seed/...`
- `grep -q GetSheetDataTyped cmd/nl-demo-seed/data_test.go && ! grep -qE 'GetSheetData\(' cmd/nl-demo-seed/data_test.go`

### T5: Build the seed plan and dry-run mode of nl-demo-seed, tests first

Kind `feature`, tier `code`, covers R8, depends on T4.

Owns: `cmd/nl-demo-seed/main.go`, `cmd/nl-demo-seed/plan.go`, `cmd/nl-demo-seed/plan_test.go`, `cmd/nl-demo-seed/apply.go`, `cmd/nl-demo-seed/testdata/**`

R8, first half: the seed plan and `--dry-run` (acceptance scenario 4). Write the tests first, watch them fail, then implement. Task T6 implements Apply and the live client; here apply.go only declares the interface and a stub.

Acceptance:

- `mkdir -p .scratch && go test ./cmd/nl-demo-seed/ -race -count=1 -v > .scratch/t5.log 2>&1; for n in TestBuildPlanWritesBothYears TestBuildPlanPercentC ... (see plan.json)`
- `go vet ./cmd/nl-demo-seed/`
- `test -z "$(gofmt -l cmd internal)"`
- `golangci-lint run ./cmd/nl-demo-seed/...`
- `mkdir -p .scratch && env -u NL_WORKIVA_CLIENT_ID -u NL_WORKIVA_CLIENT_SECRET go run ./cmd/nl-demo-seed -dry-run -mappings configs/demo/mappings-sandbo ... (see plan.json)`
- `go run ./cmd/nl-demo-seed -dry-run -reset -mappings configs/demo/mappings-sandbox.example.yaml > .scratch/t5-reset.txt && grep -q '^delete-section "'  ... (see plan.json)`

### T6: Implement live apply with read-back and report section cleanup, tests first

Kind `feature`, tier `code`, covers R8, depends on T5.

Owns: `cmd/nl-demo-seed/apply.go`, `cmd/nl-demo-seed/apply_test.go`, `cmd/nl-demo-seed/workiva.go`, `cmd/nl-demo-seed/workiva_test.go`, `cmd/nl-demo-seed/main.go`

R8, second half: `Apply` and the live Workiva adapter, so a non-dry run writes 2025 values to the sandbox source spreadsheet and 2024 values to the prior-period fields of the report spreadsheet, and `-reset` also clears the 2025 target fields and deletes report document sections whose titles match the recipe titles, touching nothing outside the plan. Write the tests first, watch them fail against the stub from T5, then implement. Do not change plan.go, data.go or their tests.

Acceptance:

- `mkdir -p .scratch && go test ./cmd/nl-demo-seed/ -race -count=1 -v > .scratch/t6.log 2>&1; for n in TestApplyWritesOnePerSheetAndReadsBack TestApplyRe ... (see plan.json)`
- `grep -q '/sections' cmd/nl-demo-seed/workiva.go && grep -q 'MethodDelete' cmd/nl-demo-seed/workiva.go && grep -q GetSheetDataTyped cmd/nl-demo-seed/apply.go`
- `test -z "$(gofmt -l .)" && go vet ./... && golangci-lint run`
- `go test ./... -race`
- `go build ./...`

### T7: Update the Copilot agent instructions, style guide and demo prompts

Kind `docs`, tier `code`, covers R9, R10, depends on T3.

Owns: `docs/demo/agent/INSTRUCTIONS.md`, `docs/demo/agent/NARRATIVE_STYLE.md`, `docs/demo/agent/DEMO_PROMPTS.md`

R9 and R10: the Copilot Studio agent documents match the demo tool names and outputs, and every demo act has exact prompts with what a correct answer contains. Write first versions against the tool names and JSON schemas in internal/mcpserver/tools/ (prepare_section.go, change_set.go, section_text.go, demo.go, plus all.go for the 13 existing tools and export_evidence.go) and the L1, L2, L3 specs in docs/specs/. Data comes from configs/demo/ (company.md, data/esg-2025.csv, data/esg-2024.csv, data/README.md, sections/*.yaml, mappings-sandbox.example.yaml for sheet names and ranges).

Acceptance:

- `python3 -I -c ' ... (see plan.json)`
- `for n in workiva_prepare_section workiva_confirm_change_set workiva_stage_section_text workiva_confirm_section_text workiva_export_evidence; do grep - ... (see plan.json)`
- `for i in 1 2 3 4 5; do grep -qE "^## Act $i" docs/demo/agent/DEMO_PROMPTS.md || exit 1; done && test $(grep -c 'Correct answer contains:' docs/demo/ag ... (see plan.json)`
- `for y in 2025 2024; do v=$(python3 -I -c "import csv; print([r['value'] for r in csv.DictReader(open('configs/demo/data/esg-$y.csv')) if r['key']=='en ... (see plan.json)`
- `! grep -q PENDIENTE docs/demo/agent/NARRATIVE_STYLE.md`
- `grep -q 'RB-03' docs/demo/agent/INSTRUCTIONS.md`
- `python3 -I configs/demo/check_no_sandbox_ids.py "$HOME/northern-lights-workspace/versions/demo-l0/docs/demo/evidence/2026-10-19-RB-01/step-3.local.jso ... (see plan.json)`

### T8: Write the two-page IT and risk brief

Kind `docs`, tier `code`, covers R11, depends on nothing.

Owns: `docs/demo/IT_BRIEF.md`

R11: docs/demo/IT_BRIEF.md, at most two pages (at most 900 words), for the KPMG firm's IT team, in plain words. Reuse content from docs/compliance/ (TECHNICAL_DOCUMENTATION.md, AUDIT.md, TRANSPARENCY.md, AI_ACT.md), docs/demo/DEMO.md ("How it works"), ADR 0001 (no model in the server) and docs/validation/LIVE_VALIDATION_2026-09-15.md (finding NL-LIVE-002). Do not invent controls the code does not have; when unsure, mark the item "open".

Acceptance:

- `test $(wc -w < docs/demo/IT_BRIEF.md) -le 900`
- `for h in 'Data flow' 'Identity' 'What is written and how' 'Audit trail and evidence export' 'Open risks'; do grep -qx "## $h" docs/demo/IT_BRIEF.md || ... (see plan.json)`
- `grep -q 'NL-LIVE-002' docs/demo/IT_BRIEF.md && grep -qi 'per-user' docs/demo/IT_BRIEF.md && grep -qi 'entra' docs/demo/IT_BRIEF.md`
- `awk '/^## Open risks/{f=1;next} /^## /{f=0} f && /^- /{n++; if ($0 !~ /Mitigation: / && $0 !~ /Status: open/) bad++} END{exit (n>=4 && bad==0)?0:1}' d ... (see plan.json)`

### T9: Run the live sandbox seed and record the redacted result in README and STATUS

Kind `other`, tier `code`, covers R8, depends on T6.

Owns: `configs/demo/make_sandbox_mappings.py`, `configs/demo/README.md`, `docs/demo/STATUS.md`, `cmd/nl-demo-seed/plan.go`, `cmd/nl-demo-seed/apply.go`, `cmd/nl-demo-seed/workiva.go`, `cmd/nl-demo-seed/plan_test.go`, `cmd/nl-demo-seed/apply_test.go`

The owner asked for the LIVE seed against the sandbox in this run, after the dry run passes (R8). Write only to the resources in the mapping file and the section recipes. Raw outputs stay in .scratch/; only a redacted summary is committed (configs/demo/README.md and the L4 row of docs/demo/STATUS.md). Do not write to docs/demo/evidence/ (lane L5 owns it).

Acceptance:

- `python3 -I configs/demo/make_sandbox_mappings.py --help > /dev/null`
- `test -f configs/demo/mappings-sandbox.yaml && git check-ignore -q configs/demo/mappings-sandbox.yaml && ! git ls-files --error-unmatch configs/demo/ma ... (see plan.json)`
- `! grep -qE '<id-[0-9]{2}>|<document-id>' configs/demo/mappings-sandbox.yaml`
- `grep -q '^## Seed run 2026-10-19' configs/demo/README.md && grep -q '^## Running the seed' configs/demo/README.md`
- `grep -qE 'read-back: [0-9]+ ok, 0 mismatch' configs/demo/README.md && grep -qE '^read-back: [0-9]+ ok, 0 mismatch' .scratch/l4-seed/seed-reset.txt &&  ... (see plan.json)`
- `! git ls-files -co --exclude-standard docs/demo/evidence | grep -q 'L4-seed'`
- `! grep -qiE 'bearer|client_secret|access_token|[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' .scratch/l4-seed/dry-run.txt ... (see plan.json)`
- `grep -E '^\| L4 Data and agent \|' docs/demo/STATUS.md | grep -q 'claude/demo-l4-data-and-agent' && ! grep -E '^\| L4 Data and agent \|' docs/demo/STA ... (see plan.json)`
- `test $(grep -c '^L4 -> L0' docs/demo/STATUS.md) -ge 2 && grep -q '^L4 -> L2' docs/demo/STATUS.md`
- `go test ./cmd/nl-demo-seed/ -race -count=1`
- `python3 -I configs/demo/check_no_sandbox_ids.py "$HOME/northern-lights-workspace/versions/demo-l0/docs/demo/evidence/2026-10-19-RB-01/step-3.local.jso ... (see plan.json)`

## Checkpoints

- After T1, T2, T8: sandbox layout and live write semantics recorded, demo transport serves CSV data, IT brief written.
- After T4: data, recipes and mappings locked in by tests; scenarios 1 and 3 green offline, scenario 2 partially (until L2).
- After T6: full `go test ./... -race`, lint and build green (in T6 acceptance); dry run proven (scenario 4, T5).
- After T9: live seed done with read-back `0 mismatch`; redacted summary in configs/demo/README.md and STATUS.

## Coverage

- R1: T3
- R2: T1, T3, T4
- R3: T3, T4
- R4: T3, T4
- R5: T3, T4
- R6: T1, T3, T4
- R7: T2, T4
- R8: T5, T6, T9
- R9: T7
- R10: T7
- R11: T8

Longest dependency chain: 6 tasks (T1, T3, T4, T5, T6, T9).

## Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Template value cells are formulas or social fields have no report cells | High | T1 picks literal or empty cells, falls back to Data Collection or per-disclosure sheets, documents fallbacks in configs/demo/README.md |
| Reset deletes an existing KPMG section with a recipe title | High | T1 lists document sections; T3 renames titles on collision; T9 stops before `-reset` if a match exists |
| Real IDs leak into the public repository | High | git-ignored real mapping, placeholder example, ID-free seed output, leak check in T1, T3, T7, T9 acceptance; raw seed output stays in .scratch |
| Live write semantics differ from the fake (null clear, percent cells, value types) | High | T1 live probe on the Testing MCP scratch range records them; T5 and T6 implement the recorded lines; T9 may make a bounded fix |
| Float decoding breaks exact comparisons | Medium | GetSheetDataTyped and DecimalText everywhere; acceptance greps exclude GetSheetData and fmt.Sprint |
| L2 recipe loader differs from its spec | Medium | Strict KnownFields test mirrors the L2 schema (T6); recheck when L2 merges |

## Assumptions

- Owner scope for this run is the L4-owned files in docs/demo/LANES.md plus the L4 row and request lines in docs/demo/STATUS.md. The planner therefore does not edit docs/architecture.md or add an ADR (both shared, not L4-owned); the cross-task decisions are lane-local and are recorded in the shared contract in each task's instructions and in configs/demo/README.md (T1). No decision here changes another lane's interface.
- configs/demo/mappings-sandbox.yaml (real IDs) is git-ignored through configs/demo/.gitignore; the committed configs/demo/mappings-sandbox.example.yaml uses the RB-01 placeholders <id-NN> and <document-id>. R6 is met by the example plus the generator (T9). T9 generates the real file only in its own checkout; the generator refuses to write unless git check-ignore passes, and Tom or the orchestrator runs it in the run checkout after configs/demo/.gitignore is merged (critique c1).
- Mapping files carry an extra top-level document_id key for nl-demo-seed. internal/bootstrap uses yaml.v3 without KnownFields, so the server ignores the key and the same file still works with -mappings.
- Committed section recipes use document_id demo-doc-annual-report so no real document ID is committed. How L2 points recipes at the real sandbox document is an open question and a STATUS request (T9); it does not block L4.
- Section short names are env and soc (src_env_energy_total as in spec scenario 3). Mandatory keys energy_total and employees_total keep scenario 3 and company.md consistent.
- Demo mode uses one flat layout per demo sheet (header row, then CSV rows in A:D, value in column D) so internal/workiva/demo_esg.go serves any CSV without per-field configuration. The write overlay lives in a package-level registry keyed by *DemoTransport because the struct is in L0's demo_client.go; T9 asks L0 for a field so it can be removed (critique c1 minor).
- Read path (critique c1 major 1): every read in tests and Apply uses GetSheetDataTyped (json.Number) and compares exact decimal text via DecimalText/big.Rat, accepting json.Number or string cell values; GetSheetData (float64) and fmt.Sprint comparisons are excluded by acceptance greps.
- Live semantics (critique c1 major 2): T1 probes the scratch range Testing MCP / Sheet1 (write a decimal string, read typed, clear with null, read) and reads the number formats of chosen cells, recording clear-value and percent-cells-store-fractions in configs/demo/README.md; T5 and T6 implement exactly those lines; T9 may make a bounded fix of about 30 lines in cmd/nl-demo-seed or describe a corrective task.
- Evidence (critique c1 major 3, orchestrator clarification): the owner did not ask for docs/demo/evidence/. Raw seed output stays in .scratch/; the redacted summary goes in a 'Seed run 2026-10-19' section of configs/demo/README.md and the L4 STATUS row; the sandbox document's section names are not committed, only whether one matches a recipe title.
- Acceptance scenario 2 needs L2's recipe loader, which has not merged. T4 covers the L4 half with a strict KnownFields test mirroring the L2 schema; the T3 server smoke start is a mapping-file check only and is not evidence for R5. Scenario 2 is partially proven until L2 merges.
- Acceptance scenario 5 (seed --reset then workiva_prepare_section in the sandbox) is deferred until L2 merges, and scenario 6 is Tom's RB-03 run; no task depends on L2 or RB-03. R9 asks for updates after each RB-03 run; T7 writes the first version against the L0 tool stubs and the L1 to L3 specs.
- nl-demo-seed lists and deletes report document sections through workiva.Client.Do in its own adapter (T6), because L3 owns internal/workiva/documents.go and its DocumentWriter has no delete. Paths come from RB-02 (result.md on codex/demo-l3-section-text).
- Live sandbox access in worker worktrees: deployments/.env and the *.local.* ID files are git-ignored and outside the worktrees, so instructions give their paths under $HOME. The leak check (configs/demo/check_no_sandbox_ids.py, T1) reads the real IDs from those files and scans every tracked or unignored file; it runs only on Tom's machine.
- Task sizing (critique c1 major 5): transport tests and code are one task (T2), data plus recipes plus demo mapping are one task (T3) so the four planted findings are verified together, plan and dry run are one task (T5), and the live seed (T9) depends only on T6. Tests are written first inside T2, T5 and T6; T4's tests check data that T1 and T3 already wrote. The under-one-minute target is recorded in the README seed section, not asserted, because elapsed time depends on Workiva.
- Full project lint and race tests run in T6's acceptance (critique c1 minor); T2 runs golangci-lint on internal/workiva; T4 builds with -o /dev/null so no binary lands in the repository.
- Overlapping owns all lie on dependency paths: CSV files T1 -> T3; configs/demo/README.md T1 -> T9; cmd/nl-demo-seed/main.go T4 -> T5 -> T6; apply.go T5 -> T6 -> T9; plan.go and plan_test.go T5 -> T9; apply_test.go and workiva.go T6 -> T9. T9 may edit the two test files only for expectations tied to the README convention lines, in the same commit as the README correction (critique c2).
- Both material_change plants are on non-% fields so a percent-as-fraction conversion cannot hide them (critique c2). T9's deliberate stop when a recipe-title section already exists is a block_question to the owner, not a failing acceptance. T9 checks .scratch outputs for real IDs with a direct grep, because check_no_sandbox_ids.py scans only unignored files.
- c2 fixes applied without a third critique (plan_critic_rounds=2).

## Open questions

- How should lane L2 load the real sandbox report document ID for live runs without committing it (committed recipes carry document_id demo-doc-annual-report)? Options: NL_DEMO_SECTIONS_DIR pointing to a git-ignored copy of configs/demo/sections, or an environment override in L2. Recorded as an L4 -> L2 request in STATUS.md; not blocking L4.
- Acceptance scenario 5 (nl-demo-seed --reset followed by workiva_prepare_section esg-environmental in the sandbox) waits for L2 to merge; scenario 6 is Tom's RB-03 run. Both stay open after this run.
- If the sandbox report spreadsheet has no cells for some social fields, T1 places them in an empty block of CY/PY Data Output without labels visible in Workiva. Tom may want to type those labels or accept it for the demo.
