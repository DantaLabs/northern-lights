# Adversarial Review — Northern Lights (Sep 8, 2026)

Reviewed by: Hermes Agent (full codebase read, 50+ files, go test -race, go vet)
Scope: security, correctness, compliance, concurrency, operational robustness
Known backlog items (BACKLOG.md) excluded unless I found additional depth.

---

## ISSUE-001: Copy-paste bug — valuesResponse error path returns data instead of error

**File:** `internal/workiva/demo_client.go:142-148`
**Severity:** BUG (low in prod, masks demo-mode test failures)
**Labels:** bug, demo-mode

```go
rng, err := A1ToRange(cellRange)
if err != nil {
    return demoJSONResponse(mustJSON(ValuesOnlyGrid(t.fixture, rng)))  // ← BUG: uses zero-value rng
}
return demoJSONResponse(mustJSON(ValuesOnlyGrid(t.fixture, rng)))
```

When `A1ToRange` errors, the error path returns `ValuesOnlyGrid` with the zero-value Range instead of `demoBadRequest(...)`. Both the error and success paths are identical. An invalid A1 range silently returns empty data instead of an error.

**Fix:** Replace the error path with `return demoBadRequest("invalid range: " + cellRange)`.

---

## ISSUE-002: Retry on non-idempotent writes causes duplicate operations

**File:** `internal/workiva/client.go:98-145`
**Severity:** HIGH — correctness + data integrity
**Labels:** security, write-path

`Do()` retries on transport errors for ALL HTTP methods including PATCH. If a PATCH request reaches Workiva (202 accepted) but the TCP connection drops before the response is read, the retry sends a second PATCH, creating duplicate async operations and duplicate cell writes. Since writes are async ops (202 + poll), there's no idempotency key or deduplication.

**Fix:** Use `net/http/httptrace` to detect whether `WroteRequest` fired. If the request was fully sent, do NOT retry non-idempotent methods. Alternatively, only retry GET/HEAD/PUT/PATCH-with-idempotency-key.

---

## ISSUE-003: Actor identity hardcoded to "copilot" in write audit entries

**File:** `internal/mcpserver/tools/update_field.go:237`
**Severity:** MEDIUM — compliance (EU AI Act Art. 12 actor attribution)
**Labels:** compliance, audit

`auditWrite()` sets `Actor: "copilot"` regardless of the `X-NL-Actor` header. The middleware `recordToolCall` captures the real actor from the header, but the rich before/after mutation entry always says "copilot". Two audit entries per mutation have inconsistent actor attribution. Same issue in `sync_mapping.go:181`.

**Fix:** Pass the actor from the request context (set by middleware) into the tool handlers, or extract it from the MCP request headers in the tool.

---

## ISSUE-004: Hash chain excludes workiva_op_url

**File:** `internal/audit/log.go:109-112`
**Severity:** MEDIUM — audit integrity
**Labels:** compliance, security

`entryHash()` hashes: prevHash + ts + actor + tool + action + target + before + after. The `workiva_op_url` field is NOT included. An attacker who can modify the SQLite file can change the Workiva operation URL (the cross-reference to Workiva's own audit trail) without breaking the hash chain.

**Fix:** Add `workiva_op_url` to the hash input. This is a breaking change for existing chains — migrate by versioning the hash algorithm (e.g., `v2:` prefix).

---

## ISSUE-005: Rate limiter bypassed on retries (amplifies 429 storms)

**File:** `internal/workiva/client.go:70-74 vs 127-134`
**Severity:** MEDIUM — reliability under load
**Labels:** rate-limit, reliability

The rate limiter `Wait()` is called once before the retry loop. Retries within the loop (on 429, 5xx, transport error) do NOT re-acquire limiter budget. When the workspace-wide 429 triggers a retry, the retried request bypasses the limiter entirely, potentially amplifying load against the shared workspace limit — exactly the opposite of backoff intent.

**Fix:** Move `limiter.Wait()` inside the retry loop, or re-acquire on each attempt.

---

## ISSUE-006: Silent audit failure violates EU AI Act Art. 12

**File:** `internal/mcpserver/server.go:111-120`
**Severity:** HIGH — compliance
**Labels:** compliance, audit

```go
if _, err := log.Append(ctx, audit.Entry{...}); err != nil {
    _ = err  // silently swallowed
}
```

Tool calls execute and succeed even when the audit entry fails. For Art. 12 record-keeping, a failed audit append MUST be surfaced. The comment says "auditing must not break tool execution" — but this creates a gap where mutations happen without any record.

**Fix:** Log the error to stderr at minimum. For write/mutation tools, consider returning a warning in the tool response. Add a configurable `AuditFailurePolicy` (warn/fail) so operators choose strictness.

---

## ISSUE-007: LIKE pattern injection in SearchFields

**File:** `internal/mapping/store.go:270`
**Severity:** LOW — data leak via broad matches
**Labels:** security, search

`like := "%" + query + "%"` — user-controlled query is directly interpolated into SQL LIKE patterns without escaping `%` and `_` wildcards. A query of `%` matches all fields; `_` matches any single character. Not SQL injection (parameterized), but pattern injection causes unintended broad matches that could leak field names/values to an LLM.

**Fix:** Escape `%` → `[%]` and `_` → `[_]` in the query before building the LIKE pattern. Or use `strings.NewReplacer("%", "\\%", "_", "\\_")` with `ESCAPE '\'`.

---

## ISSUE-008: Write proceeds when before-value read fails

**File:** `internal/mcpserver/tools/update_field.go:182-184, 190-197`
**Severity:** MEDIUM — compliance gap
**Labels:** compliance, audit

In `executeWrite()`, if `readFieldValue()` fails (API down, network error), the function does NOT return an error. The write proceeds (line 190). The audit entry's `before_json` is empty. For Art. 12 compliance, the "before" state is essential evidence.

**Fix:** Either fail the write when the before-value cannot be read, or explicitly record `"before": "UNAVAILABLE"` in the audit JSON with an additional flag.

---

## ISSUE-009: confirm_token value mismatch is undocumented behavior

**File:** `internal/mcpserver/tools/update_field.go:164-170`
**Severity:** LOW — confusion risk for LLM callers
**Labels:** docs, mcp-tools

The confirmed write checks `pending.FieldName != in.Name` but does NOT check `pending.Value == in.Value`. The tool uses the staged value (`pending.Value`) and ignores the re-presented `in.Value`. This is correct (prevents TOCTOU between staging and confirmation) but undocumented — an LLM presenting a different value in the confirmation call will see the staged value written, not the one it just passed.

**Fix:** Document in tool description: "The value written is the one from the staging call. The value parameter in the confirmation call is validated against the staged value and rejected if different."

---

## ISSUE-010: Token fetch holds mutex during network call

**File:** `internal/workiva/auth.go:68-80`
**Severity:** MEDIUM — availability
**Labels:** concurrency, reliability

`ClientCredentialsToken()` holds `p.mu` during the entire `fetch()` network call. If the Workiva token endpoint hangs, ALL API calls serialize behind the mutex. Production uses `http.Client{Timeout: 60s}`, bounding the hang, but `http.DefaultClient` (no timeout) is used in tests and when `nil` is passed.

**Fix:** Use `golang.org/x/sync/singleflight` instead of a mutex: first caller fetches, others wait for the result, mutex is released immediately after the fetch starts.

---

## ISSUE-011: Retry-After header has no upper bound

**File:** `internal/workiva/client.go:176-187`
**Severity:** LOW — resource exhaustion
**Labels:** reliability

`retryAfterDelay()` parses Retry-After without capping. A malformed header of "999999" causes ~11.5 days of sleep per retry. Context cancellation saves the caller, but long-lived goroutine leaks are possible if ctx isn't properly bounded.

**Fix:** Cap at e.g. `120 * time.Second`.

---

## ISSUE-012: Double rate-limiting on operation polling

**File:** `internal/workiva/operations.go:66 + internal/workiva/client.go:70-74`
**Severity:** LOW — performance
**Labels:** rate-limit, performance

`WaitOperation()` sleeps for `retryAfterDelay()` (default 1s) THEN the next `Do()` call acquires the CategoryOperations rate limiter (1 req/sec). Each poll waits ~2s instead of ~1s. The delay and limiter are redundant.

**Fix:** Remove the explicit sleep in `WaitOperation()` and let the rate limiter handle spacing, OR skip the operations limiter when a Retry-After sleep was just applied.

---

## ISSUE-013: No pagination safety limit in GetSheetData

**File:** `internal/workiva/sheets.go:62-86`
**Severity:** LOW — resource exhaustion
**Labels:** reliability, defensive-coding

The pagination loop follows `@nextLink` until empty with no maximum page count. A circular or malformed pagination response causes infinite reads until context timeout. With 50k cells/page, even 100 pages is 5M cells — more than enough for any real use case.

**Fix:** Add a `maxPages` constant (e.g., 1000) and break with an error if exceeded.

---

## ISSUE-014: Demo mode API token is "demo"

**File:** `cmd/workiva-mcp/main.go:110`
**Severity:** LOW — accidental exposure
**Labels:** security, demo-mode

`NL_DEMO_MODE=true` sets the bearer token to "demo". If accidentally enabled in production (env var typo), the API is accessible with a trivially guessable token. The startup message logs this, but a single log line is easy to miss.

**Fix:** Require `NL_DEMO_TOKEN` to be explicitly set, or use a random token generated at startup and printed to stderr.

---

## ISSUE-015: Pending writes table never auto-cleaned

**File:** `internal/mapping/store.go:374-427`
**Severity:** LOW — storage leak
**Labels:** housekeeping

`pending_writes` grows without bound. Staged writes that are never confirmed sit forever. No TTL cleanup job or periodic purge exists.

**Fix:** Add a `PurgeExpiredPendingWrites(ctx, maxAge)` method and call it periodically (e.g., on startup, or via a background goroutine every 10 minutes).

---

## ISSUE-016: No migration version tracking

**File:** `internal/mapping/store.go:22-55`
**Severity:** LOW — future risk
**Labels:** database, maintainability

Migrations are idempotent via `CREATE TABLE IF NOT EXISTS`. Future non-additive migrations (ALTER TABLE, column renames) have no way to track which migrations are applied. A `schema_version` table is needed before the first ALTER.

**Fix:** Add a `schema_version` table with a single row tracking the current migration number. Check on `Open()` and apply pending migrations.

---

## ISSUE-017: Rate limits are process-local (undocumented)

**File:** `internal/ratelimit/limiter.go`
**Severity:** LOW — documentation gap
**Labels:** docs, rate-limit

Multiple server instances against the same Workiva workspace each have independent rate limiters. Combined request rate can exceed workspace-wide limits (600 read/min, 60 write/min). This is not documented anywhere.

**Fix:** Add a warning to README/CONFIG docs: "Rate limits are per-process. Running multiple instances against the same workspace can exceed workspace-wide limits."

---

## ISSUE-018: Actor header is spoofable

**File:** `internal/mcpserver/server.go:103-107`
**Severity:** MEDIUM — audit integrity
**Labels:** security, audit

`X-NL-Actor` is trusted without validation. Any client with the bearer token can claim any actor identity (e.g., "admin@company.com") in audit entries. The header is set by the Copilot connector — but if the connector is misconfigured or the token leaks, actor attribution is meaningless.

**Fix:** Document that actor identity is "best-effort" and depends on the connector's integrity. For stronger guarantees, validate actor against a whitelist or use the connector's own auth identity.

---

## ISSUE-019: CI missing golangci-lint

**File:** `.github/workflows/ci.yml`
**Severity:** LOW — code quality
**Labels:** ci, quality

CI runs `go vet`, `gofmt`, and `go test -race` but not `golangci-lint` (which catches security patterns, error handling, and more). The plan (Task 0) mentioned it but the CI config doesn't include it.

**Fix:** Add `golangci/golangci-lint-action@v6` step.

---

## ISSUE-020: No GitHub remote — repo not pushable

**Severity:** MEDIUM — blocks community readiness
**Labels:** infra, community

No git remote exists. CI config, README badges, CONTRIBUTING, SECURITY docs all exist locally but are invisible. The plan calls for `DantaLabs/northern-lights` public repo.

**Fix:** Create the GitHub repo and push.

---

## ISSUE-021: Demo client valuesResponse uses error value on error path

(Same as ISSUE-001, called out separately for the VALUES endpoint specifically — the fix should also audit the sheetdataResponse for similar patterns. sheetdataResponse at line 120-122 correctly returns demoBadRequest on error.)

---

## Summary by severity

| Severity | Count | Issues |
|----------|-------|--------|
| HIGH     | 3     | 002 (double-write), 006 (silent audit failure), 008 (write without before) |
| MEDIUM   | 6     | 003 (hardcoded actor), 004 (hash excludes op_url), 005 (rate limiter bypass), 010 (mutex during fetch), 018 (spoofable actor), 020 (no remote) |
| LOW      | 12    | 001 (demo bug), 007 (LIKE injection), 009 (undocumented confirm behavior), 011 (uncapped Retry-After), 012 (double rate-limit), 013 (no pagination limit), 014 (demo token), 015 (pending writes leak), 016 (no migration tracking), 017 (process-local limits), 019 (CI gaps), 021 (dup of 001) |

**Deduped total: 20 unique issues** (3 HIGH, 6 MEDIUM, 11 LOW)
