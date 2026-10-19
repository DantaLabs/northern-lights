# Code map for the demo

Where the existing code each lane builds on lives. Paths and names were checked against `main` at commit `62b60e2` on 19 October 2026. Read the file before relying on a signature.

## Server and tools

| What | Where |
|---|---|
| Server entry point, flag parsing, tool registration loop (`for _, tool := range tools.All()`) | `cmd/workiva-mcp/main.go`, `buildServer` |
| Mapping file loading, including the demo-mode branch that ignores `-mappings` | `cmd/workiva-mcp/main.go`, around `bootstrap.LoadDemoMappings` |
| `Tool` interface, `Registry`, `Deps` | `internal/mcpserver/registry.go` |
| Actor resolution (Entra actor first, then the `nl-actor` header) | `internal/mcpserver/server.go`, `ActorFromContextOrRequest` |
| The 13 current tools, in order | `internal/mcpserver/tools/all.go` |
| Tool error helpers, resource allowlist, cache TTL, cell text | `internal/mcpserver/tools/helpers.go`: `fail`, `failMsg`, `requireDeps`, `resourceAllowed`, `denyResource`, `cellText`, `valueFromCells` |
| Tool-count assertions (13) that must not change | `internal/mcpserver/tools/schema_test.go`, `json_transport_test.go`, `json_transport_acceptance_test.go`, `transfer_value_test.go` |

## Writes, confirmation and audit

| What | Where |
|---|---|
| Two-phase write: stage, single-use token, confirm, write, read-back, audit, cache refresh | `internal/mcpserver/tools/update_field.go`: `stageWrite`, `executeConfirmedWrite`, `executeWrite`, `auditWrite`, `refreshCacheAfterWrite`, `expandCellEdits` |
| Pending write persistence bound to actor and permission, token digest | `internal/mapping/store.go`: `CreatePendingWrite`, `ConsumePendingWriteFor`, `tokenDigest` |
| Permission names (`workiva.write.confirm` and others) | `internal/identity/principal.go` |
| Hash-chained audit log | `internal/audit/log.go`: `Append`, `AppendTx`, `Verify` |
| SQLite open and migrations | `internal/sqlitedb/`: `Open`, `Migrate` |

## Workiva client

| What | Where |
|---|---|
| HTTP client with rate limiter and retries | `internal/workiva/client.go`: `NewClient`, `Do` |
| Sheet reads, typed reads, A1 ranges | `internal/workiva/sheets.go`: `GetSheetData`, `GetSheetDataTyped`, `GetRangeValues`; `internal/workiva/a1.go` |
| Sheet updates (edit cells, literal edit cells, formats, insert rows) | `internal/workiva/update.go`: `NewEditCellsUpdate`, `NewLiteralEditCellsUpdate`, `UpdateSheet`, `UpdateSheetWithRetryAfter`, `WriteCells` |
| Operation polling | `internal/workiva/operations.go`: `WaitOperation`, `WaitOperationWithInitialRetryAfter`, `InspectOperation` |
| Demo-mode fixture transport (VSME energy spreadsheets) | `internal/workiva/demo_client.go` |
| Provider router, read and write capabilities, optional capability pattern | `internal/workivaprovider/`, see `ContentMetadataReader` |

## Mapping and configuration

| What | Where |
|---|---|
| Mapping store: spreadsheets, sheets, fields, cache | `internal/mapping/store.go`: `GetField`, `SearchFields`, `SyncMapping`, `CacheCells`, `GetCachedCells` |
| Declarative mapping YAML and demo mappings | `internal/bootstrap/bootstrap.go`: `LoadMappings`, `LoadDemoMappings`, `DemoMappings`; example in `configs/example.yaml` |
| Configuration and environment variables (`NL_DEMO_MODE`, `RequireWriteConfirmation`) | `internal/config/config.go` |
| Decimal helpers (read only, assurance is frozen) | `internal/assurance/decimal.go` |

## Frozen for the demo

`internal/content/`, `internal/transfer/`, `internal/backupjob/`, `cmd/nl-backup-job/`, `cmd/nl-assurance/`, and the backup, restore and evidence storage files in `internal/assurance/`. See ADR 0002.

## Scripts

| What | Where |
|---|---|
| Local server plus Cloudflare Quick Tunnel for Copilot Studio tests | `scripts/test-session.sh` (`start`, `status`, `requests`, `stop`) |
| MCP smoke test (tool count, 401 on bad key) | `scripts/verify-mcp.sh` |
