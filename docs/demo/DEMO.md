# KPMG demo: ESG section pipeline

Due Monday 2 November 2026. Recorded video first, live run if Copilot Studio behaves on the day.

## Goal

Show the KPMG Workiva advisory team that an analyst can run a report section from Teams: ask questions and get answers cited to Workiva cells, see problems the agent found without being asked, approve one change set that fills the section's spreadsheet values, and get a consultant-quality narrative written into the report document, with legal wording kept exactly as approved and every action in the audit trail. Then run the same recipe on a second section to show it repeats.

The audience is not technical. They should leave believing two things: this saves analyst hours on every section, and their IT team can approve it because nothing leaves the Microsoft tenant except through Copilot and every write is approved and logged.

## Demo script

Split screen: Copilot in Teams on the left, the Workiva workspace on the right. 7 to 8 minutes.

1. Ask (2 min). Questions across the workspace, for example "What was total energy consumption in 2025 and how does it compare with 2024?". Answers read current and prior cells, give both values with spreadsheet, sheet and cell citations, and state the direction in words without computing a percentage. Follow-up questions work. Uses existing read tools plus the agent instructions in `docs/demo/agent/INSTRUCTIONS.md`.
2. Catch (1 min). The analyst asks to prepare the environmental section. Before proposing anything, the agent lists findings: a missing value, a change above the materiality threshold. Lane L2.
3. Build (2 min). One preview of about 30 cell changes, one approval, values fill on the right. Then the narrative: Copilot drafts from narrative facts, the server checks it, and a new "Environmental disclosures" section appears in the report document with the narrative and the verbatim policy paragraph. Lanes L1, L2, L3.
4. Guard (1 min). The analyst asks Copilot to "make the policy paragraph sound better" and the server refuses because verbatim blocks cannot change. A draft with a number that is not a narrative fact is refused by the number guard. The analyst asks "Show me the audit trail for what we just wrote."; `workiva_audit_trail` shows the confirmed writes with their actor and Workiva operation fields. Lane L3 plus existing tools.
5. Repeat (1 min). "Now prepare the social section." Same recipe engine, different YAML. Lanes L2 and L4.

Closing slide: the annual report sections with their status (environmental and social live, the rest "same recipe, needs a mapping file and sample data") and the analyst hours the ESG section takes today by hand. That number comes from the KPMG team: PENDIENTE.

## How it works

1. Copilot Studio agent, published to Teams, calls Northern Lights over MCP with the analyst's delegated Entra identity.
2. Northern Lights reads Workiva through the existing REST client and mapping store.
3. `workiva_prepare_section` runs a section recipe: reads source and prior-period fields, computes narrative facts, runs checks, stages a change set and returns findings, facts, verbatim blocks and the preview.
4. `workiva_confirm_change_set` writes all values in one approved operation per sheet, reads them back and writes one audit row per field.
5. `workiva_stage_section_text` checks the drafted text (verbatim blocks unchanged, every number a narrative fact) and `workiva_confirm_section_text` creates the report document section and writes the text.

No model runs inside the server (ADR 0001). All demo tools are behind `NL_DEMO_PIPELINE=true` (ADR 0002).

## Scope

In scope: two sections (environmental, social) from the VSME-style ESG template, two reporting years of synthetic data, one report document, the four demo tools above, agent instructions with the consultant style guide, a two-page IT and risk brief.

Evidence export is after the demo; the live script shows the audit trail.

Out of scope until after the demo: PDF or file ingestion through Copilot, scanned documents, more than two sections, per-user Workiva permissions, backup and restore changes, Wave 4.2, 4.3 and 5, the official Workiva MCP adapter.

## Sandbox resources

| Resource | Purpose | Agents may write |
|---|---|---|
| ESG source spreadsheet (name PENDIENTE) | The filled KPMG-style template, current year | Only via the L4 seed command |
| ESG report spreadsheet (name PENDIENTE) | Target values, current and prior year | Yes, through the demo tools |
| Annual report document (name PENDIENTE) | Target for the narrative sections | Yes, through the demo tools |
| `Testing MCP` / `Sheet1` | Scratch range used since the 15 September validation | Yes, any test |

Tom fills the PENDIENTE names on day 1 from `workiva_list_spreadsheets`. Use synthetic data only. The company in the data is fictional.

## Calendar

| Days | Work |
|---|---|
| Mon 19 to Tue 20 Oct | L0 scaffold merged; RB-01 baseline smoke test; RB-02 documents API spike; L4 synthetic data and section YAML drafts |
| Wed 21 to Fri 23 Oct | L1 change set, L2 section recipe, L3 text checks, in parallel |
| Mon 26 to Tue 27 Oct | L3 document section writer; L4 social section; integration on `claude/demo-integration` |
| Wed 28 to Thu 29 Oct | Copilot Studio wiring and Teams publish (RB-03); IT and risk brief |
| Fri 30 Oct to Mon 2 Nov | Rehearsal, recorded video, buffer |

## Done means

RB-03 passes end to end on the sandbox, the video is recorded, CI is green on `claude/demo-integration`, and the IT brief is written.
