# Narrative style for report sections

Copilot follows this guide when it drafts a narrative block. The reader is a director, shareholder or advisor who wants to know what changed and what it means, not the raw figure. The server's number guard rejects any number that is not a narrative fact, so the rules below also keep drafts from being refused.

PENDIENTE: compare these rules with one or two past report sections from the KPMG templates in the sandbox and adjust wording conventions (for example "the Group" or the company name, British or American spelling) to match.

## Rules

1. Lead each paragraph with the movement and its size, then the absolute value: "Total energy consumption fell 8% year on year to 1.24 GWh." Never open with the raw figure alone.
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

Narrative facts (synthetic): energy total 1.24 GWh, change -8% (-8.1%), not material; renewable share 46%, prior 38%, change 8 percentage points, material; water withdrawal 18,400 m3, change 12%, material; analyst said the renewable increase came from a new on-site solar installation.

Data-dump draft (rejected by the style guide):

> Total energy consumption was 1.24 GWh. Renewable share was 46%. Water withdrawal was 18,400 m3. Waste was 312 t.

Advisory draft:

> The share of renewable energy rose 8 percentage points to 46%, following the commissioning of an on-site solar installation. Water withdrawal increased 12% to 18,400 m3.
>
> Total energy consumption fell 8% to 1.24 GWh. Other environmental indicators remained within the materiality threshold.

Rule 3 puts the material changes first even though energy consumption is the largest absolute figure; the waste figure disappears into the grouped sentence because it is not material.
