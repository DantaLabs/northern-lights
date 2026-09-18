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

Still open from that pass: controlled provider-side 429 validation, a real
Copilot Studio connector/activity trace, a genuinely read-only Workiva grant,
and all multi-user/horizontal governance. The multi-user negative baseline also
proved that one actor can consume another actor's confirmation token under the
current documented single-user design.

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

3. **Copilot Studio auth header support**: bearer token middleware assumes
   Copilot connectors can send a static Authorization header. Verify against
   a real Copilot Studio MCP connector; may need a proxy shim.

   External acceptance also requires verifying tool discovery, actor-header
   propagation, and matching Copilot/Workiva activity traces with real access.

4. **Rate limits are process-local** (adversarial review ISSUE-017): the
   token-bucket limiter lives in the server process, so multiple replicas
   each get their own budget while Workiva enforces workspace-wide limits.
   Single-replica deployments are unaffected. Horizontal scaling needs a
   shared limiter (e.g. Redis) or per-replica budget division. Documented
   here as a known limitation.

5. **Multi-user governance**: add authenticated identities, user/tenant-scoped
   mappings and confirmation tokens, RBAC, and a shared limiter before serving
   multiple users, tenants, or replicas. Live characterization proved that a
   token staged by one actor can currently be consumed by another actor.

6. **MCP sessions are process-local**: the Go SDK's default stateful session
   map is not shared across replicas. Choose stateless mode or shared,
   tenant-bound session storage before horizontal deployment, and validate
   cross-replica continuation without sticky routing.

## Closed by adversarial review pass

1. **401 token refresh retry** (internal/workiva/client.go): `Do` now
   invalidates the cached token and retries once with a freshly fetched token
   after a 401 response. Repeated 401 responses return `APIError{401}`.

## Disposition of the 2026-09-08 adversarial review (ADVERSARIAL_REVIEW.md)

Fixed and verified by tests:

- ISSUE-001 demo values endpoint returned data on parse error -> 400 now
- ISSUE-002 PATCH/POST/PUT no longer retried on 5xx/transport (429 only)
- ISSUE-003 write audit entries record the X-NL-Actor identity
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
- ISSUE-018 actor header sanitized (trim, 128-char cap, control chars)
- ISSUE-019 CI runs golangci-lint
- ISSUE-020 repository published: github.com/DantaLabs/northern-lights

Rejected after verification:

- ISSUE-008 (write proceeds when before-read fails): FALSE POSITIVE.
  `executeWrite` aborts when `readFieldValue` errors; the failing-test
  claim does not reproduce. `gridValue` ignores its error only after a
  successful read, yielding an empty before value for empty cells, which
  is correct behavior.
