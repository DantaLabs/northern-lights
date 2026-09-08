# Example Prompts for Copilot + Northern Lights

These prompts show how a reporting analyst would interact with Workiva
through Copilot. Each entry lists the user prompt, the expected tool
sequence, and what the user should see.

---

**"List all connected Workiva spreadsheets."**

Tools: `workiva_list_spreadsheets`

Copilot returns the mapped spreadsheet and sheet names with their IDs.

---

**"Show me all mapped fields for the B3 energy report."**

Tools: `workiva_search_fields(query: "energy")`

Copilot returns matching field names, ranges, descriptions, and cached
values when available.

---

**"What is our current Scope 1 energy consumption?"**

Tools: `workiva_search_fields(query: "scope 1")` then
`workiva_get_field(name: "scope1_kwh")`

Copilot resolves the field name, fetches the live value, and reports it
with units.

---

**"Read the full energy section, cells A1 through D10."**

Tools: `workiva_read_range(spreadsheet_id, sheet_id, range: "A1:D10")`

Copilot reads the range directly and formats the cell grid.

---

**"Update the total energy field to 42000."**

Tools: `workiva_search_fields(query: "total energy")` then
`workiva_update_field(name: "total_energy_kwh", value: 42000)`

Phase 1: Copilot receives a confirmation token and shows the before/after
preview.

**"Yes, go ahead."**

Tools: `workiva_update_field(name: "total_energy_kwh", value: 42000,
confirm_token: "<token>")`

Phase 2: value written, audit entry recorded.

---

**"Connect a new spreadsheet to the mapping layer."**

Tools: `workiva_sync_mapping(spreadsheet_id, sheet_id, name_column: "A",
value_column: "B", start_row: 2)`

Copilot reports how many fields were discovered and their names.

---

**"Who last changed the cell ss-1/sh-1/B3?"**

Tools: `workiva_audit_trail(limit: 10, target: "ss-1/sh-1/B3")`

Copilot lists recent audit entries for that exact target, showing actor, tool,
timestamp, and what changed.

---

**"What changed on the sustainability report this week?"**

Tools: `workiva_audit_trail(limit: 50)`

Copilot summarises recent entries, grouped by date or actor.

---

**"Show me the raw values in the carbon intensity sheet."**

Tools: `workiva_search_fields(query: "carbon intensity")` then
`workiva_read_range(spreadsheet_id, sheet_id, range)`

Copilot reads the range identified from the field mapping.

---

**"Connect our 2026 VSME report and tell me how many fields it has."**

Tools: `workiva_sync_mapping(...)` then
`workiva_list_spreadsheets()`

Copilot syncs the mapping and reports the count.
