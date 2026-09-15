# Northern Lights

Open-source MCP server connecting Microsoft Copilot to Workiva
spreadsheets for EU reporting teams.

[![CI](https://github.com/DantaLabs/northern-lights/actions/workflows/ci.yml/badge.svg)](https://github.com/DantaLabs/northern-lights/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/dantalabs/northern-lights.svg)](https://pkg.go.dev/github.com/dantalabs/northern-lights)

## Why

EU implementation and sustainability reporting teams use Workiva for
CSRD/VSME/ESRS compliance, but programmatic editing of Workiva
spreadsheets is difficult. Implementation teams unfamiliar with the
API end up not using it at all.

Northern Lights exposes Workiva spreadsheets as MCP tools that any
MCP-compatible client (Microsoft Copilot, Claude, custom agents) can
call in natural language. It adds a semantic mapping layer (field names
to cells), a read cache for fast responses, and a hash-chained audit
trail for EU AI Act compliance.

## Quick start

### Docker

```bash
# Create deployments/.env with your credentials:
#   NL_API_KEY=your-secret
#   NL_WORKIVA_CLIENT_ID=your-client-id
#   NL_WORKIVA_CLIENT_SECRET=your-client-secret

docker compose -f deployments/docker-compose.yml up -d
```

### Demo mode (no Workiva credentials needed)

```bash
NL_DEMO_MODE=true NL_API_KEY= ./workiva-mcp
```

The server generates a random bearer token for the run and prints it to the
server log. Use that token when connecting an MCP client. All data is
synthetic. Connect any MCP client to `http://localhost:8080/mcp` to explore.

## Runtime configuration

YAML supports `region`, `db_path`, `listen_addr`, `read_cache_ttl`,
`require_write_confirmation`, `disable_localhost_protection`, and
`allowed_resources`. Environment overrides are `NL_REGION`, `NL_DB_PATH`,
`NL_LISTEN_ADDR`, `NL_READ_CACHE_TTL`, `NL_REQUIRE_WRITE_CONFIRMATION`,
`NL_DISABLE_LOCALHOST_PROTECTION`, and `NL_ALLOWED_RESOURCES`, plus the
environment-only credentials `NL_API_KEY`, `NL_WORKIVA_CLIENT_ID`, and
`NL_WORKIVA_CLIENT_SECRET`, and `NL_DEMO_MODE`.

The optional resource policy is a map from spreadsheet IDs to allowed sheet
IDs. Use `*` to permit every sheet in one spreadsheet:

```yaml
allowed_resources:
  spreadsheet-123: [sheet-a, sheet-b]
  spreadsheet-456: ["*"]
```

`NL_ALLOWED_RESOURCES` replaces the YAML value with the same structure as
JSON, for example `{"spreadsheet-123":["sheet-a"]}`. Missing policy means
unrestricted access within the Workiva grant. Once configured, all other
resources are denied and filtered from discovery, mappings, search, and cache
results. Empty or malformed policies stop startup, including explicit YAML null, empty
environment overrides, non-string IDs, and duplicate spreadsheet keys.
Policy changes require a process restart; persisted confirmation tokens are
checked against the new policy before execution.

Both probes are unauthenticated and return no resource or credential data.
`GET /healthz` reports process liveness. `GET /readyz` reports only whether
the local mapping and audit databases are reachable; it deliberately does not
depend on Workiva availability. Test Workiva connectivity separately by using
an authenticated MCP client connected to `/mcp` (see [Copilot setup](docs/COPILOT_SETUP.md)):

1. Call `workiva_list_spreadsheets` with `{}`. Confirm a permitted spreadsheet
   and sheet are returned; an empty catalog alone does not validate data access.
2. Call `workiva_read_range` with
   `{"spreadsheet_id":"<allowed-id>","sheet_id":"<allowed-sheet-id>","range":"A1"}`
   using an approved non-sensitive cell. Check `isError` is false and the
   returned `rows` match the expected cell. This tool reads Workiva directly.

Run with `NL_DEMO_MODE` unset or false to validate Workiva credentials and
API connectivity. This separate live smoke procedure has not been run here.
The container probe runs `/workiva-mcp healthcheck -url http://127.0.0.1:8080/readyz`;
if the listen address changes, update the probe URL and port mapping together.

The supported deployment boundary is one tenant and one replica.

## Tool catalog

| Tool | Description |
|---|---|
| `workiva_list_spreadsheets` | List connected spreadsheets and their sheets |
| `workiva_search_fields` | Search mapped fields by name or alias (call this first for NL references) |
| `workiva_get_field` | Get a field's current value (cached or live) |
| `workiva_read_range` | Read a raw cell range from Workiva |
| `workiva_update_field` | Update a field's value (two-phase confirmation by default) |
| `workiva_sync_mapping` | Discover and map fields from a two-column spreadsheet structure |
| `workiva_audit_trail` | Query recent audit log entries |

`workiva_update_field` returns one of these outcomes:

| Status | Certainty and reconciliation metadata | Retry or restaging guidance |
|---|---|---|
| `awaiting_confirmation` | No mutation was submitted. Returns the exact target, current `before` value, `after_preview`, and a single-use `confirm_token`. | The write may be abandoned safely. Use the token after approval, or restage to create a new preview. |
| `written` | Workiva reported completion. Returns the exact target, `before`, `after`, and `workiva_op_url`. | Do not retry. Stage another write only for a new intentional change. |
| `written_audit_failed` | Workiva reported completion, but the rich local audit append failed. Returns the exact target, `before`, `after`, and `workiva_op_url`. | Do not retry or restage the same value. Reconcile in Workiva history and preserve separate evidence. |
| `write_rejected` | The HTTP response establishes that Workiva did not accept the mutation. Returns the exact target, `before`, and intended `after_preview`; an operation URL is included if available. | Correct the request and restage before retrying. |
| `write_outcome_unknown` | Submission or polling ended without proof of the final outcome; the write may have completed. Returns the exact target, `before`, intended `after_preview`, and `workiva_op_url` when Workiva supplied one. | Do not retry or restage until the target and operation have been reconciled in Workiva. |
| `write_failed` | Workiva accepted the submission and then reported a terminal failed operation. Returns the exact target, `before`, intended `after_preview`, and `workiva_op_url`. | Inspect and reconcile the failed operation, correct the cause, then restage. |

`write_failed` is a known terminal operation result. `write_outcome_unknown`
means that submission or polling did not establish whether the mutation took
effect, so blind retrying could duplicate a write.

## Architecture

```
Copilot (MCP client)
  |
  v
Northern Lights (Go binary)
  |-- MCP streamable HTTP endpoint (/mcp)
  |-- Tool registry (7 tools, extensible)
  |-- Mapping store (SQLite: field names, cell cache)
  |-- Audit log (SQLite: hash-chained, append-only)
  |-- Workiva client (OAuth2, rate-limited, retrying)
  v
Workiva Spreadsheets API (2026-01-01)
```

## Documentation

- [Copilot Studio setup](docs/COPILOT_SETUP.md)
- [Example prompts](docs/EXAMPLE_PROMPTS.md)
- [EU AI Act compliance](docs/compliance/AI_ACT.md)
- [Technical documentation](docs/compliance/TECHNICAL_DOCUMENTATION.md)
- [Audit log](docs/compliance/AUDIT.md)
- [Transparency notice](docs/compliance/TRANSPARENCY.md)
- [Backlog](BACKLOG.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE)
