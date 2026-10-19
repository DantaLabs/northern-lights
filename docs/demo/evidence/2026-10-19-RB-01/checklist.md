# RB-01 checklist (2026-10-19)

The run used `claude/demo-integration` at 448764f, with L0 merged and `NL_DEMO_PIPELINE` unset. It ran through the local endpoint `http://localhost:8090/mcp`. `step-1.txt` shows that the Quick Tunnel from step 1 also passed `verify-mcp.sh`.

- [x] Step 2 reports 13 tools and a 401 for the malformed key. See `step-2.txt`.
- [x] Step 3 returns at least the ESG template and the report spreadsheet. See `step-3.json`: `VSME-Digital-Template-1.0.1 Unlocked` and `VSME - Factbook template`, plus `Testing MCP` and `NL_RO_Scratch_20261009`.
- [x] `resources.md` lists every name and ID from step 4. The annual report document is marked PENDIENTE, with a note of what was searched. See `resources.md`.
- [x] Step 5 returns values without an error. See `step-5.json`: `Testing MCP` / `Sheet1` A1:C10, 4 rows with values, no `isError`.
- [x] No token or key appears in any saved file. In `requests.txt` the key fingerprint is replaced with `<keyfp>`. The `key_sha256` line is removed from `step-1.txt`. All files were checked by grep for `Bearer`, `secret`, `token` and the key fingerprint.

Sandbox spreadsheet and sheet IDs and `nl_audit_id` values are replaced with placeholders, because the repository is public. The real values are in the uncommitted `*.local.*` files in this folder.

Other files: `step-1.txt` (session start), `step-6.txt` (session stop), `requests.txt` (redacted request log).

Result: **pass**.
