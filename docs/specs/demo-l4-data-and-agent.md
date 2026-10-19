# Demo L4: data, section recipes and the Copilot agent

## Goal

Provide everything the demo runs on besides server code: a fictional company with two years of synthetic ESG data, the environmental and social section recipes, verbatim policy text, mapping files for demo mode and for the sandbox, a seed command that loads the sandbox, and the Copilot Studio agent instructions with the consultant narrative style.

## Non-goals

- Copying KPMG material into the repository. The KPMG templates in the sandbox give the structure and labels; every number and every sentence of policy text is invented.
- Sections beyond environmental and social.
- Changes to server packages other than `internal/workiva/demo_esg.go`.

## Users and scenarios

- Tom runs `nl-demo-seed --reset` before each rehearsal and the sandbox is back to the starting state in under a minute.
- An agent working on L2 or L3 runs the server in demo mode with `-mappings configs/demo/mappings-demo.yaml` and gets realistic data offline.
- The KPMG audience reads a narrative that sounds like an advisory report, not a data dump.

## Requirements

R1: `configs/demo/company.md` describes one fictional company: name containing "Demo", sector, countries, sites, headcount, reporting years 2024 and 2025.
R2: `configs/demo/data/esg-2024.csv` and `esg-2025.csv` hold at least 20 environmental and 12 social fields with columns `key,label,unit,value`, using the labels and order of the KPMG ESG template in the sandbox (names from RB-01).
R3: The data is internally consistent: every total equals the sum of its parts, every share is between 0 and 100%, and year-on-year changes stay within plus or minus 30%, except the planted findings in R4.
R4: The 2025 data plants exactly these findings, listed in `configs/demo/data/README.md`: one required environmental field empty, one environmental change above 10%, one social change above 10%, and one environmental value below its `min`.
R5: `configs/demo/sections/esg-environmental.yaml` and `esg-social.yaml` follow the L2 recipe schema, with `materiality_pct: 10`, and each references one verbatim block in `configs/demo/sections/verbatim/`. The verbatim text is invented for the demo and starts with no heading.
R6: `configs/demo/mappings-demo.yaml` maps every field to `demo-esg-` spreadsheet IDs; `configs/demo/mappings-sandbox.yaml` maps the same field names to the sandbox IDs and ranges recorded in RB-01.
R7: `internal/workiva/demo_esg.go` serves the CSV data for `demo-esg-` spreadsheets in demo mode, reading the directory named by `NL_DEMO_ESG_DATA` (default `configs/demo/data`).
R8: `cmd/nl-demo-seed` writes 2025 values to the sandbox source spreadsheet and 2024 values to the prior-period fields of the report spreadsheet, using the existing Workiva client and `configs/demo/mappings-sandbox.yaml`. `--dry-run` prints every planned write; `--reset` also clears the 2025 target fields and deletes report document sections whose titles match the recipe titles. It writes only to spreadsheets and documents named in the mapping file and recipes.
R9: `docs/demo/agent/INSTRUCTIONS.md` and `docs/demo/agent/NARRATIVE_STYLE.md` match the final tool names and outputs; update them after each RB-03 run.
R10: `docs/demo/agent/DEMO_PROMPTS.md` lists the exact analyst prompts for each act in `docs/demo/DEMO.md` and what a correct answer contains.
R11: `docs/demo/IT_BRIEF.md` is at most two pages: data flow, identity (delegated Entra), what is written and how (preview, approval, read-back), audit trail and evidence export, and open risks in plain words, including the Workiva grant scope finding NL-LIVE-002 and the absence of per-user Workiva permissions. It reuses `docs/compliance/` content.

## Interfaces and data

CSV columns: `key,label,unit,value`; `value` is empty for a missing value, otherwise a plain decimal with a dot separator. Field `key` values are the `key` values in the section YAML. Mapped field names follow `src_<section>_<key>` for the source spreadsheet and `rpt_<section>_<key>_<year>` for the report spreadsheet.

## Constraints

- No real company names, no real people, no KPMG text.
- The seed command never writes outside the resources in the mapping file and recipes, and prints no secrets.
- `demo_esg.go` must not change the existing VSME demo fixture routes.

## Acceptance scenarios

1. (R2, R3, R4) A Go test in `cmd/nl-demo-seed` loads both CSV files and passes the consistency rules, finding exactly the four planted exceptions.
2. (R5, R6) Starting the server in demo mode with the flag on and `-mappings configs/demo/mappings-demo.yaml` loads both recipes without errors.
3. (R7) In demo mode, reading `src_env_energy_total` returns the 2025 CSV value.
4. (R8) `nl-demo-seed --dry-run` against the sandbox mapping prints one planned write per field and per year and sends nothing.
5. (R8) `nl-demo-seed --reset` followed by `workiva_prepare_section esg-environmental` in the sandbox returns the planted environmental findings.
6. (R9, R10) RB-03 act 1 to 5 prompts in `DEMO_PROMPTS.md` produce the answers described there.
7. (R11) Tom reads `IT_BRIEF.md` in under ten minutes and every risk names its mitigation or says "open".

## Open questions

None

## Approval

Approved: pending Tom
