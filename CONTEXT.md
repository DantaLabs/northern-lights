# Northern Lights

An MCP server that lets Microsoft Copilot read and write Workiva spreadsheets and documents for reporting analysts, with every write previewed, approved by a person and recorded in a hash-chained audit trail.

## Workiva objects

**Spreadsheet**:
A Workiva spreadsheet, addressed by spreadsheet ID and sheet ID.
_Avoid_: workbook, file

**Report document**:
The Workiva document that holds the annual report text, split into sections.
_Avoid_: doc, Word file, report file

**Section**:
One named part of a report document, such as "Environmental disclosures".
_Avoid_: chapter, page

**Field**:
A named mapping from a business term to an exact cell range in a spreadsheet, stored in the mapping store.
_Avoid_: variable, cell name

## Demo pipeline

**Section recipe**:
A YAML definition of one report section: which source fields feed which target fields, the prior-period fields, the validation rules and the report document section it writes to.
_Avoid_: workflow, pipeline definition, template

**Change set**:
A list of field writes staged together, shown as one preview and approved with one confirmation token.
_Avoid_: batch, bulk write, transaction

**Finding**:
A problem or notable change the server detects while preparing a section, such as a missing value or a large year-on-year change.
_Avoid_: alert, warning, insight

**Narrative fact**:
A number the server computed for the narrative (current value, prior value, change, percentage, direction, display string). Copilot may only use numbers that appear as narrative facts.
_Avoid_: metric, KPI, stat

**Verbatim block**:
Approved legal or policy wording that must appear in the report exactly as stored, character for character.
_Avoid_: boilerplate, legal snippet, quote

**Narrative block**:
Text Copilot drafts from narrative facts in the consultant style.
_Avoid_: summary, commentary block, AI text

**Number guard**:
The server check that every number in a narrative block matches a narrative fact display string.
_Avoid_: hallucination check, number validation

## Ways of working

**Lane**:
One parallel stream of demo work with its own branch, its own files and one spec in `docs/specs/`.
_Avoid_: track, workstream, epic

**Runbook**:
Step-by-step instructions for a test Tom runs by hand, with a checklist and an evidence folder.
_Avoid_: test plan, manual test script
