# Requested local recovery tests — 2026-10-07

Historical test-only checkpoint. The user subsequently authorized the two
production fixes; continuation and current verification are recorded in
[`WAVE3_RECOVERY_BINDING_FIXES_2026-10-07.md`](WAVE3_RECOVERY_BINDING_FIXES_2026-10-07.md).
The failure evidence below is retained, not the current acceptance verdict.

Verdict: recovery fault matrix accepted locally; full test gate REJECTED by a
new real-service restore regression. No production changes, live provider
requests, commit, push, merge or deployment occurred.

## Scope and implementation

- `provider_recovery_integration_test.go` stages and confirms through the real
  transfer Service and a usable instrumented provider. It observes provider
  reads, one write and polling before a signed, checkpointed Blob backup.
  Source handles and exact disposable source/envelope/sidecar paths are removed
  before real `RestoreSelectedEnvelopeAndScan`.
- The test uses distinct stage and confirm ingress digests. It includes exact
  prebackup provider-counter comparisons, original signed-bundle trust bootstrap,
  identical token-free sealed replay, new-key/old-token refusal with the expected
  claim error, and zero additional fence requests during confirmation replay.
  These post-admission assertions are NOT yet executed: clean admission fails
  first on the two findings below.
- `recovery_matrix_integration_test.go` covers missing claims, fence-only orphans
  and canonical contradictory terminals through real Blob backup/destruction/
  restore/join. Contradictory local and external terminal digests match so the
  case reaches semantic contradiction validation rather than a digest error.
- Exact quarantine/audit counts survive DB reopen; old quarantined IDs cannot
  be reused. Orphan quarantine does not manufacture a transfer or change the
  valid existing terminal row.
- Pending staged state, expiry, row version, row count and confirmation digest
  survive restore and token-free Store replay. This is a Store projection test,
  not a pending-confirmation full Service execution test.
- A signed fixture's failing startup-audit trigger forces transactional rollback:
  original state/version remain and no quarantine, reconciliation, transfer
  quarantine event or rich audit row is partially committed.
- An independently trusted minimum audit sequence rejects a stale signed backup
  without installing a destination. Separate destination-exists rejection
  preserves sentinel bytes. Older released-binary rollback is not tested.
- `service_test.go` only adds a test-helper trust-key return path while preserving
  the existing helper API. No production startup or transfer wiring changed.

One fresh GPT-6 Luna implemented the lifecycle regression. The reused second
worker stalled without landing a matrix file and was interrupted; the productive
worker completed the matrix sequentially. The coordinator inspected actual files,
reproduced the failures and routed test-quality findings back for correction.
No fabricated production RED or fixture alteration is treated as a product fix.

## Important findings requiring a fix decision

1. **RECOVERY-01 — claim scope mismatch.** Confirm's claim fence binds confirm
   ingress digests, but restore compares them against the separately persisted
   stage digests. Legitimate distinct requests produce `claim_binding_mismatch`.
   Fix must bind to independently persisted confirm evidence, not weaken the
   checks or reuse stage and confirm keys in the fixture.
2. **RECOVERY-02 — readback encoding mismatch.** The terminal fence hashes the
   service's struct-ordered typed JSON, while completion persistence rewrites
   readback bytes with canonical key ordering. Restore hashes stored bytes and
   produces `terminal_readback_mismatch`. Fix must establish a consistent encoding
   and an explicit fail-closed decision for legacy fences; no signed fence or
   persisted test data should be rewritten to manufacture a passing restore.

Both findings are independently reproduced in the new test; the process remains
not ready and durably quarantines the otherwise completed transfer. The user was
asked whether to remediate production in this pass or retain failing regressions.
No production remediation was inferred from the test-only request.

## Independent verification

Final coordinator stable-tree results:

- `go test ./... -count=1` FAILS only on
  `TestServiceConfirmationReplaySurvivesAzureBackupDestroyRestore`, with exactly
  `claim_binding_mismatch;terminal_readback_mismatch`. All other packages passed.
- `go test -race ./internal/transfer -run '^TestRealBlobRecoveryMatrix' -count=1`
  PASSES both matrix tests and all six subcases.
- `GOARCH=386 go test ./internal/transfer -run '^TestRealBlobRecoveryMatrix' -count=1`
  PASSES; `GOARCH=386 go build ./...` PASSES.
- `go vet ./...`, `go build ./...` PASS.
- Exact pinned `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --timeout=5m ./...`
  PASSES with zero issues. Initial deferred-close warnings were corrected, not
  suppressed.
- `git diff --check` and empty `gofmt -l .` PASS. Scoped high-confidence secret
  pattern scan of the new tests found no matches; not a comprehensive audit.

No full-suite race or full-suite 386 PASS is claimed for this failing tree.
The earlier all-green checkpoint predates these new regressions and must not
be cited as the current tree's gate result. Full provider replay/crash-window,
live Azure/Workiva/Copilot, older-binary compatibility, full integration review,
production backup/evidence wiring and committed-checkout/remote CI remain open.
