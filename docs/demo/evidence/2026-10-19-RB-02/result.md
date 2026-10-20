# RB-02 result (2026-10-19)

**Decision: document.** L3 writes the narrative into a section of the report document. The sheet fallback (R8) is not needed.

All five endpoints worked against the sandbox (eu region, `https://api.eu.wdesk.com`, header `X-Version: 2026-01-01`, scope `file:read file:write`). Two paragraphs were written into a new section and read back character for character. The test section was then deleted.

The repository is public, so the IDs below are placeholders. `<document-id>` is the report document `2025 Annual Report - EN`, the only document the grant can see (`step-2.json`). The real values are in the uncommitted `*.local.json` files in this folder.

## Calls L3 should use

### 1. Find a section by title (`EnsureSection`)

`GET /documents/{documentId}/sections?$maxpagesize=100`, following `@nextLink`.

The response is `data[]`, where each item has `id`, `name`, `parent` (null or `{id, name}`), `index` and `children`. Match on `name`. Section IDs look like `<document-id>_<n>`.

### 2. Create a section if none matches (`EnsureSection`)

`POST /documents/{documentId}/sections`

```json
{"name": "Environmental disclosures"}
```

This returns `201` synchronously. There is no operation.

```json
{"children": [], "id": "<document-id>_244", "index": 0, "name": "NL test 20261019T…Z", "nonPrinting": false, "parent": null}
```

With no `parent`, the section is created at the top level. The response shows `index: 0` even though the document already had other top-level sections. To nest it, for example under "2. Environmental information", send `"parent": {"id": "<section id>"}`. This run did not test nesting; see the open points.

### 3. Get the section's rich-text ID

`GET /documents/{documentId}/sections/{sectionId}`

Use `body.richText`, not the section ID, as the rich-text ID. Use `body.revision` as the edit base revision. `headers.odd.richText` and `footers.odd.richText` are the header and footer containers; do not use them.

### 4. Write the paragraphs (`WriteSectionText`)

`POST /content/richText/{richTextId}/edit`

```json
{
  "revision": "<body.revision>",
  "isolateEdits": false,
  "data": [
    {
      "type": "insertText",
      "insertText": {
        "insertAt": {"paragraphIndex": 0, "offset": 0},
        "text": "<paragraph 1>\n<paragraph 2>"
      }
    }
  ]
}
```

- A `\n` in `text` starts a new paragraph, so one edit writes every paragraph.
- The response is `202` with headers `Location: https://api.eu.wdesk.com/operations/{operationId}` and `Retry-After: 2`. The body is `{"operationLocation": "<same URL>"}`.
- Poll the operation with `GET {Location}` (the existing `Client.WaitOperationWithInitialRetryAfter` does this). The first poll returned `{"status": "completed", "resourceUrl": ".../operations/{operationId}/richTextBatchEditResults"}`.
- The results `GET /operations/{operationId}/richTextBatchEditResults` returned `{"data": [], "revision": "…"}`.
- To replace existing text in a section that already has content, the rich-text guide says to put a `deleteText` over the old selection before the `insertText` in the same batch, with `isolateEdits: false`. This run did not test that, because the new section was empty.

### 5. Read the text back (`ReadSectionText`)

`GET /content/richText/{richTextId}/paragraphs`, following `@nextLink`.

The response is `data[]`, one item per paragraph with `index`. The text is in `elements[].textSpan.text`, where the element has `type: "textSpan"`. Concatenate the spans of each paragraph. The read-back gave exactly the two paragraphs that were written (`step-5.json`, `exact_match: true`).

### 6. Delete a section (live test cleanup only)

`DELETE /documents/{documentId}/sections/{sectionId}` returns `204`. A later `GET` of the same section returns `404 "section not found"`.

## Open points for L3

- **Nesting.** If the section should sit under "2. Environmental information" rather than at the top level, test it in the live opt-in test (scenario 10).
- **Replacing text.** For `WriteSectionText` on a section that already has text, read the paragraphs, delete from (0,0) to the end of the last paragraph, then insert. The delete was not exercised here.
- **Empty section.** A new section's body already has one empty paragraph. Writing at (0,0) fills it, so no empty paragraph is left behind.
- **Run history.** The spike ran twice. The first run's read-back parser looked for the wrong field (`text` instead of `textSpan.text`); the API response was already correct. The section from each run was deleted (`204`), and a final listing showed no sections titled "NL test" left.
