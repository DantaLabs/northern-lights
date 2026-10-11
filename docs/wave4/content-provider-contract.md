# Content placement provider contract

The versioned Workiva endpoint and schema pages were checked on 2026-10-10 before the additive metadata adapter work.

- [Retrieve sheet data](https://developers.workiva.com/2026-01-01/getsheetdata.html): GET `/spreadsheets/{spreadsheetId}/sheets/{sheetId}/sheetdata`, `X-Version: 2026-01-01`, narrow `$cellrange`, and `$fields` rooted at the response `data` object. The response wraps `SheetData` in `data` and may include `@nextLink`.
- [CellData schema](https://developers.workiva.com/2026-01-01/spreadsheets.html): `value` is raw scalar content or a formula string; `calculatedValue` is a formula result; `formats` and `effectiveFormats` are distinct observations. Missing fields cannot establish a negative fact. Decimal values must retain their JSON number representation.
- [Update sheet content](https://developers.workiva.com/2026-01-01/updatesheet.html): edits use POST `/spreadsheets/{spreadsheetId}/sheets/{sheetId}/update` and `editCells.cells`. Asynchronous acceptance returns an operation location and polling delay. An accepted request alone does not establish the final value.

The documented CellData shape does not provide a cell-protection or writable flag. This inspection does not establish an alternative authoritative capability for either fact. A successful read, operator declaration, or user confirmation must not fill that gap. The default content metadata adapter must leave those facts unknown, and content placement must block until an independently verified provider capability supplies them.

Single-cell observations bind the exact coordinate, raw value, formatting fingerprint, and uncached-read provenance. Formatting changes, formula targets, protected cells, ambiguous interpretation, or missing critical observations require a new preview or denial. Tests may inject explicit verified observations; that is local behavior evidence and does not close the production provider gate.

Provider submission will reuse the existing reviewed mutation sequence. No content-specific alternative may weaken durable claim fencing, at-most-once submission, operation preservation, uncached read-back, or reconciliation after uncertainty.
