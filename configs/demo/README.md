# Demo L4 configuration

Data and mappings for the KPMG demo (lane L4). The spec is `docs/specs/demo-l4-data-and-agent.md`.

## Files

- `data/esg-2025.csv`, `data/esg-2024.csv`: the fields of the ESG data, header `key,label,unit,value`. The `value` column is filled by a later task.
- `mappings-sandbox.example.yaml`: sandbox mapping with placeholder IDs (committed). `mappings-sandbox.yaml` is the real one and is git-ignored (`.gitignore`), because the repository is public.
- `check_no_sandbox_ids.py`: `python3 -I configs/demo/check_no_sandbox_ids.py LOCAL_ID_FILE...` exits 1 and prints only file paths if a repository file contains a real sandbox ID.
- Later tasks add `mappings-demo.yaml`, `company.md` and `sections/`.

## Contract

- Sections: environmental (`env`, section recipe `esg-environmental`, "Environmental disclosures") and social (`soc`, `esg-social`, "Social disclosures"). Reporting period 2025, prior period 2024.
- Field keys are lowercase snake_case and unique across both sections; `energy_total` and `employees_total` are mandatory. CSV rows: environmental fields first, then social, in template order.
- Mapped field names: `src_<env|soc>_<key>` (KPMG ESG template, 2025), `rpt_<env|soc>_<key>_2025` (current-year target) and `rpt_<env|soc>_<key>_2024` (prior period), both in the ESG report spreadsheet.
- Read cells with `GetSheetDataTyped` (numbers stay `json.Number`), compare values as exact decimals, treat absent, nil and "" as empty.
- Demo mode: spreadsheet `demo-esg-source` has sheet `demo-esg-source-2025` ("ESG data 2025"); `demo-esg-report` has `demo-esg-report-cy` ("CY Data Output") and `demo-esg-report-py` ("PY Data Output"). Row 1 is the header key, label, unit, value; row r is CSV data row r-1; a field range is `D<r>`. Document ID `demo-doc-annual-report`.

## Sandbox cells

`src_` cells are empty literal input cells (merged ranges are given by their top-left cell). The totals on the Social Disclosures sheet are formulas linked from General Information, so `employees_total` uses the input cell of that sheet. Other totals the template computes (fuels, scope totals, waste totals, `K` columns) are formulas and are not mapped.

The ESG report spreadsheet has no free cells for these fields: CY Data Output and PY Data Output hold formulas that link only B1 rows (A1:G19), and each per-disclosure sheet holds one year of filled literal values. So every `rpt_` field uses an empty block to the right of that used range, columns I to L of CY Data Output (2025 and the current-year target) and PY Data Output (2024), format General. Field number n (1 is `energy_total`) has range `L<n+1>`. Columns I to K and row 1 are reserved for key, label and unit and are not mapped.

Waste has no literal totals, so two waste types (rows 1 and 2 of the waste table) stand in. The type and unit cells of those rows are not mapped: the seed task must fill them itself (see the template), only the recycled and disposed amounts are mapped. No chosen cell has a percent format, so every `field_type` is `number`.

| key | template sheet | range | format |
|---|---|---|---|
| env_energy_total | Environmental Disclosures | G5 | number, 2 dp |
| env_electricity_renewable | Environmental Disclosures | G12 | number, 2 dp |
| env_electricity_non_renewable | Environmental Disclosures | J12 | number, 2 dp |
| env_self_generated_electricity_renewable | Environmental Disclosures | G13 | number, 2 dp |
| env_self_generated_electricity_non_renewable | Environmental Disclosures | J13 | number, 2 dp |
| env_ghg_scope1 | Environmental Disclosures | D22 | number, 2 dp |
| env_ghg_scope2_location | Environmental Disclosures | D23 | number, 2 dp |
| env_ghg_scope2_market | Environmental Disclosures | D24 | number, 2 dp |
| env_ghg_scope3_purchased_goods | Environmental Disclosures | D30 | number, 2 dp |
| env_ghg_scope3_capital_goods | Environmental Disclosures | D31 | number, 2 dp |
| env_ghg_scope3_fuel_energy | Environmental Disclosures | D32 | number, 2 dp |
| env_ghg_scope3_upstream_transport | Environmental Disclosures | D33 | number, 2 dp |
| env_ghg_scope3_waste | Environmental Disclosures | D34 | number, 2 dp |
| env_ghg_scope3_business_travel | Environmental Disclosures | D35 | number, 2 dp |
| env_ghg_scope3_commuting | Environmental Disclosures | D36 | number, 2 dp |
| env_land_sealed_area | Environmental Disclosures | D161 | number, 2 dp |
| env_land_nature_onsite | Environmental Disclosures | D162 | number, 2 dp |
| env_land_nature_offsite | Environmental Disclosures | D163 | number, 2 dp |
| env_land_use_total | Environmental Disclosures | D164 | number, 2 dp |
| env_water_withdrawn_total | Environmental Disclosures | D169 | number, 2 dp |
| env_water_withdrawn_high_stress | Environmental Disclosures | D170 | number, 2 dp |
| env_water_discharge_production | Environmental Disclosures | D176 | number, 2 dp |
| env_waste_recycled_type1 | Environmental Disclosures | J189 | number, 2 dp |
| env_waste_disposed_type1 | Environmental Disclosures | K189 | number, 2 dp |
| env_waste_recycled_type2 | Environmental Disclosures | J190 | number, 2 dp |
| env_waste_disposed_type2 | Environmental Disclosures | K190 | number, 2 dp |
| soc_employees_total | General Information | E62 | number, 2 dp |
| soc_employees_permanent | Social Disclosures | D10 | number, 2 dp |
| soc_employees_male | Social Disclosures | D18 | number, 2 dp |
| soc_employees_female | Social Disclosures | D19 | number, 2 dp |
| soc_employees_other | Social Disclosures | D20 | number, 2 dp |
| soc_employees_not_reported | Social Disclosures | D21 | number, 2 dp |
| soc_employees_left | Social Disclosures | D43 | number, 2 dp |
| soc_employees_start | Social Disclosures | D44 | number, 2 dp |
| soc_employees_end | Social Disclosures | D45 | number, 2 dp |
| soc_accidents_recordable | Social Disclosures | D49 | number, 2 dp |
| soc_fatalities_work_related | Social Disclosures | D53 | number, 2 dp |
| soc_employees_collective_bargaining | Social Disclosures | D60 | number, 2 dp |
| soc_training_hours_male | Social Disclosures | D66 | number, 2 dp |
| soc_training_hours_female | Social Disclosures | D67 | number, 2 dp |
| soc_managers_male | Social Disclosures | D74 | number, 2 dp |
| soc_managers_female | Social Disclosures | D75 | number, 2 dp |

The report document already has a section named `Environmental disclosures` or `Social disclosures`: no (it has "2. Environmental information" and "3. Social information").

## Live write semantics

Probed on the scratch spreadsheet `Testing MCP` with `WriteCells`, then cleared.

- A decimal string `"46.5"` reads back as a number: `json.Number("46.5")`, not text.
- Writing JSON null is rejected (HTTP 400); writing `""` clears the cell and it reads back as `""`.
- Percent cells store fractions: the template percent formulas are plain ratios (for example turnover is left divided by the average headcount), so 46% is raw 0.46. No literal percent cell exists to confirm the display.

clear-value: empty-string
percent-cells-store-fractions: yes

Verified in the live seed run of 2026-10-19 (see below): writes into the empty block of the report sheets and into the template cells, and clearing with an empty string, all read back as expected (126 cells, 0 mismatch). No percent cell is mapped, so `percent-cells-store-fractions` is still unconfirmed on a literal percent cell.

## Generate the real mapping file

`make_sandbox_mappings.py` maps each `<id-NN>` in the example to the real ID at the same position in `step-3.json` (placeholders, in the RB-01 evidence folder) and in `step-3.local.json` (real IDs, on Tom's machine), and takes `<document-id>` from the document list in `step-2.local.json` of the RB-02 folder. It refuses to write unless the output path is git-ignored and prints only counts.

## Running the seed

```
python3 -I configs/demo/make_sandbox_mappings.py \
  --example configs/demo/mappings-sandbox.example.yaml \
  --placeholders docs/demo/evidence/2026-10-19-RB-01/step-3.json \
  --real <RB-01 step-3.local.json> --document <RB-02 step-2.local.json> \
  --out configs/demo/mappings-sandbox.yaml
python3 -I configs/demo/check_no_sandbox_ids.py <RB-01 step-3.local.json> <RB-02 step-2.local.json>
go run ./cmd/nl-demo-seed -dry-run -reset -mappings configs/demo/mappings-sandbox.yaml
# live run, with NL_WORKIVA_CLIENT_ID, NL_WORKIVA_CLIENT_SECRET and NL_REGION loaded in a subshell:
go run ./cmd/nl-demo-seed -reset -mappings configs/demo/mappings-sandbox.yaml
```

The dry run needs no network or credentials. A live run ends with `read-back: N ok, 0 mismatch` and `done: elapsed ...`.

## Seed run 2026-10-19

- Base commit d7b4612 (branch `task/20261019-1501-T9-a1`).
- Commands: the four above; the dry run printed no IDs.
- Planned and applied with `-reset`: 84 writes, 42 clears, 2 section deletes (the two deletes found nothing to delete).
- Before the live run the report document's sections were listed read-only: does any match a recipe title? no.
- `read-back: 126 ok, 0 mismatch`
- `done: elapsed 99.7s` (above the one-minute target; the rate limiter paces the calls).
- Bounded fixes to the seed code: none.
- Deferred: acceptance scenario 5 (needs lane L2's `workiva_prepare_section`) and scenario 6 (Tom's RB-03).
