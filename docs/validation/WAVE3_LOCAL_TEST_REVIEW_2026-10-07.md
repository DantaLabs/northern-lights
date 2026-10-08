# Phase 3 Wave 3 local test review — 2026-10-07

Verdict: the bounded fence-scan/startup test pass is accepted locally. This is
not full Wave 3 acceptance or production transfer enablement. The user selected
“Finish local tests and review first.” No commit, push, merge, deployment, or
live provider mutation was performed. Existing staging was preserved.

## Reviewed changes

- Corrected `TestRestoreScanSection9Table`: only the inventory-failure case now
  injects a scan error. Missing body/path/metadata cases reach real validation.
- Added a matching machine-verified claim/terminal success case and duplicate
  inventory rejection; normalization errors leave transfer state, audit and
  quarantine rows unchanged while blocking readiness.
- Added exact page/object/byte boundaries, LIST timeout and negative-limit
  coverage alongside the existing over-limit and GET-timeout tests.
- Removed production transfer construction after clean startup restore
  admission. `buildServer` explicitly supplies `Transfer: nil` until recovery
  and release prerequisites pass.
- Added a signed-bundle, trusted-principal, raw MCP startup regression. It
  tests actual server wiring after a mocked clean admission, returning
  `unavailable`, `transfer_service_unavailable`, and
  `no_mutation_submitted=true`. It does not prove real Blob restore/join.
- Corrected `scripts/verify-mcp.sh` to require all 13 registered tools and
  corrected startup documentation to state that admission does not enable
  production transfer.

Two GPT-6 Luna workers performed bounded work. The coordinator inspected the
actual files and independently reran the gates. One worker's initial shell
inspection was interrupted by an aborted approval; its subsequent bounded
test pass completed. No valid pre-fix RED run was captured for the startup
production change; the prior premature constructor wiring was directly
inspected. Initial fixture/compile failures during editing are not gate results.

## Independent evidence

All final commands passed in the stable candidate:

- `go test ./... -count=1`
- `go test -race ./... -count=1`
- `GOARCH=386 go test ./... -count=1`
- `GOARCH=386 go build ./...`
- `go vet ./...` and `go build ./...`
- `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --timeout=5m ./...`
  (the exact CI pin; zero issues)
- `git diff --check` and empty `gofmt -l .`

The coordinator ran the smoke script against a disposable local demo server:
initialize/notification, JSON responses, 13-tool discovery and malformed-key
401 all passed. The server was stopped. Raw MCP/schema acceptance tests passed.
Migration v1-v13 bytes matched `origin/main`; v14-v15 matched the original
staged integration baseline. No migration was changed by this pass.

The original historical evaluation probes were preserved. Their initial rerun
failed because confirm requests omit now-required ingress digests. An isolated
copy added those prerequisites and strengthened route denial to require both
an error and zero POSTs. The known read-back fixture then correctly failed with
`readback_mismatch`: source 2 and post-write target 1 are unequal. Removing
only that contradictory source override in the isolated copy produced equal
values; all 23 probes passed without changing candidate equality semantics.

A temporary Git index exported candidate tree
`fff6882aa943cdc46be215fe1fc5cb7c345fc199`, including every new source file,
without altering the original index. The exported checkout passed
`go test ./... -count=1` and `go build -buildvcs=false ./...`; the flag is needed
because the archive has no VCS metadata. Its initial default build failed only
while obtaining VCS status. This is candidate packaging evidence, not a
committed-checkout or remote CI claim. A scoped high-confidence secret-pattern
scan of the tracked patch and new files passed; it is not a comprehensive
credential audit.

Prior Codex completion records from October 6 confirm that earlier fence/join
HTTP tests were blocked by loopback restrictions, and a later unrestricted full
test suite passed. They did not record full release acceptance.

## Remaining integration/release gates

Transfer evidence export still rejects the transfer subject. Authenticated
backup triggering, authoritative private Blob selection/publication and durable
evidence delivery remain open. A combined Blob backup → destroy → restore →
fence join test was open at this checkpoint. The subsequent bounded combined local test
is accepted in `WAVE3_COMBINED_RECOVERY_LOCAL_2026-10-07.md`; full fault-window
and live destruction/recovery acceptance remain open. Separate unit suites or
mocked startup admission are not that proof. Provider relationship refresh
remains spike-gated.

Full integrated semantic approval, live Azure/Copilot/Workiva acceptance,
committed-checkout verification, branch CI and main merge remain unverified.
Do not describe this local test checkpoint as accepted Wave 3 or Phase 3 GA.
