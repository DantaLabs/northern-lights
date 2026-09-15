# Northern Lights Validation Plan and Findings

Date: 2026-09-15
Commit under test: `26b136fdc6a8a489277049af9a5187948dd8dee9`
Validation branch: `test/live-validation-2026-09-15`
Target: Workiva EU workspace, public Streamable HTTP MCP endpoint, post-MVP multi-user design

## Executive status

| Area | Status | Evidence |
|---|---|---|
| Local build, vet, format, race suite | PASS | `go vet ./...`, `gofmt -l .`, `go build ./...`, and `go test ./... -race -count=1` passed |
| Live Workiva OAuth and discovery | PASS AFTER FIX | Credentials authenticated against the EU API. The original commit failed decoding live spreadsheet timestamps. A contract fix and regression test now allow discovery of 3 spreadsheets and 44 sheets. |
| Live Workiva read and mapping sync | PASS | Read `A1:C10` from the dedicated `Testing MCP / Sheet1` resource and synced 3 mapped fields. |
| Live confirmed write, polling, read-back, restore | PASS | `test_write_value` changed from `0` to `424242`, completed asynchronously, read back as `424242`, then restored to `0` through the same confirmed-write path. Both operations report `completed`. |
| Workiva permission behavior | PARTIAL, FINDING | A token request asking only for `file:read` received all scopes configured on the API grant, including `file:write`, and a no-op write was accepted. Least privilege must be enforced on the Workiva grant itself. |
| Live rate behavior | PARTIAL | Workiva returned `Retry-After: 5` on accepted updates and `Retry-After: 1` while reading completed operations. The client completed polling correctly. A provider-side 429 was not deliberately induced. |
| 100,000-cell mapped-write cap | PASS LOCALLY AND THROUGH LIVE SERVER | Exact 100,000-cell expansion passes a focused boundary test. A live-configured mapping of 100,001 cells was rejected before any Workiva mutation. A destructive 100,000-cell live write was intentionally not sent. |
| Public HTTPS MCP transport and bearer auth | PASS | Cloudflare public tunnel accepted the Go MCP SDK client, discovered all 7 tools, and completed live Workiva discovery. Invalid bearer token returned HTTP 401. |
| `X-NL-Actor` propagation | PASS FOR DIRECT MCP, FAIL FOR MULTI-USER SECURITY | Direct MCP calls wrote the supplied actor to the audit chain. The header is caller-controlled and is not tied to authenticated claims. |
| Real Copilot Studio connector | BLOCKED | No authenticated Copilot Studio browser session was available. Tool discovery, Copilot auth behavior, dynamic actor propagation, and Copilot activity matching remain unvalidated. Proxy-shim need is still unknown. |
| Multi-user and horizontal scaling | NOT IMPLEMENTED, NEGATIVE BASELINE CONFIRMED | One bearer key authorizes every caller. A token staged as `alice` was successfully consumed as `bob`; the write audit actor became `bob`. SQLite state and rate buckets are replica-local. |
| Audit integrity | PASS | Validation database ended with 46 audit rows, 4 write rows, 7 distinct actors, and `audit verify` reported an intact chain. |

## Findings

### NL-LIVE-001, live discovery contract mismatch

Severity: High
Status: Fixed on validation branch

The live 2026-01-01 spreadsheet catalog and the official endpoint example return:

Source: <https://developers.workiva.com/2026-01-01/getspreadsheets.html>

```json
{
  "created": {"dateTime": "..."},
  "modified": {"dateTime": "..."}
}
```

Commit `26b136f` modeled both fields as strings. `workiva_list_spreadsheets` therefore failed before returning any resource. The mock fixture had copied the incorrect string shape, so the existing suite passed.

Fix:

- Updated the contract fixture to use the live object shape.
- Added decoding that extracts `created.dateTime` and `modified.dateTime` while preserving the existing public string fields.
- Watched the focused regression test fail with the live error, then pass after the implementation change.

### NL-LIVE-002, OAuth scope requests do not narrow this grant

Severity: High for least-privilege deployments
Status: Open governance action

Requesting a token with only `scope=file:read` returned the broader set configured on the Workiva API grant, including `file:write`. A no-op update on the disposable test cell returned 202 and completed.

Implication: changing the scope string in Northern Lights is not a reliable permission boundary for this grant. Create separate integration users and API grants with only the permissions each deployment needs. Validate the effective scopes returned by the token endpoint at startup without logging the token.

### NL-LIVE-003, async update polling matches live behavior

Severity: Informational
Status: Validated

Both the mutation and restoration returned accepted operation locations. Workiva returned an initial `Retry-After: 5`, and operation reads returned `Retry-After: 1`. Northern Lights waited, polled to `completed`, returned the operation URL, and allowed an uncached read-back.

### NL-LIVE-004, the expansion cap is enforced before mutation

Severity: Informational
Status: Validated with one residual boundary limitation

- 100,000 cells: exact boundary expansion test passes.
- 100,001 cells: a mapping against the live disposable sheet returned `mapped write range exceeds maximum of 100000 cells` before reading or mutating the range.
- The sheet remained unchanged.

A live write of exactly 100,000 cells was not sent because it would overwrite a large area merely to prove a local guard. If Workiva payload acceptance at that size is a release requirement, create a purpose-built blank 100,000-cell sheet and run the test in a scheduled sandbox window, then delete the sheet.

### NL-LIVE-005, provider-side 429 behavior remains unproven

Severity: Medium
Status: Open

Six live reads succeeded. Their observed per-call durations were approximately 0.27 to 1.01 seconds, with total elapsed time approximately 3.87 seconds. Network and Workiva latency dominated, so this does not isolate the 600-per-minute local limiter. No deliberate 429 burst was sent.

The mock suite validates 429 retry logic, but a controlled sandbox load test is still needed to capture Workiva's actual 429 body and `Retry-After` behavior.

### NL-COPILOT-001, direct public MCP works but Copilot acceptance is incomplete

Severity: High for the Copilot milestone
Status: Blocked on authenticated Copilot Studio access

Through a public HTTPS tunnel, an MCP SDK client with `Authorization: Bearer <NL_API_KEY>` discovered all 7 tools and completed live Workiva discovery. An invalid token returned 401. This validates Northern Lights transport and middleware, not Copilot Studio behavior.

Microsoft's current Copilot Studio documentation confirms Streamable HTTP, an MCP onboarding wizard, API-key authentication with a configurable header name, and OAuth 2.0 options. This makes a proxy unlikely to be necessary for static `Authorization` alone, provided the connection stores the full `Bearer <NL_API_KEY>` value. The documented API-key flow does not establish a trustworthy dynamic end-user identity. Source: <https://learn.microsoft.com/en-us/microsoft-copilot-studio/mcp-add-existing-server-to-agent>.

A real connector must still prove:

1. Copilot Studio can store and send the bearer value in the expected header format.
2. Streamable HTTP initialization and tool discovery succeed without a proxy-specific content transformation.
3. The connector can send a dynamic actor identity, not just a static header.
4. Copilot activity records can be matched to Northern Lights audit rows and Workiva operation locations.

### NL-GOV-001, confirmation tokens are not actor-bound

Severity: Critical before multi-user rollout
Status: Open, expected under the documented single-user boundary

A write staged with actor `alice@northern-lights.local` was successfully confirmed by `bob@northern-lights.local`. The write completed, and the rich mutation audit row attributed the action to Bob. The cell was restored afterward.

Confirmation tokens must be bound to tenant ID, authenticated user ID, role, action, target, value, and expiry. Consumption must compare and delete atomically.

### NL-GOV-002, `X-NL-Actor` is attribution metadata, not identity

Severity: Critical before multi-user rollout
Status: Open

Any caller holding the shared bearer key can choose the actor header. Sanitization prevents malformed values but does not establish identity. The public edge must derive actor and tenant from a validated Entra ID JWT or a trusted connector assertion and strip any caller-supplied actor header.

### NL-GOV-003, Redis limiter alone does not make the service horizontally safe

Severity: Critical before multi-replica rollout
Status: Open

Rate buckets are process-local, but so are SQLite mappings, pending confirmations, snapshots, and the audit chain. Replacing only the limiter would still allow replica-specific authorization state, cross-replica confirmation failures, divergent mappings, and independent audit chains.

Horizontal mode needs:

- Redis for shared rate budgets and optionally short-lived confirmation state.
- PostgreSQL or another shared transactional store for tenant-scoped mappings, durable confirmations, snapshots, and audit evidence.
- Per-tenant audit serialization or a transactional append design that preserves chain order across replicas.

## Test plan to close the remaining gates

### Phase A, controlled Workiva sandbox completion

Prerequisites:

- Dedicated integration user.
- Dedicated test workspace and disposable spreadsheet.
- Separate read-only and read-write API grants created in Workiva Admin.
- Resource allowlist restricted to the disposable spreadsheet and sheet.

Tests:

1. Authenticate each grant and record effective scopes, never token values.
2. Confirm read-only grant can discover and read but receives 403 on a no-op update.
3. Confirm read-write grant can stage, confirm, poll, read back, and restore one cell.
4. Run 650 narrow reads through a purpose-built load harness, stop immediately on first 429, record status, response schema, `Retry-After`, and recovery time. Do not use write traffic for the 429 test.
5. Run two concurrent operations and verify each preserves its own initial `Retry-After` and operation URL.
6. On a blank disposable sheet only, decide whether the release requires a true 100,000-cell API write. If yes, write a reversible marker, poll, verify sampled corners and cell count, then delete or clear the sheet. Also verify 100,001 is rejected locally with zero outbound update requests.
7. Export Northern Lights audit evidence, verify the hash chain, and match operation URLs and timestamps to Workiva file history.

Exit criteria:

- Read-only write is denied.
- Read-write round trip and restoration pass.
- Actual 429 format and retry behavior are captured.
- No unknown-outcome operation remains unreconciled.
- Disposable data returns to baseline.

### Phase B, real Copilot Studio acceptance

Prerequisites:

- Copilot Studio Developer or Sandbox environment with Dataverse.
- Generative orchestration and Copilot Credits enabled.
- Public HTTPS Northern Lights endpoint.
- One test agent and one test user.

Tests:

1. Add Northern Lights as a Model Context Protocol tool using Streamable HTTP.
2. Configure API-key authentication to emit `Authorization: Bearer <NL_API_KEY>`.
3. Verify all 7 exact tool names and their schemas appear in Copilot Studio.
4. Run prompts for list, mapping sync, natural-language search, live read, staged write, confirmation, read-back, audit query, and restoration.
5. Inspect the connector's outbound request or trusted ingress logs to determine whether `X-NL-Actor` can be dynamic. If Copilot only supports a static value, require an Entra-aware proxy.
6. Test missing, malformed, and expired credentials and confirm no Workiva request is made.
7. Match one Copilot activity trace to the Northern Lights audit call row, rich write row, and Workiva operation location by correlation ID and timestamp.

Proxy decision:

- No proxy if Copilot can send the bearer token and a cryptographically trustworthy per-user identity.
- Use a proxy if authentication needs OAuth exchange, actor headers are static, or Entra claims must be translated into trusted tenant/user context.

Exit criteria:

- Real Copilot discovers and invokes tools.
- Dynamic user attribution is trustworthy.
- One confirmed write can be traced across all three systems.
- The proxy decision is recorded with evidence.

### Phase C, post-MVP multi-user and horizontal governance

Implementation sequence:

1. Introduce an authenticated principal model: `tenant_id`, `user_id`, `roles`, `subject`, and token issuer.
2. Validate Entra ID JWTs at ingress. Strip inbound `X-NL-Actor`; derive the audit actor from claims.
3. Add tenant scope to every resource query and composite uniqueness constraint.
4. Bind confirmation records to tenant, user, role, field ID, resource IDs, range, value hash, issuance time, and expiry. Store only a hash of the presented token.
5. Add RBAC permissions: `workiva.read`, `workiva.write.preview`, `workiva.write.confirm`, `mapping.sync`, `audit.read`, and `tenant.admin`.
6. Replace `*ratelimit.Limiter` with an interface and a Redis implementation keyed by tenant, Workiva workspace, and category. Use one atomic server-time algorithm, such as GCRA or a Lua token bucket.
7. Move mappings, durable pending writes, and audit state from local SQLite to shared PostgreSQL for horizontal mode. Keep SQLite only as an explicit single-replica profile.
8. Serialize per-tenant audit appends with row locks or advisory locks so replicas cannot fork the hash chain.
9. Define outage policy: fail closed for writes, confirmations, and authorization; optionally allow bounded stale reads only when policy explicitly permits it.

Acceptance tests:

- Two replicas behind a load balancer never exceed one shared workspace budget.
- 100 concurrent confirmation attempts produce exactly one mutation.
- Alice cannot consume Bob's token.
- Tenant A cannot list, search, read, write, or audit Tenant B resources.
- Read-only users cannot stage or confirm writes.
- Removing a role takes effect on the next request.
- Redis outage fails writes closed and produces a clear audit/recovery event.
- PostgreSQL failover does not fork the audit chain.
- Actor, tenant, correlation ID, and Workiva operation location remain consistent across replicas.

## Artifacts

Validation outputs are stored outside the repository under `/tmp/nl-validation/` and contain no credential values. The source fix and boundary regression test are on `test/live-validation-2026-09-15`.
