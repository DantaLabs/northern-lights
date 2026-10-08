# Wave 3 recovery binding fixes — 2026-10-07

Verdict: bounded RECOVERY-01/02 remediation accepted locally; all local gates pass.
This supersedes the failing-tree result in
`WAVE3_RECOVERY_MATRIX_TESTS_2026-10-07.md`.
The user explicitly authorized RECOVERY-01/02 remediation and handoff/backlog
updates. No commit, push, merge, deployment or live provider mutation is included.

## Fixes and independent review

- RECOVERY-01: restore claim validation uses independently persisted confirmation
  evidence, not the stage row's different ingress digests. Exactly one
  tenant/transfer/tool/action-scoped reservation must exist; it must belong to
  the transfer actor, be sealed with the successful confirmation status, and
  match both confirmation digests. Records from other actors are counted before
  actor validation, so contradictory duplicates cannot be hidden by filtering.
  Missing, ambiguous, wrongly scoped or mismatched evidence quarantines.
- RECOVERY-02: `canonicalTypedValue` uses assurance's sorted canonical JSON
  encoding before intent storage and fence hashing. Completion persistence uses
  the same encoding. Boolean false and numeric metadata remain explicit.
  Existing signed fences are never rewritten or normalized for admission.
  Legacy struct-order terminal readback hashes disagreeing with persisted bytes
  continue to quarantine. Missing legacy completion evidence has no stage-digest
  fallback; nonterminal uncertainty never becomes permission to submit or poll.
- Production delta versus the saved failing checkpoint is limited to
  `internal/transfer/service.go`, `restore_scan.go`, `restore_scan_binding.go`.
  Tests/fixtures changed in `service_test.go`, `b1_red_test.go`, and
  `restore_scan_red_test.go`. The two requested recovery integration tests remain
  unchanged from the failing checkpoint; their distinct stage/confirm keys and
  post-restore no-repeat assertions were not weakened to pass.
- One GPT-6 Luna implemented the dependent fixes sequentially. The coordinator
  inspected the actual production/test deltas against the recoverable failing
  snapshot, reviewed resource/error handling and strict bindings, and separately
  reran the focused suite. No Blocking or Important finding remains within this
  bounded remediation. This is not a fresh whole-integration approval.

## Executable evidence

The worker reproduced the original lifecycle RED
`claim_binding_mismatch;terminal_readback_mismatch`, then observed reservation
negative regressions and the canonical encoder test fail before implementation.
The coordinator's independent verbose focused run passes:

- Real Service stage/confirm, one provider write and polling, signed checkpointed
  Blob backup, destruction of disposable source/envelope artifacts, selected
  restore and clean fence admission.
- Exact token-free sealed confirmation replay, new-key/old-token refusal, zero
  further provider reads/writes/polls through restore and replay, and no fence
  requests during replay. Restored signed-bundle trust is bootstrapped first.
- All six real-Blob matrix cases: missing claim, orphan, contradictory terminal,
  pending-stage/token-free projection, audit-failure quarantine rollback and
  signed-minimum rollback rejection/existing-destination preservation.
- Eleven confirmation-evidence negatives: missing, duplicate under another
  actor, wrong actor, key digest, request digest, state, status, tenant, tool,
  action and transfer reference. Each persists the expected quarantine reason.
- Existing Section 9 scan table, legacy terminal hash rejection and canonical
  encoding positives for false and numeric metadata.

## Final local gate

Coordinator independently ran the following fail-fast sequence on the frozen
final production/test tree. Every command passed (exit 0):

```sh
go test ./... -count=1
go test -race ./... -count=1
GOARCH=386 go test ./... -count=1
GOARCH=386 go build ./...
go vet ./...
go build ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --timeout=5m ./...
git diff --check
test -z "$(gofmt -l .)"
```

Pinned lint reports `0 issues.` Full transfer race tests completed in 259.689s.
Only documentation changed after this full gate; diff/format checks are rerun
after those edits. No production/test edits followed the gate.

Applied migration v1–v13 bytes match `origin/main`; v14–v15 match the preserved
staged integration baseline. No migration changes occurred in this pass.
The staged patch SHA-256 remains
`90d8ea068f8fd5bd10f16a767910ce5f91bd61b4491b7673de836d3d7b163699`.
A scoped high-confidence credential-pattern scan of tracked/untracked candidate
source found no matches; this is not a comprehensive credential audit.
Recoverable snapshot location:
`/home/agapantha/northern-lights-workspace/evidence/wave3-recovery-binding-fixes-20261007/`.
The manifest records tracked-patch/untracked-archive hashes and member count;
the original Git index remains unchanged. Prior failing checkpoint is retained.

## Remaining acceptance boundary

These are local tests using an instrumented provider and fake Azure HTTP service,
not live Workiva/Azure acceptance. Pending-stage coverage is Store projection,
not pending full-Service confirmation execution. The matrix is not every crash
window or older released-binary compatibility.

Production transfer remains nil/disabled. Transfer evidence export, authenticated
backup trigger, authoritative private Blob selection/evidence delivery, remaining
fault-window tests, provider relationship spike, live Azure/Workiva/Copilot,
fresh whole-integration review, committed-checkout verification and remote CI
remain open. Evaluate this acceptance position with the user before delivery.
