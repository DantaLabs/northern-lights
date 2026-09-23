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

YAML supports `auth_mode`, `region`, `db_path`, `listen_addr`, `read_cache_ttl`,
`require_write_confirmation`, `disable_localhost_protection`, and
`allowed_resources`. Environment overrides are `NL_REGION`, `NL_DB_PATH`,
`NL_LISTEN_ADDR`, `NL_READ_CACHE_TTL`, `NL_REQUIRE_WRITE_CONFIRMATION`,
`NL_DISABLE_LOCALHOST_PROTECTION`, and `NL_ALLOWED_RESOURCES`, plus the
environment-only credentials `NL_API_KEY`, `NL_WORKIVA_CLIENT_ID`, and
`NL_WORKIVA_CLIENT_SECRET`, and `NL_DEMO_MODE`. `NL_AUTH_MODE` overrides YAML
and defaults to `api_key`.

### Entra authentication and permissions

`auth_mode: api_key` preserves the Phase 1 shared-key behavior. `auth_mode: entra`
validates a single tenant's Microsoft Entra access tokens through OIDC
discovery and cached JWKS rotation. Entra identity and authorization settings
are environment-only and startup fails if they are partial or unsafe:

```bash
export NL_AUTH_MODE=entra
export NL_ENTRA_TENANT_ID="<tenant-guid>"
export NL_ENTRA_AUTHORITY="https://login.microsoftonline.com/<tenant-guid>/v2.0"
export NL_ENTRA_AUDIENCE="<API-client-ID-GUID>"
export NL_ENTRA_SCOPE_PERMISSIONS='{"NorthernLights.Read":["workiva.read"]}'

# Leave these unset for delegated-only deployments. Configure both only after
# a real app-only token for this API proves that it contains idtyp: app:
export NL_ENTRA_ROLE_PERMISSIONS='{"NorthernLights.Reader":["workiva.read"]}'
export NL_ENTRA_APP_CLIENT_PERMISSIONS='{"<authorized-client-guid>":["workiva.read"]}'
```

Set the API app registration manifest's `requestedAccessTokenVersion` to `2`;
Northern Lights requires the tenant-specific `/v2.0` issuer. OAuth clients
request a delegated scope as `api://<API-client-ID>/<scope>` (or the API's
`api://<API-client-ID>/.default` grant where appropriate), but that scope
identifier is not the token audience configured above. For a v2 access token,
Entra emits the API application's client-ID GUID in `aud`, so
`NL_ENTRA_AUDIENCE` must be that GUID. Northern Lights compares `aud` exactly
and does not translate an `api://` Application ID URI into a GUID.
`NL_ENTRA_TENANT_ID` and `NL_ENTRA_AUDIENCE` must both be UUIDs and are stored
in lowercase hyphenated form. `NL_ENTRA_AUTHORITY` must already contain that
canonical tenant path exactly; a differently cased or otherwise noncanonical
path is rejected rather than rewritten.

For app-only support, merge this claim object into the API/resource app
registration manifest's existing `optionalClaims.accessToken` array. Preserve
every existing optional claim; do not replace the array with only this entry:

```json
{
  "optionalClaims": {
    "accessToken": [
      {
        "name": "idtyp",
        "source": null,
        "essential": false,
        "additionalProperties": []
      }
    ]
  }
}
```

Microsoft documents `idtyp` as the most accurate distinction between an app
token and an app-plus-user token and emits `app` for app-only tokens. Access
token optional claims must be configured on the resource/API registration
that owns the token. See Microsoft's
[Optional claims reference](https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference)
and [claims-validation guidance](https://learn.microsoft.com/en-us/entra/identity-platform/claims-validation).
Delegated-only deployments do not need `NL_ENTRA_ROLE_PERMISSIONS` or
`NL_ENTRA_APP_CLIENT_PERMISSIONS`. Do not enable app-only mappings until a real
access token issued for this API has been inspected and proves `idtyp` is
exactly `app`; this repository has not completed that live acceptance gate.

The complete permission vocabulary is `workiva.read`,
`workiva.write.preview`, `workiva.write.confirm`, `mapping.sync`, `audit.read`,
and `tenant.admin`. Delegated tokens receive permissions only through `scp`;
app-only tokens additionally require `idtyp` to be exactly `app` and receive
permissions only through the intersection of `roles` and the explicit
authorized-client policy. Entra mode accepts only `Bearer` access tokens and
never falls back to `NL_API_KEY` after JWT failure.

Entra-mode Streamable HTTP is intentionally stateless in Wave 1. It neither
issues nor relies on `Mcp-Session-Id`; every request revalidates the bearer
token and its permissions. API-key mode retains its existing stateful MCP
session behavior for compatibility. Stateful Entra sessions must not be
enabled unless a future shared, tenant-bound session store passes
cross-principal and cross-replica acceptance testing.

Signature, RS256 algorithm, key ID/key, exact issuer and audience, `exp`,
`nbf`, tenant, `sub`, and immutable object identity are validated before MCP
execution. Audit identity is `<tid>/<oid>`; email, UPN,
`preferred_username`, `unique_name`, `nl-actor`, and `X-NL-Actor` are never
authorization inputs. `NL_ENTRA_ALLOW_SUBJECT_FALLBACK=true` explicitly permits
the documented `<tid>/sub:<sub>` fallback when `oid` is unavailable; it is off
by default. Health and readiness probes remain anonymous and disclose no
identity configuration.

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

The supported deployment boundary remains one tenant and one replica. Wave 1
adds trusted principals and tool authorization, but tenant-scoped database
records and actor-bound confirmation tokens are deferred to Phase 2 Wave 2.

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
