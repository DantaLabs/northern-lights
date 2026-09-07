# Backlog

Issues found during independent review, pending work:

1. **401 token refresh retry** (internal/workiva/client.go): `Do` fetches the
   token once before the retry loop and never retries on 401. If Workiva
   expires a token early (clock skew, revocation), requests fail until the
   cached token naturally expires. Fix: on 401, invalidate the cached token,
   refetch, and retry once.

2. **Verify editCells/SheetUpdate payload against live API**: the write
   payload shape (`editCells` array of `{range, value}`) is built from the
   2026-01-01 docs, not from a live workspace. First run against a real
   Workiva sandbox must validate the exact schema, including whether
   `editCells` entries nest the range differently.

3. **Documents API out of scope for MVP**: Workiva Documents (prose editing)
   intentionally deferred to v0.2 per plan.

4. **Copilot Studio auth header support**: bearer token middleware assumes
   Copilot connectors can send a static Authorization header. Verify against
   a real Copilot Studio MCP connector during Task 12; may need a proxy
   shim.
