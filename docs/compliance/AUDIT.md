# Audit Log

## Design

The audit log is an application-level append-only SQLite table with a
SHA-256 hash chain. Each row hashes the previous row's hash along with its
own fields. Modifying any row, or deleting a row that has a successor, breaks
the chain at that sequence number. Deleting only the final row is not
detectable by the chain verifier.

```
row N: hash = sha256(row[N-1].hash || ts || actor || tool || action || target || before || after)
row 0: hash = sha256("GENESIS" || ts || actor || tool || action || target || before || after)
```

The hash uses the stored datetime format (`2006-01-02 15:04:05` UTC),
not the RFC 3339 representation, so the chain is stable across reads.

## What is logged

| Field | Description |
|---|---|
| seq | Auto-incrementing sequence number |
| ts | UTC timestamp |
| actor | Who triggered the action (`X-NL-Actor` header, or `copilot` by default) |
| tool | The MCP tool name (e.g. `workiva_update_field`) |
| action | What happened (`call`, `read`, `write`, `sync`, `init`) |
| target | Identifier (e.g. `spreadsheetId/sheetId/B3:D10` or field name) |
| before_json | Cell value or state before the mutation (writes only) |
| after_json | Cell value or state after the mutation (writes only) |
| workiva_op_url | The Workiva operationLocation URL for cross-referencing with Workiva's own file history |

## Verifying the chain

```bash
./workiva-mcp audit verify -db /path/to/northern-lights.db
```

Exit code 0 means the chain is intact. Non-zero exit with the first
broken sequence number means tampering was detected.

## Exporting for auditors

```bash
./workiva-mcp audit export -db /path/to/northern-lights.db -format jsonl
```

One JSON object per line, suitable for ingestion into SIEM systems,
compliance dashboards, or archival.

## Retention guidance

EU financial reporting norms (ESRS, CSRD) require document retention of
at least 10 years. The audit log does not auto-expire. Operators should
include the database file in their backup and retention policies.

## Integration with Workiva's own history

Every Workiva field update through `workiva_update_field` stores the Workiva
`operationLocation` URL in the `workiva_op_url` column. An auditor can follow
this URL (authenticated) to see the exact Workiva file revision that resulted
from the mutation.
