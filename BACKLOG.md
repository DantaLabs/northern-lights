# Backlog

## Open

1. **Verify editCells/SheetUpdate payload against live API**: the write
   payload shape (`editCells` array of `{range, value}`) is built from the
   2026-01-01 docs, not from a live workspace. First run against a real
   Workiva sandbox must validate the exact schema, including whether
   `editCells` entries nest the range differently.

2. **Documents API out of scope for MVP**: Workiva Documents (prose editing)
   intentionally deferred to v0.2.

3. **Copilot Studio auth header support**: bearer token middleware assumes
   Copilot connectors can send a static Authorization header. Verify against
   a real Copilot Studio MCP connector; may need a proxy shim.

4. **Rate limits are process-local** (adversarial review ISSUE-017): the
   token-bucket limiter lives in the server process, so multiple replicas
   each get their own budget while Workiva enforces workspace-wide limits.
   Single-replica deployments are unaffected. Horizontal scaling needs a
   shared limiter (e.g. Redis) or per-replica budget division. Documented
   here as a known limitation.

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
- ISSUE-011 Retry-After capped at 30s
- ISSUE-012 operation polling paced by the limiter; extra sleep only when
  Retry-After exceeds the limiter interval
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
