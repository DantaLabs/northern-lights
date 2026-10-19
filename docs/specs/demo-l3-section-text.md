# Demo L3: section text

## Goal

Write the section narrative into the Workiva report document safely. Copilot drafts the text; the server refuses any change to verbatim blocks and any number that is not a narrative fact, shows one preview, and after one approval creates the section if needed, writes the text and reads it back.

## Non-goals

- Drafting or rewriting text on the server.
- Tables, images, styles or formatting inside the section beyond plain paragraphs.
- Numbers written as words ("eight per cent"); the demo narrative style uses digits.

## Users and scenarios

- After the change set lands, Copilot drafts two narrative paragraphs from the narrative facts, adds the verbatim policy block unchanged, and calls `workiva_stage_section_text`. The analyst approves; "Environmental disclosures" appears in the report document.
- The analyst asks Copilot to "make the policy paragraph sound better". Copilot's edited block is refused with a plain message it relays to the analyst.

## Requirements

R1: First task: run runbook RB-02 against the sandbox and record in `docs/demo/evidence/<date>-RB-02/` the exact request paths, bodies and responses for creating a section, editing its rich text and reading it back. R7 and R8 follow what RB-02 shows.
R2: `Stage` refuses unless `recipe.FactStore.LatestFacts` returns facts for the same actor and section, with the message "Prepare the section first."
R3: `Stage` accepts 1 to 20 blocks of at most 4,000 characters each.
R4: A `verbatim` block must name a verbatim ID from the stored set and its text must equal the stored text byte for byte, after converting CRLF to LF and nothing else; otherwise `Stage` refuses with "Verbatim block <ID> cannot be changed. Use the approved text exactly." and the first differing character position.
R5: Number guard: every numeric token in a `narrative` block (digits with optional sign, thousands separators, decimal point and trailing %, not immediately preceded by a letter, so "m3" and "CO2" are not tokens) must equal the numeric part of a fact `Display` form, appear in a fact `Label` (for example "Scope 1"), or be the section's `period` or `prior_period`; otherwise `Stage` refuses with "The number <token> is not in the prepared facts for this section."
R6: `Stage` persists the preview in the existing SQLite database in tables prefixed `section_text_`, bound to actor, `workiva.write.confirm`, section ID and a SHA-256 of the full text, with a 10-minute expiry and only a digest of the token stored.
R7: `internal/workiva/documents.go` implements `workivaprovider.DocumentWriter` with the Workiva 2026-01-01 documents and content endpoints confirmed in RB-02: `EnsureSection` finds a section by exact title or creates it, `WriteSectionText` replaces its text with the given paragraphs and waits for the operation, `ReadSectionText` returns its paragraphs.
R8: If RB-02 shows that rich-text editing of document sections is not available, `NL_DEMO_TEXT_TARGET=sheet` with `NL_DEMO_NARRATIVE_SHEET=<spreadsheetID>/<sheetID>` writes one paragraph per row in column A of that sheet instead, and reads it back the same way.
R9: `Confirm` consumes the token once (unknown, reused, expired, foreign actor and missing permission each refused distinctly), then calls `EnsureSection`, `WriteSectionText` and `ReadSectionText`, and returns `completed` when the read-back equals the staged paragraphs and `mismatch` otherwise.
R10: `Confirm` writes one audit row with actor, section ID, document ID, Workiva section ID, the text SHA-256 and the verbatim IDs used.
R11: `internal/workiva/demo_documents.go` serves an in-memory document so demo mode can create a section, write text and read it back offline.
R12: `workiva_stage_section_text` takes `section_id` and `blocks`, returns the preview with a `summary` that shows each block with a `[verbatim]` or `[narrative]` label; `workiva_confirm_section_text` takes `text_id` and `confirm_token`.

## Interfaces and data

Implements `sectiontext.Service` from `internal/sectiontext/types.go` and `workivaprovider.DocumentWriter` from `internal/workivaprovider/documents.go` (both created by L0). Reads `DocumentID`, `Title`, `Period` and `PriorPeriod` from `recipe.Service.Sections()`.

Workiva 2026-01-01 reference lists "Create a new section in a document" under Documents and "Initiate edits to rich text" under Content: https://developers.workiva.com/2026-01-01/spreadsheets.html. Whether rich-text edits apply to document sections is not confirmed there; RB-02 decides.

## Constraints

- Tests use a fake `recipe.FactStore` until L2 merges.
- Live tests create sections only with titles starting `NL test ` and delete them at the end with "Delete a single section".
- No retries after an uncertain write outcome; return `reconciliation_required`.

## Acceptance scenarios

1. (R1) The RB-02 evidence folder has the three request and response pairs, with tokens removed.
2. (R2) Staging before any prepare call is refused with "Prepare the section first."
3. (R3) Zero blocks, 21 blocks, or a 4,001-character block are refused.
4. (R4) The stored verbatim text passes; the same text with CRLF line endings passes; one changed character fails with the block ID and position.
5. (R5) "Energy consumption fell 8.1% to 1.24 GWh in 2025" passes against the L2 example fact; "fell 9%" fails naming "9%"; "in 2025 compared with 2024" passes; "Scope 1 emissions" passes when a fact label contains "Scope 1"; "18,400 m3" passes when "18,400 m3" is a display form.
6. (R6, R9) Reused, expired, foreign-actor and missing-permission confirmations are refused with four distinct errors and nothing is written.
7. (R7, R9, R10, R11) In demo mode, confirm creates "Environmental disclosures", writes three paragraphs, reads back the same three, returns `completed` and writes one audit row.
8. (R7) Calling `EnsureSection` twice with the same title creates one section.
9. (R8) With `NL_DEMO_TEXT_TARGET=sheet`, the same flow writes rows A1 to A3 of the configured sheet and reads them back.
10. (R7, R9) Live, opt-in test (`NL_LIVE_SANDBOX=1`): create "NL test <timestamp>" in the sandbox report document, write and read back two paragraphs, then delete the section.

## Open questions

None

## Approval

Approved: pending Tom
