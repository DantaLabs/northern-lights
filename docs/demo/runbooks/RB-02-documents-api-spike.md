# RB-02 Documents API spike

Run by the L3 agent on Tom's machine, through the Workiva API with the sandbox credentials in `deployments/.env`. Decides whether L3 writes the narrative into a report document section or falls back to a narrative sheet.

Reference: Workiva Platform API 2026-01-01, groups Documents ("Create a new section in a document", "Partially update a single section", "Delete a single section") and Content ("Initiate edits to rich text"), https://developers.workiva.com/2026-01-01/spreadsheets.html. Read each endpoint's own page before calling it.

## Steps

1. Get a token with the existing OAuth provider in `internal/workiva/auth.go` (write a throwaway program under `.scratch/`, never committed).
2. List documents and find the annual report document from RB-01 `resources.md`. If it is not listed, list all documents the grant can see and save the list. Save as `step-2.json`.
3. Create a section titled `NL test <timestamp>` in that document. Save request path, body and response as `step-3.json`.
4. Write two plain paragraphs into that section with the rich-text or section edit endpoint. Poll the operation to a terminal status. Save as `step-4.json`.
5. Read the section's content back. Save as `step-5.json`. Note whether the two paragraphs match exactly.
6. Delete the test section. Save as `step-6.json`.
7. Write `result.md`: which endpoints worked, the exact paths and bodies L3 should use, and the decision "document" or "sheet fallback".

## Checklist

- [ ] The annual report document was found, or `step-2.json` shows why not.
- [ ] Section creation returned a section ID.
- [ ] The edit reached a terminal operation status.
- [ ] Read-back matches the two paragraphs, or `result.md` explains the difference.
- [ ] The test section was deleted.
- [ ] `result.md` states "document" or "sheet fallback".
- [ ] No token or secret in any saved file.
