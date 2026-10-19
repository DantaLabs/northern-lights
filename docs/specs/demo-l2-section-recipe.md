# Demo L2: section recipe

## Goal

Prepare one report section with one tool call: read the source and prior-period values defined in a section recipe, compute narrative facts, detect findings, stage a change set for the target fields, and return everything Copilot needs to show the analyst and to draft the narrative.

## Non-goals

- Writing anything to Workiva. Writes happen in L1 (`Confirm`) and L3.
- Drafting text. Copilot drafts; this lane only computes the numbers it may use.
- Recipes with formulas, cross-section dependencies or more than one target spreadsheet.

## Users and scenarios

- The analyst says "prepare the environmental section". Copilot calls `workiva_prepare_section` with `esg-environmental`, lists the findings, shows the change set summary and asks for approval.
- The analyst says "now the social section". Same tool, `esg-social`, different YAML, no code change.

## Requirements

R1: At startup with the flag on, the server loads every `*.yaml` file in the directory named by `NL_DEMO_SECTIONS_DIR` (default `configs/demo/sections`) and fails startup with the file name and reason if a recipe is invalid.
R2: A recipe has `id`, `title`, `period`, `prior_period`, `document_id`, `materiality_pct`, a list of `fields` and a list of `verbatim` blocks, in the schema shown below. Unknown keys are rejected.
R3: Every field names a `source`, `target` and optional `prior` mapped field; the recipe is rejected at load if a named field is not in the mapping store.
R4: `Prepare` reads source and prior values through the provider router without the cache.
R5: For each field with a numeric current value, `Prepare` computes `Change` and `ChangePct` with exact decimal arithmetic (no float64), `Direction` (`up`, `down`, `flat`, or `new` when the prior value is missing or zero) and `Material` (absolute `ChangePct` at or above `materiality_pct`).
R6: `Display` holds every approved text form of the fact: the current and prior values in `display_unit` with the field's `decimals`, the absolute change with unit, and the percentage change rounded to 0 and to 1 decimal place, with a % sign. For fields whose unit is `%`, the change is also given in percentage points (for example "8 percentage points"), and `Material` uses the percentage-point change.
R7: `Prepare` returns findings of kind `missing` (required field empty), `out_of_range` (outside `min` or `max`), `material_change` and `sign_change`, each with the field label and a one-sentence message.
R8: `Prepare` stages a change set through `changeset.Service.Stage` with one item per field that has a source value, writing the source value to the target field, with `Source` set to the source field name.
R9: `Prepare` loads verbatim block text from the files named in the recipe, byte for byte, and returns it in `Verbatim`.
R10: `Prepare` stores the facts and verbatim blocks per actor and section through `recipe.FactStore`, replacing the previous set, so L3 can check drafted text against them.
R11: `workiva_prepare_section` takes `section_id`; an unknown ID returns an error that lists the available section IDs and titles.
R12: The tool output contains `Prepared`, the change set `summary` from L1, and a `next_step` string telling Copilot to show findings first, then the summary, ask for approval, and call `workiva_confirm_change_set`.
R13: `Sections()` returns ID, title, period, prior period and document ID for every loaded recipe, sorted by ID.

## Interfaces and data

Implements `recipe.Service` and `recipe.FactStore` from `internal/recipe/types.go` (created by L0). FactStore persists in the existing SQLite database in tables prefixed `recipe_`.

Recipe schema (content written by L4 in `configs/demo/sections/`):

```yaml
id: esg-environmental
title: Environmental disclosures
period: "2025"
prior_period: "2024"
document_id: "<Workiva document ID>"
materiality_pct: 10
fields:
  - key: energy_total
    label: Total energy consumption
    unit: MWh
    display_unit: GWh        # optional; converts by powers of 1,000 between Wh, kWh, MWh, GWh, and t, kt for tonnes
    decimals: 2
    source: src_env_energy_total
    target: rpt_env_energy_total_2025
    prior: rpt_env_energy_total_2024
    required: true
    min: 0
verbatim:
  - id: env-policy
    title: Environmental policy statement
    file: verbatim/env-policy.txt   # relative to the sections directory
```

## Constraints

- Decimal arithmetic with `math/big` or the existing decimal helpers in `internal/assurance/decimal.go` (read only; do not edit assurance files).
- Tests use a fake `changeset.Service` until L1 merges.
- Section YAML used in tests lives in `internal/recipe/testdata/`.

## Acceptance scenarios

1. (R1, R2, R3) An invalid recipe (unknown key, missing `id`, unknown mapped field) stops startup with the file name and reason; a valid one loads.
2. (R4, R5) Current 1,240,000 kWh against prior 1,350,000 kWh gives change -110,000, change percentage -8.148148..., direction `down`, `Material` false at a 10% threshold.
3. (R5) A prior of zero or missing gives direction `new` and empty `ChangePct`.
4. (R6) The fact above, with `display_unit: GWh` and `decimals: 2`, has display forms "1.24 GWh", "1.35 GWh", "-0.11 GWh", "-8%" and "-8.1%".
5. (R7) A required empty field, a value below `min`, a 25% increase and a positive-to-negative change each produce one finding of the right kind.
6. (R8) Five fields with values and one empty produce a staged change set of five items with the right targets and sources.
7. (R9, R10) Verbatim text is returned byte for byte (including line breaks and non-ASCII characters), and `LatestFacts` for the same actor and section returns the stored facts and blocks; another actor gets nothing.
8. (R11, R12, R13) An unknown section ID lists the available IDs from `Sections()`; a known one returns findings, facts, verbatim blocks, summary and `next_step`.
9. (R4, R8) In demo mode with the L4 fixture data, preparing `esg-environmental` returns the planted environmental findings listed in `configs/demo/data/README.md` and one change set item per environmental field that has a 2025 value.

## Open questions

None

## Approval

Approved: pending Tom
