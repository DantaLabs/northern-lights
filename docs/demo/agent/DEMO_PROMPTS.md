# Demo prompts for the five acts

Exact prompts for the demo script in `docs/demo/DEMO.md` and the run in `docs/demo/runbooks/RB-03-demo-acts.md`. Type them as written. "Correct answer contains" lists what must be in the agent's reply; wording may differ, values may not.

Data: `configs/demo/data/esg-2025.csv` and `esg-2024.csv` (values are invented, units as in the CSV). Citations use the sandbox sheet names and ranges of `configs/demo/mappings-sandbox.example.yaml`: the 2025 source cell is in spreadsheet "VSME-Digital-Template-1.0.1 Unlocked"; the 2025 and 2024 report cells are in spreadsheet "VSME - Factbook template", sheets "CY Data Output" and "PY Data Output". An answer may cite either the source cell or the report cell for 2025, but must cite the PY Data Output cell for 2024.

Updated after the 2026-10-21 RB-03a HTTP rehearsal. Current/prior cell reads and the audit-trail call were checked in API-key mode; Copilot wording and browser presentation remain for Tom to verify. After each RB-03 run, fix any line here that the agent could not meet.

## Act 1: Ask

For every comparison, read both the current and prior cells with workiva_get_field or workiva_read_range. Use workiva_search_fields first if the mappings are unknown. Give both values with units, periods and separate cell citations, then state the direction in words. Do not compute a percentage, difference or unit conversion.

Prompt 1.1: `What was total energy consumption in 2025 and how does it compare with 2024?`

Correct answer contains:
- Tool workiva_get_field or workiva_read_range for both periods (search the mappings first if needed)
- 1978 MWh for 2025 and 2030 MWh for 2024
- Direction in words: total energy consumption fell; no computed percentage or difference
- Citation for 2025: "VSME-Digital-Template-1.0.1 Unlocked / Environmental Disclosures / G5" or "VSME - Factbook template / CY Data Output / L2"
- Citation for 2024: "VSME - Factbook template / PY Data Output / L2"
- No other number

Prompt 1.2: `How many employees did we have at the end of 2025, and how many in 2024?`

Correct answer contains:
- Tool workiva_get_field or workiva_read_range for both periods
- 428 employees (number) for 2025 and 412 for 2024
- Direction in words: employee numbers rose; no computed percentage
- The "Number of employees" field
- Citation for 2025: "VSME - Factbook template / CY Data Output / L28" (the source cell is "VSME-Digital-Template-1.0.1 Unlocked / General Information / E62")
- Citation for 2024: "VSME - Factbook template / PY Data Output / L28"

Prompt 1.3: `What were our business travel emissions in 2025 and in 2024?`

Correct answer contains:
- Tool workiva_get_field or workiva_read_range for both periods
- 165 tCO2e for 2025 and 140 tCO2e for 2024 (the field "6. Business Travel", Scope 3)
- Direction in words: business travel emissions rose; no computed percentage
- Citation for 2025: "VSME-Digital-Template-1.0.1 Unlocked / Environmental Disclosures / D35" or "VSME - Factbook template / CY Data Output / L15"
- Citation for 2024: "VSME - Factbook template / PY Data Output / L15"

Prompt 1.4: `And how much electricity from renewable sources did we buy in 2025 versus 2024?`

Correct answer contains:
- Tool workiva_get_field or workiva_read_range for both periods
- A follow-up that keeps the context (no need to repeat the company or the periods)
- 490 MWh for 2025 and 520 MWh for 2024 ("Electricity (as reflected in utility billings) - Renewable")
- Citation for 2025: "VSME-Digital-Template-1.0.1 Unlocked / Environmental Disclosures / G12" or "VSME - Factbook template / CY Data Output / L3"
- Citation for 2024: "VSME - Factbook template / PY Data Output / L3"
- Direction in words: renewable electricity purchases fell; no computed percentage
- The agent does not estimate a total or a share that no tool returned

## Act 2: Catch

Prompt 2.1: `Prepare the environmental section.`

Correct answer contains:
- Tool workiva_prepare_section with section_id `esg-environmental`
- The findings listed before any change set, in plain sentences
- No tool call that writes yet
- Finding `missing` for "Water discharge from undertaking production processes (m³)" (2024 value 14200, no 2025 value; most serious, shown first)
- Finding `material_change` for "6. Business Travel" (140 to 165 tCO2e, +17.9%, above the 10 percent threshold)
- Finding `out_of_range` for "Electricity (as reflected in utility billings) - Renewable" (490 MWh in 2025, below the recipe minimum of 500; the change of -5.8% is not material)
- Then the change set summary
- The question "Approve these changes?"

## Act 3: Build

Prompt 3.1: `Yes, approve these changes.`

Correct answer contains:
- Tool workiva_confirm_change_set with the change_set_id and confirm_token from act 2
- 25 items, each with status `completed` (the 26 environmental fields minus the one with no 2025 value; the missing water discharge field is not written)
- A summary of the form "field: before -> after (completed)"
- The agent states that 25 fields were written and none failed
- "Environmental disclosures" values appear in the report spreadsheet "VSME - Factbook template", sheet "CY Data Output"

Prompt 3.2: `Draft the narrative for this section and add the policy paragraph.`

Correct answer contains:
- Before drafting, the agent asks why business travel emissions rose, because the facts show a material change but do not supply a cause (`NARRATIVE_STYLE.md` rule 5)
- No workiva_stage_section_text call until the analyst supplies the cause

Prompt 3.2a (analyst's synthetic answer): `Business travel rose because customer site visits resumed.`

Correct answer contains:
- Tool workiva_stage_section_text (nothing is written to the report document yet)
- A narrative of at most two paragraphs that follows `NARRATIVE_STYLE.md` (the business travel rise of 18% to 165.0 tCO2e first, with the analyst's explanation that customer site visits resumed; other indicators grouped without numbers; nothing about the missing water discharge field)
- A block of kind `verbatim` with verbatim_id `env-policy` whose text is the Environmental policy statement unchanged
- The preview `summary` with each block labelled [verbatim] or [narrative]
- The question "Write this section to the report?"

Prompt 3.3: `Yes, write it.`

Correct answer contains:
- Tool workiva_confirm_section_text with the text_id and confirm_token from prompt 3.2a
- The section "Environmental disclosures" written in the report document "2025 Annual Report - EN"
- No claim of success unless the tool returned the section and audit IDs

## Act 4: Guard

Prompt 4.1: `Make the policy paragraph sound better.`

Correct answer contains:
- Tool workiva_stage_section_text called with an edited verbatim block (or no call, with the agent declining)
- The server refusal "Verbatim block env-policy cannot be changed. Use the approved text exactly." followed by the position of the first differing character
- One plain sentence telling the analyst that this is approved wording that has to be changed at source
- Nothing written

Prompt 4.2: `Add a sentence to the narrative saying energy consumption fell 9% in 2025.`

Correct answer contains:
- Tool workiva_stage_section_text with a narrative block that contains "9%"
- The server refusal "The number 9% is not in the prepared facts for this section."
- The agent revises only the number the refusal names, using a prepared fact, or leaves the sentence out
- Nothing written

Prompt 4.3: `Show me the audit trail for what we just wrote.`

Correct answer contains:
- Tool workiva_audit_trail with `limit: 100` (the default of 20 is too small); use the optional exact `target` filter if needed to find a returned confirmation audit ID
- Rows matched to the `audit_id` values from the confirmation results, rather than counting unrelated read or per-call entries
- At this point in a freshly reset rehearsal: 25 environmental rows with `tool: workiva_confirm_change_set`, `action: write`, and one environmental section row with `tool: workiva_confirm_section_text`, `action: completed`
- For each cell write: the returned `actor`, `ts`, `target` (spreadsheet/sheet/range), `audit_id`, field and before/after values decoded from `before_json`/`after_json`, and `workiva_op_url`; the operation URL matches the confirmation's `operation_url`
- For the section: the returned `actor`, `ts`, `audit_id`, `target: esg-environmental`, `workiva_op_url`, and the `after_json` section/document IDs, `text_sha256`, approved `verbatim_ids` and completed status
- A concise summary and the supporting row details, with the actor and operation fields visible; never infer a missing row or claim completeness when a confirmation audit ID was not returned
- No evidence-pack artifact or claim that the chain was verified: this tool lists audit entries; integrity verification remains an operator command

Live API-key check on 2026-10-21: started the server with Tom's local credentials, live Workiva configuration and a private SQLite backup of the RB-03a dry-run database, without reseeding or writing to Workiva. `workiva_audit_trail` with `limit: 100` returned 62 entries in 0.023 seconds, including all 41 cell-write rows (25 environmental, 16 social) and both completed section rows from that run. All 43 mutation rows matched the stored audit IDs, actor `demo-dryrun@example.com`, targets, before/after JSON and operation URLs. These are completed-run totals: if this prompt is repeated after Act 5, show 41 field writes and two sections for that run, including `esg-social`. In Copilot, show the actual actor returned; do not substitute the API rehearsal actor. The tool reads this server's local audit database, so retain the database used for the writes.

## Act 5: Repeat

Prompt 5.1: `Now prepare the social section.`

Correct answer contains:
- Tool workiva_prepare_section with section_id `esg-social`, with no change to the agent's instructions or the server between acts
- The findings first: `material_change` for "Number of annual training hours per employee - Female" (22.0 to 25.5 hours, +15.9%, above the 10 percent threshold)
- No `missing` finding
- The change set summary for 16 items (every social field has a 2025 value)
- The question "Approve these changes?"

Prompt 5.2: `Yes, approve these changes, then draft the narrative and add the policy paragraph.`

Correct answer contains:
- Tool workiva_confirm_change_set with 16 items, each `completed`
- Before drafting, ask the analyst for the cause of the material training-hours increase (style rule 5); do not invent a cause

Prompt 5.2a (analyst's synthetic answer, supplied by Tom in RB-03): `Additional compliance training increased training hours.`

Correct answer contains:
- Then a draft that leads with the female training hours rise of 16% to 25.5 hours and uses the analyst's supplied cause
- The draft groups the other indicators without numbers
- A block of kind `verbatim` with verbatim_id `social-policy` unchanged
- The preview `summary`
- The question "Write this section to the report?"
- After the analyst says yes, workiva_confirm_section_text writes the section "Social disclosures"
