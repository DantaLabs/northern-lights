# RB-01 Baseline smoke test

Runs on Tom's machine through the API only. Confirms the current `main` still works against the sandbox and records the names and IDs the demo needs.

## Steps

1. Start a local session: `scripts/test-session.sh start`. It builds the server, starts it with `deployments/.env` and opens a Quick Tunnel.
2. Run `NL_URL=http://localhost:8090/mcp NL_KEY=<key from deployments/.env> scripts/verify-mcp.sh`. Save the output as `step-2.txt`.
3. Call `workiva_list_spreadsheets` through the local endpoint (a JSON-RPC `tools/call` with the same headers `verify-mcp.sh` uses). Save the result as `step-3.json`.
4. From step 3, write down the spreadsheet name and ID, and every sheet name and ID, of: the KPMG ESG template, the ESG report spreadsheet, the annual report document if it appears, and `Testing MCP`. Save them as `resources.md`.
5. Call `workiva_read_range` on `Testing MCP` / `Sheet1` range `A1:C10`. Save as `step-5.json`.
6. Run `scripts/test-session.sh stop`.

## Checklist

- [ ] Step 2 reports 13 tools and a 401 for the malformed key.
- [ ] Step 3 returns at least the ESG template and the report spreadsheet.
- [ ] `resources.md` lists every name and ID from step 4. Anything not found is marked PENDIENTE with what was searched.
- [ ] Step 5 returns values without an error.
- [ ] No token or key appears in any saved file.

## After the run

Tom copies the names from `resources.md` into the "Sandbox resources" table of `docs/demo/DEMO.md`, and L4 uses the IDs in `configs/demo/mappings-sandbox.yaml`.
