# Backlog

## Open

0. **Official Workiva MCP readiness, Northern Lights remains primary**:
   preserve the existing seven Northern Lights tools and REST implementation as
   the default. The provider boundary in `internal/workivaprovider` permits read
   and write capabilities to be replaced independently without changing public
   tool schemas, mapping, confirmation, or audit behavior. Do not enable an
   official-MCP route until its live contracts, identity, residency, errors,
   audit correlation, rollback, and operational advantage pass the gates in
   [`docs/WORKIVA_PROVIDER_STRATEGY.md`](docs/WORKIVA_PROVIDER_STRATEGY.md).
   The authenticated EU gateway exposed its tool catalog, but `list_workspaces`
   returned `access denied: this user is not enabled for the MCP gateway`.
   Workiva must enable the test user before official-provider live acceptance.

Validation evidence and the remaining acceptance plan are recorded in
[`docs/validation/LIVE_VALIDATION_2026-09-15.md`](docs/validation/LIVE_VALIDATION_2026-09-15.md).
Commit `26b136f` was not previously live-validated. The 2026-09-15 pass found
and fixed a live spreadsheet-discovery timestamp contract mismatch, then
validated EU discovery, reads, mapping sync, confirmed writes, operation
polling, read-back, restoration, the local 100,000-cell boundary, public HTTPS
MCP transport, bearer rejection, actor propagation, and audit-chain integrity.

Still open from that pass: controlled provider-side 429 validation, a genuinely
read-only Workiva grant, and all multi-user/horizontal governance. The
multi-user negative baseline also proved that one actor can consume another
actor's confirmation token under the current documented single-user design.

The Copilot Studio integration and auth milestone ran on 2026-09-17 (see item
3). Four of its five checks passed; the second-user check is pending.

1. **Live Workiva sandbox validation, partial**: live EU OAuth, discovery,
   narrow reads, mapping sync, confirmed writes, operation polling, read-back,
   restoration, and cap rejection are validated. Remaining: validate a truly
   read-only grant, capture a provider-side 429 and recovery, and decide whether
   a destructive exact-100,000-cell API write is required. The 2026-01-01
   request and response contracts, including field paths, scalar values, range
   bounds, nested updates, and operation delays, are covered by
   official-document-shaped tests.
2. **Documents API out of scope for MVP**: Workiva Documents (prose editing)
   intentionally deferred to v0.2.

3. **Copilot Studio integration, partial**: validated 2026-09-17 in the
   Developer environment `dev-as-d9867869` with a custom connector, agent
   "Northern Lights test" (standard harness) and maker credentials, over a
   Cloudflare Quick Tunnel. Setup rules learned on the first run (standard
   agent, `nl-actor` as a formula, maker credentials, connector creator builds
   the agent) are in
   [`docs/copilot-studio-test-runbook.md`](docs/copilot-studio-test-runbook.md).

   Passed:
   - Authenticated MCP connector, discovery of all 7 tools and schemas. The
     schema test found nothing to fix.
   - Key flow: Copilot Studio sent the key both with and without `Bearer `.
     The server accepts both and rejects other shapes with 401 before any
     Workiva call. No proxy shim is needed.
   - Write test on `scope_2_energy_kwh` (800, 12345, 800): 7 `nl_audit_id`
     values matched audit records; the two confirmed writes carry Workiva
     operations `a78a0b68-d397-40f7-a2a5-367338dc8e4a` and
     `6c87e2d0-ae03-4f40-866b-6aa646583189`. Audit chain intact.
   - Trace matching at time level: Application Insights showed 15 successful
     Northern Lights calls (16:05 to 16:17 UTC) that line up by time, tool and
     user with the audit log.

   Remaining:
   - **Second-user actor check**: `nl-actor` carried `System.User.Email` on
     every call, but only one user was tested. Developer environments are
     owner-only, so this moves to the Sandbox phase.
   - **Exact trace matching (optional)**: Application Insights does not store
     tool outputs, so `nl_audit_id` never appears there. Record `traceparent`
     (already visible in the debug log) on the audit row to join Copilot
     activity to audit records exactly instead of by time.
   - **Stable public ingress**: the Quick Tunnel was deregistered by Cloudflare
     after under 8 hours ("Tunnel not found"). The agent then answered from
     other knowledge sources without reaching the server. Use a named tunnel or
     real hosting for the Sandbox phase, and turn off web search and general
     knowledge on test agents so tool outages are visible.
   - **Unexplained update calls**: two `workiva_update_field` calls at 16:14
     UTC (`b692c4e9-fc71-47a7-a1b4-e08dc05a5b3f`,
     `362ebfeb-425c-417f-b493-21ed997ad830`) wrote nothing between the write
     and the restore. Check their results in the Copilot activity view.

4. **Rate limits are process-local** (adversarial review ISSUE-017): the
   token-bucket limiter lives in the server process, so multiple replicas
   each get their own budget while Workiva enforces workspace-wide limits.
   Single-replica deployments are unaffected. Horizontal scaling needs a
   shared limiter (e.g. Redis) or per-replica budget division. Documented
   here as a known limitation.

5. **Multi-user governance**: add authenticated identities, user/tenant-scoped
   mappings and confirmation tokens, RBAC, and a shared limiter before serving
   multiple users, tenants, or replicas. Live characterization proved that a
   token staged by one actor can currently be consumed by another actor. The
   `nl-actor` header is a test-only identity asserted by the key holder; for
   customers the actor must come from the OAuth (Entra ID) token.

6. **MCP sessions are process-local**: the Go SDK's default stateful session
   map is not shared across replicas. Choose stateless mode or shared,
   tenant-bound session storage before horizontal deployment, and validate
   cross-replica continuation without sticky routing.

7. **Rotate the test API key**: the current `NL_API_KEY` value was found in
   plain text in four local agent session logs from 2026-09-14 and 15 (Codex
   and Hermes), outside `deployments/.env`. Rotate it after the Copilot Studio
   tests and remove those log copies.

## Closed by adversarial review pass

1. **401 token refresh retry** (internal/workiva/client.go): `Do` now
   invalidates the cached token and retries once with a freshly fetched token
   after a 401 response. Repeated 401 responses return `APIError{401}`.

## Closed by the Copilot Studio milestone (2026-09-17)

- Redacted `/mcp` diagnostics behind `NL_DEBUG_HEADERS=1`: JSON-RPC method,
  status and error text, User-Agent, header names, key scheme, length and
  8-character SHA-256 fingerprint, actor and tracing headers. No characters
  of the key and never the request body. (An earlier version logged the first
  7 characters, which exposed the start of raw keys; fixed.)
- `scripts/test-session.sh start|status|requests|stop` runs a full local test
  session: server, Quick Tunnel, local connector copy, verification, and a
  redacted request table.
- Authorization accepts `Bearer <key>` (any case) or the raw key; empty,
  double-`Bearer` and other schemes return 401 before the MCP handler.
- Actor read from `nl-actor`, falling back to `X-NL-Actor`; trimmed, 256-char
  cap, control characters rejected. Writes (preview and confirm) are refused
  without an actor; reads record `unknown`.
- `nl_audit_id` in every tool result (structured content and first text
  block), stored on the audit row as `audit_id` and included in the hash chain.
- Schema test fails on `$ref`, array-valued `type`, numeric exclusive bounds,
  or a tool count other than 7; warns on enums.
- `POST /mcp` answers `application/json` (no SSE); `GET /mcp` returns 405.
- Connector OpenAPI file, `traces.kql`, `scripts/verify-mcp.sh` and the test
  runbook added under `deployments/copilot-studio/`, `scripts/` and `docs/`.

## Disposition of the 2026-09-08 adversarial review (ADVERSARIAL_REVIEW.md)

Fixed and verified by tests:

- ISSUE-001 demo values endpoint returned data on parse error -> 400 now
- ISSUE-002 PATCH/POST/PUT no longer retried on 5xx/transport (429 only)
- ISSUE-003 write audit entries record the caller identity (now `nl-actor`,
  falling back to `X-NL-Actor`)
- ISSUE-004 workiva_op_url is part of the audit hash chain (both paths)
- ISSUE-005 rate limiter Wait runs once per retry attempt
- ISSUE-006 audit append failures logged loudly; write tools refused when
  the audit log is unavailable
- ISSUE-007 LIKE wildcards in search queries are escaped
- ISSUE-009 confirm with a value different from the staged one fails
- ISSUE-010 token fetch deduplicated with singleflight, no lock held
  across the network call
- ISSUE-011 Retry-After honors valid non-negative integer seconds exactly;
  malformed and duration-overflow values fall back safely
- ISSUE-012 operation polling paced by the limiter; explicit sleep is used
  only when Retry-After exceeds the limiter interval
- Cache hits require every bounded mapped cell and assemble in row-major A1
  order; unbounded mappings fall back to a live read
- Confirmation tokens bind field ID, name, spreadsheet, sheet, and range;
  changed mappings fail closed and require restaging
- ISSUE-013 sheetdata pagination capped at 50 pages
- ISSUE-014 demo mode generates a random API token per run
- ISSUE-015 expired pending writes cleaned at startup
- ISSUE-016 schema_migrations table tracks applied versions per app
- ISSUE-018 actor header sanitized (trim, control chars; cap raised to 256
  chars on 2026-09-17)
- ISSUE-019 CI runs golangci-lint
- ISSUE-020 repository published: github.com/DantaLabs/northern-lights

Rejected after verification:

- ISSUE-008 (write proceeds when before-read fails): FALSE POSITIVE.
  `executeWrite` aborts when `readFieldValue` errors; the failing-test
  claim does not reproduce. `gridValue` ignores its error only after a
  successful read, yielding an empty before value for empty cells, which
  is correct behavior.
