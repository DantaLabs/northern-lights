# Combined local Blob recovery review — 2026-10-07

Scope: signed Blob backup → destruction of disposable local source/backup
artifacts → real restore → Azure fence scan/join. This is local fake-HTTP
composition evidence, not full Wave 3/Wave 5 or live Azure acceptance.

## Implementation and verified behavior

- `internal/transfer/startup_recovery_integration_test.go` calls real
  `UploadEnvelope`, `CaptureHighWater`, and `RestoreSelectedEnvelopeAndScan`.
  Neither inventory nor admission is replaced by a canned Ready result.
- Canonical create-only claim/terminal objects bind a seeded machine-verified
  transfer, operation reference and uncached readback. The backup includes a
  signed terminal audit checkpoint and externally captured fence high-water.
- The test closes all source handles and removes exact disposable source,
  envelope and SQLite sidecar paths before restoring into a new empty path.
  No production, live provider or user database is destroyed.
- Matching fences admit restore with both Ready flags true and no findings.
  The signed database hash, audit sequence and old terminal state are checked.
- A missing terminal fence blocks readiness and freezes the old transfer into
  reconciliation. Quarantine and audit counts survive independent DB reopen.
- Canonically invalid bytes and a fence newer than the signed high-water fail
  closed through the actual Azure adapter.
- Original valid token/time claim attempts are refused for both terminal and
  quarantined restored rows. This proves a Store CAS barrier, not the complete
  provider submission/Confirm replay lifecycle. No provider write is modeled.
- The only production change is the validated
  `assurance.NewAzureBackupTransportWithClient` wrapper. Caller-owned client
  authentication is explicit; default production constructors and startup
  transfer disablement are unchanged.

## Review and execution

Two GPT-6 Luna agents designed/implemented and inspected the test. The
coordinator independently read the actual files and reproduced failures.
Initial failures included absent checkpoint, compile errors, and incomplete
disposable-envelope cleanup. Review also corrected the fake service Date,
claim token/time, closed source handles, and DB reopen assertions.

Both delegated implementation/recovery continuations stalled. Their edits
were preserved; the coordinator applied only test-side sidecar cleanup and a
tagged-switch lint correction. The first full suite/race/386/vet/build pass
succeeded, but pinned lint rejected QF1003. Those intermediate results are
not the final gate. The final stable-tree gate passed independently:

- `go test ./... -count=1`
- `go test -race ./... -count=1`
- `GOARCH=386 go test ./... -count=1` and `GOARCH=386 go build ./...`
- `go vet ./...` and `go build ./...`
- `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --timeout=5m ./...`
  (zero issues)
- `git diff --check` and empty `gofmt -l .`

Coordinator verdict: ACCEPT for this bounded local composition test, with no
open Blocking/Important finding in that scope. The second Luna's preliminary
review supplied material fixture findings, but its final review continuation
also stalled and was interrupted without a final verdict. The coordinator's
actual-file inspection and independent reruns substitute for that unfinished
review; no fresh full-integration approval is claimed. Both workers are stopped.
A scoped high-confidence secret-pattern scan of the two code files had no
matches; this is not a comprehensive credential audit. No migration changed.

## Remaining scope

This fixture seeds complete terminal evidence rather than executing every
provider/crash-window arrow. Missing-claim/orphan/contradictory windows retain
their separate local test coverage. Pending-confirmation lifecycle, every
fault-window combined test, compatible rollback and live revision destruction
remain broader acceptance work. Authenticated backup triggering, authoritative
private Blob selection/publication, transfer evidence export/delivery, full
integrated semantic approval, live Workiva/Copilot/Azure, committed checkout
and remote CI remain open. Production transfer remains disabled. No commit,
push, merge or deployment was performed.
