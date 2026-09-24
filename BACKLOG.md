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

The Azure enterprise deployment milestone ran on 2026-09-18 (see item 8). It
replaced the disposable Quick Tunnel with a stable Azure Container Apps
endpoint and passed its live write round-trip. This entry closes the "stable
public ingress" gap from item 3.

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

5. **Multi-user governance, Wave 1 implemented locally; live and later waves
   remain open**: the code now has explicit `api_key` and single-tenant `entra`
   modes, OIDC discovery/cached JWKS validation, immutable `tid`/`oid`
   principals, delegated-scope and explicit app-role/client authorization, the
   six-permission vocabulary, per-tool enforcement, trusted audit actors, and
   fail-closed 401/403 paths. API-key mode remains the default. This has not
   been validated with a live Entra app registration or Copilot OAuth
   connection. The Wave 1 v2 contract requires the API manifest's
   `requestedAccessTokenVersion` to be `2` and
   `NL_ENTRA_AUDIENCE="<API-client-ID-GUID>"`. Clients request scopes as
   `api://<API-client-ID>/<scope>`, while the server exactly validates the GUID
   that Entra emits in the v2 token's `aud`; it does not normalize the scope URI
   into an audience. Tenant and audience UUIDs are canonicalized, while the
   authority must already use the canonical tenant path. The Wave 1 app-only
   deployment contract is implemented and documented: the API manifest must
   add `idtyp` to `optionalClaims.accessToken` without discarding existing
   optional claims, and role/client policy variables stay unset for
   delegated-only deployments. App-only access must remain disabled until a
   real token issued for the API proves `idtyp=app`; that is a live acceptance
   gate and has not been completed here. Wave 2 must still tenant-scope
   mappings, snapshots, audit rows, and confirmations and bind confirmation
   tokens to the authenticated principal. Live characterization proved that
   the current persisted token model allows a token staged by one actor to be
   consumed by another, so Entra mode is not yet a claim of complete multi-user
   isolation. Wave 3 must still add shared state and a shared limiter before
   multiple replicas.

6. **Stateful MCP sessions are process-local**: Wave 1 Entra mode is
   intentionally stateless and does not issue or rely on `Mcp-Session-Id`;
   API-key compatibility mode retains the Go SDK's stateful session map. Do not
   enable stateful Entra sessions until shared, tenant-bound session storage
   passes cross-principal and cross-replica acceptance without sticky routing.

   The final independent Wave 1 security review found no Blocking or Important
   findings. Its remaining non-blocking hardening items are intentionally open:
   add a committed JSON-RPC batch authorization regression test or reject batch
   arrays at the HTTP pre-check (the SDK middleware already denies every
   unauthorized batch element before tool execution and audits it, but returns
   HTTP 200 with a JSON-RPC error); add inbound `/mcp` rate limiting or JWKS
   refresh debouncing to limit pre-auth key-fetch amplification; document that
   `tenant.admin` is reserved and currently grants no tool capability; strip any
   configured custom actor header in Entra mode as defense in depth; and ensure
   deployments never set `MCPGODEBUG=allowsessionsinstateless=1`. These do not
   weaken the clean Wave 1 authorization verdict, but remain explicit follow-up
   work before broad production exposure.

7. **Rotate the test API key, partial**: `NL_API_KEY` was rotated 2026-09-22.
   New value lives in `deployments/.env`, Key Vault `kv-nl-70cff1d0`, and the
   Container App secret (revision `--rot1754`); the old key now returns 401 and
   the full verify suite passes with the new key. Still open: the old value
   remains in plain text in four local agent session logs from 2026-09-14/15
   (Codex and Hermes) — remove those log copies. `NL_DEBUG_HEADERS=1` is still
   enabled on the live Container App; disable it after the Sandbox acceptance
   run (it is useful for the two-user trace). Workiva client secret rotation
   is a separate Workiva-side step (regenerate the API grant secret, update
   Key Vault + Container App secret).

8. **Azure enterprise deployment, partial** (2026-09-18, previously
   unrecorded): provisioned in subscription `Azure subscription 1`, resource
   group `WorkivaTest`, Japan East: Container Registry
   `northernlights70cff1d0`, Container Apps environment
   `cae-northern-lights-test`, single-replica Container App
   `ca-northern-lights` (image `northern-lights:test-fc065f5`, system-assigned
   managed identity), Key Vault `kv-nl-70cff1d0` holding `nl-api-key`,
   `nl-workiva-client-id`, `nl-workiva-client-secret` (mirrored as Container
   App secrets), storage account `stnl70cff1d0`, Log Analytics workspace,
   Application Insights `WorkivaMCP`. Stable endpoint:
   `https://ca-northern-lights.braveriver-d67a1a27.japaneast.azurecontainerapps.io/mcp`.
   Passed: `/healthz` and `/readyz` 200, unauthenticated `/mcp` 401, 7-tool
   discovery, live Workiva discovery, and a full write round-trip
   (`0 -> 987654 -> 0`) with two Workiva operation IDs and intact audit chain.
   Re-verified healthy 2026-09-22. Remaining: run the full Copilot Studio
   acceptance against this endpoint from the new Sandbox, reconcile the two
   unexplained `workiva_update_field` calls from 2026-09-17, then rotate
   credentials and disable debug logging (item 7).

9. **Power Platform Sandbox created** (2026-09-22, operator-side):
   `northern-lights-sandbox` (`e6e63f00-6b6a-eef0-a8a1-aa33522d53b0`), type
   Sandbox, region Japan, Dataverse Yes, state Ready, agent
   `Workiva_Sandbox_Test` created inside it. Billing plan `TestWorkiva` now
   lists `Northern-Lights-Sandbox` as a target. Both Microsoft users are in
   the environment and the agent was shared. Remaining: fix second-user actor
   propagation and complete acceptance.

10. **Copilot Sandbox two-user read test, closed with root cause**
    (2026-09-22): Sigmundus@ (agent owner) called the server with
    `nl-actor=Sigmundus@SicMundusInc.onmicrosoft.com`, HTTP 200. Sigmundo@
    (second user) received the Workiva spreadsheet list through the same
    shared maker connection, HTTP 200, but `nl-actor` was empty on every
    request. Root cause identified by the operator: sigmundo@ has no
    Microsoft 365 license, hence no mailbox, so `System.User.Email` evaluates
    empty in Power Fx. Verdict: shared-connectivity PASS, two-user
    attribution EXPLAINED-NOT-PROVEN. For unlicensed users the tool input
    needs a fallback identity expression (for example `System.User.Id` or
    `System.User.PrincipalName`) or Entra-backed principals (item 5). No
    production multi-user claim is made.

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
