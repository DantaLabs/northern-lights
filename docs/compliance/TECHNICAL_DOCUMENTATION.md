# Technical Documentation (Annex IV)

Per EU AI Act Annex IV, this document describes the Northern Lights MCP
server's capabilities, limitations, intended use, and data flows.

## 1. General description

Northern Lights is a deterministic, rule-based software tool that:
- Maps semantic field names (e.g. "scope1_kwh") to Workiva spreadsheet
  cell references (A1 notation).
- Translates tool calls from an MCP client (typically a Copilot LLM)
  into Workiva Spreadsheets API (version 2026-01-01) requests.
- Caches read data locally for fast repeated access.
- Attempts to record every tool call and data mutation in a hash-chained,
  application-level append-only audit log. Write calls are refused when the
  initial audit record cannot be appended; some read-only calls can continue
  after the failure is logged.

It contains no machine-learning model, no training data, and no
learned parameters. All behaviour is deterministic, and the audit mechanisms
are operator-verifiable.

## 2. System architecture

```
Copilot Studio (MCP client)
  |  streamable HTTP, bearer auth
  v
Northern Lights (this software)
  |  internal/mcpserver: 7 tools, bearer auth middleware, audit
  |  internal/mapping: SQLite field mapping + cell cache
  |  internal/audit: hash-chained log
  |  internal/workiva: OAuth2 client, rate-limited, retrying
  v
Workiva Spreadsheets API (2026-01-01)
```

All data at rest is in a single SQLite database on the operator's
infrastructure. No data is sent to third parties other than the Workiva API
or identity service and the MCP client.

## 3. Intended use

- EU sustainability and financial reporting teams using Workiva who
  want to query and edit spreadsheet data through natural-language
  interfaces.
- Implementation teams who need to map Workiva spreadsheet fields to
  semantic names for ESG/CSRD/VSME reporting.

## 4. Known limitations

1. **Natural-language ambiguity.** The LLM may map a user query to the
   wrong field. The two-phase write confirmation is the primary
   safeguard; the deployer should train users to review confirmations.

2. **Write payload schema unverified against live API.** The `editCells`
   payload shape is built from Workiva's 2026-01-01 documentation, not
   from a live workspace. First deployment must validate against a real
   environment.

3. **Spreadsheet-only scope.** Workiva Documents API (prose editing) is
   not supported. Deferred to v0.2.

4. **Rate limits are workspace-wide.** Heavy Northern Lights usage
   counts against the same 600/min (reads) and 60/min (writes) limits
   as all other integrations in the workspace.

## 5. Data flows

| Data | Source | Destination | Retention |
|---|---|---|---|
| OAuth2 client credentials | Env vars | Workiva token endpoint | Never stored |
| Access token | Workiva token endpoint | In-memory cache | Until expiry + 30s buffer |
| Field mappings | YAML config or `workiva_sync_mapping` tool | SQLite (mapping tables) | Until manually deleted |
| Cell values | Workiva sheetdata endpoint | SQLite (snapshots table) | Overwritten on each read, controlled by `read_cache_ttl` |
| Audit entries | Tool-call and mutation audit records | SQLite (audit_log table) | Default 10-year guidance, operator-managed |

## 6. Human oversight measures

- `RequireWriteConfirmation` (default true): writes require a two-phase
  confirm token, ensuring a human or the LLM explicitly approves each
  mutation.
- Successful Workiva field updates include before and after values in their
  mutation audit record; an audit append failure is returned to the caller.
- The audit log is verifiable: changing a stored row or deleting a row with a
  successor breaks the hash chain, and `audit verify` reports the first
  corrupted sequence number.
