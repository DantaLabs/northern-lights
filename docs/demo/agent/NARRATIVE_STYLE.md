# Narrative style for report sections

Copilot follows this guide when it drafts a narrative block. The reader is a director, shareholder or advisor who wants to know what changed and what it means, not the raw figure. The server's number guard rejects any number that is not a narrative fact, so the rules below also keep drafts from being refused.

Conventions checked on 2026-10-19 against the paragraphs of several existing sections of the sandbox report document (read-only; nothing copied):

- Refer to the company by its name, here "Demo Harbour Components". The existing sections also use "the Group" and "the Company" interchangeably; use the name so each paragraph stands on its own.
- British spelling (organisation, programme, decarbonisation, labour).
- Digits for every number, comma as thousands separator (18,400), full stop as decimal separator, "%" attached to the number (12%), a space between number and unit (1.98 GWh, 165.0 tCO2e), years as four digits ("in 2025", "compared with 2024").
- Unit symbols as the report writes them: MWh, GWh, tCO2e, m3.
- Past tense for results of a reporting year, present tense for policies, targets and commitments.

## Rules

1. Lead each paragraph with the movement and its size, then the absolute value: "Total energy consumption fell 3% year on year to 1.98 GWh." Never open with the raw figure alone.
2. Use only numbers from the narrative facts, copied exactly as one of their `Display` forms. Never round, convert, add or subtract yourself. Years may be written as the section's reporting and prior period.
3. Put material changes (facts with `Material: true`) first, largest first. Describe the remaining indicators together in one sentence without numbers, for example "Other environmental indicators remained within the materiality threshold."
4. For shares and rates (facts whose unit is %), describe the change in percentage points, not as a percentage of a percentage. Use the 0-decimal percentage by default. Use the 1-decimal form only when two changes in the same paragraph would otherwise look identical.
5. State causes only when the analyst gave them in the conversation or a fact label contains them. If the cause of a material change is unknown, ask the analyst before drafting; do not guess and do not leave a gap in the text.
6. Do not write about a field that has a `missing` finding. Tell the analyst it is missing instead.
7. Sentences of 25 words or fewer, active voice, past tense for the reporting period and present tense for policies and commitments.
8. Verbs: rose, fell, increased, decreased, was unchanged. Write "material" instead of "significant" (which implies a statistical test). No promotional or dramatic words (strong, impressive, remarkable, dramatic, robust), no speculation (likely, expected to, should).
9. Structure: paragraph 1 is the headline movement; paragraph 2 covers other material movements and the grouped sentence from rule 3; then the verbatim block, unchanged. Two narrative paragraphs at most.
10. Verbatim blocks are never edited, shortened, merged with narrative, translated or summarized. If the analyst asks to change one, say it is approved wording that cannot be changed here and that the owner of the policy text has to change it at source.

## Example

Narrative facts (synthetic, for Demo Harbour Components, period 2025, prior period 2024): total energy consumption `1.98 GWh`, prior `2.03 GWh`, change -3% (-2.6%), not material; "6. Business Travel" `165.0 tCO2e`, prior `140.0 tCO2e`, change 18% (17.9%), material; renewable electricity `490 MWh`, prior `520 MWh`, not material. The analyst said the business travel increase came from the return of customer site visits.

Data-dump draft (rejected by the style guide):

> Total energy consumption was 1.98 GWh. Business travel was 165.0 tCO2e. Renewable electricity was 490 MWh.

Advisory draft:

> Business travel emissions rose 18% to 165.0 tCO2e, as customer site visits returned.
>
> Other environmental indicators, including total energy consumption, remained within the materiality threshold.

Rule 3 puts the one material change first even though total energy consumption is the larger figure; energy consumption and renewable electricity disappear into the grouped sentence because they are not material, so the number guard has nothing to refuse.
