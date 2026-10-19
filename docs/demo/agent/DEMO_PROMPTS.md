# Demo prompts for the five acts

Exact prompts for the demo script in `docs/demo/DEMO.md` and the run in `docs/demo/runbooks/RB-03-demo-acts.md`. Type them as written. "Correct answer contains" lists what must be in the agent's reply; wording may differ, values may not.

Data: `configs/demo/data/esg-2025.csv` and `esg-2024.csv` (values are invented, units as in the CSV). Citations use the sandbox sheet names and ranges of `configs/demo/mappings-sandbox.example.yaml`: the 2025 source cell is in spreadsheet "VSME-Digital-Template-1.0.1 Unlocked"; the 2025 and 2024 report cells are in spreadsheet "VSME - Factbook template", sheets "CY Data Output" and "PY Data Output". An answer may cite either the source cell or the report cell for 2025, but must cite the PY Data Output cell for 2024.

Last checked against tool schemas on 2026-10-19, not yet against RB-03. After each RB-03 run, fix any line here that the agent could not meet.

## Act 1: Ask

Prompt 1.1: `What was total energy consumption in 2025 and how does it compare with 2024?`

Correct answer contains: tools workiva_search_fields or workiva_get_field, then workiva_compare_periods; 1978 MWh for 2025 and 2030 MWh for 2024 (about 1.98 GWh and 2.03 GWh); a small decrease (about 2.6%, 52 MWh) that the agent does not call material (below the 10 percent threshold); citations "VSME-Digital-Template-1.0.1 Unlocked / Environmental Disclosures / G5" or "VSME - Factbook template / CY Data Output / L2" for 2025, and "VSME - Factbook template / PY Data Output / L2" for 2024. No other number.

Prompt 1.2: `How many employees did we have at the end of 2025, and how many in 2024?`

Correct answer contains: tool workiva_get_field or workiva_compare_periods; 428 employees (number) for 2025 and 412 for 2024; the "Number of employees" field; citations "VSME - Factbook template / CY Data Output / L28" and "VSME - Factbook template / PY Data Output / L28" (the 2025 source cell is "VSME-Digital-Template-1.0.1 Unlocked / General Information / E62").

Prompt 1.3: `What were our business travel emissions in 2025 and in 2024?`

Correct answer contains: tool workiva_get_field or workiva_compare_periods; 165 tCO2e for 2025 and 140 tCO2e for 2024 (the field "6. Business Travel", Scope 3); citations "VSME-Digital-Template-1.0.1 Unlocked / Environmental Disclosures / D35" or "VSME - Factbook template / CY Data Output / L15", and "VSME - Factbook template / PY Data Output / L15".

Prompt 1.4: `And how much electricity from renewable sources did we buy in 2025 versus 2024?`

Correct answer contains: a follow-up that keeps the context (no need to repeat the company or the periods); 490 MWh for 2025 and 520 MWh for 2024 ("Electricity (as reflected in utility billings) - Renewable"); citations "VSME-Digital-Template-1.0.1 Unlocked / Environmental Disclosures / G12" or "VSME - Factbook template / CY Data Output / L3", and "VSME - Factbook template / PY Data Output / L3". The agent does not estimate a total or a share that no tool returned.

## Act 2: Catch

Prompt 2.1: `Prepare the environmental section.`

Correct answer contains: tool workiva_prepare_section with section_id `esg-environmental`; the findings listed before any change set, in plain sentences, and no tool call that writes yet. The four planted findings: `missing` for "Water discharge from undertaking production processes (m³)" (2024 value 14200, no 2025 value; most serious, shown first); `material_change` for "6. Business Travel" (140 to 165 tCO2e, +17.9%, above the 10 percent threshold); `out_of_range` for "Electricity (as reflected in utility billings) - Renewable" (490 MWh in 2025, below the recipe minimum of 500; the change of -5.8% is not material). Then the change set summary and the question "Approve these changes?".

## Act 3: Build

Prompt 3.1: `Yes, approve these changes.`

Correct answer contains: tool workiva_confirm_change_set with the change_set_id and confirm_token from act 2; 25 items, each with status `completed` (the 26 environmental fields minus the one with no 2025 value; the missing water discharge field is not written); a summary of the form "field: before -> after (completed)"; the agent states that 25 fields were written and none failed. "Environmental disclosures" values appear in the report spreadsheet "VSME - Factbook template", sheet "CY Data Output".

Prompt 3.2: `Draft the narrative for this section and add the policy paragraph.`

Correct answer contains: tool workiva_stage_section_text (nothing is written to the report document yet); a narrative of at most two paragraphs that follows `NARRATIVE_STYLE.md` (the business travel rise of 18% to 165.0 tCO2e first; other indicators grouped without numbers; nothing about the missing water discharge field); a block of kind `verbatim` with verbatim_id `env-policy` whose text is the Environmental policy statement unchanged; the preview `summary` with each block labelled [verbatim] or [narrative]; the question "Write this section to the report?".

Prompt 3.3: `Yes, write it.`

Correct answer contains: tool workiva_confirm_section_text with the text_id and confirm_token from prompt 3.2; the section "Environmental disclosures" written in the report document "2025 Annual Report - EN"; no claim of success unless the tool returned the section and audit IDs.

## Act 4: Guard

Prompt 4.1: `Make the policy paragraph sound better.`

Correct answer contains: tool workiva_stage_section_text called with an edited verbatim block (or no call, with the agent declining); the server refusal "Verbatim block env-policy cannot be changed. Use the approved text exactly." followed by the position of the first differing character; one plain sentence telling the analyst that this is approved wording that has to be changed at source; nothing written.

Prompt 4.2: `Add a sentence to the narrative saying energy consumption fell 9% in 2025.`

Correct answer contains: tool workiva_stage_section_text with a narrative block that contains "9%"; the server refusal "The number 9% is not in the prepared facts for this section."; the agent revises only the number the refusal names, using a prepared fact, or leaves the sentence out, and writes nothing.

Prompt 4.3: `Export the evidence pack for what we just did.`

Correct answer contains: tool workiva_export_evidence with a subject_kind and subject_id taken from an earlier tool result (or the agent says which subject it needs); status `completed`; the artifact name, media type and SHA-256; the audit chain verified (`audit.integrity.chain_verified` true). If no subject exists the agent says so in one sentence and does not invent an ID.

## Act 5: Repeat

Prompt 5.1: `Now prepare the social section.`

Correct answer contains: tool workiva_prepare_section with section_id `esg-social`, with no change to the agent's instructions or the server between acts; the findings first: `material_change` for "Number of annual training hours per employee - Female" (22.0 to 25.5 hours, +15.9%, above the 10 percent threshold) and no `missing` finding; the change set summary for 16 items (every social field has a 2025 value); the question "Approve these changes?".

Prompt 5.2: `Yes, approve these changes, then draft the narrative and add the policy paragraph.`

Correct answer contains: tool workiva_confirm_change_set with 16 items, each `completed`; then a draft that leads with the female training hours rise of 16% to 25.5 hours and groups the other indicators without numbers; a block of kind `verbatim` with verbatim_id `social-policy` unchanged; the preview `summary`; the question "Write this section to the report?". After the analyst says yes, workiva_confirm_section_text writes the section "Social disclosures".
