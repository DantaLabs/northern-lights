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

### Demo mode (no credentials needed)

```bash
NL_DEMO_MODE=true NL_API_KEY=demo ./workiva-mcp
```

All data is synthetic. Connect any MCP client to
`http://localhost:8080/mcp` to explore.

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
