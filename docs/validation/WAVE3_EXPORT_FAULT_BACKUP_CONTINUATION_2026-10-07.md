# Wave 3 continuation — coordinator review checkpoint

This is local verification, not live Azure/Workiva/Copilot acceptance or whole
Wave 3 release approval. No commit, push, merge or deployment has occurred.
Production `Transfer:nil` remains unchanged.

## Evidence export — integrated

The evidence owner implemented transfer-subject JSON/CSV export with explicit
signed route/profile associations, exact frozen revisions, current active-bundle
authorization for new keys, allowlisted persisted transfer evidence, trusted
actor identity, strict redaction and formula-safe CSV. Historical routes without
an explicit export association fail closed. Sealed replay does not rerun export
or require execution dependencies.

Coordinator review reproduced and returned fixes for stale/revoked profile
authorization and unsafe operation references (filesystem paths, arbitrary URIs
and credential-bearing URLs). The corrected regressions, raw MCP JSON/CSV calls,
storage readback/hash checks and targeted race/386 tests independently pass.
Production durable evidence storage is still absent: memory evidence storage in
acceptance fixtures is test-only and does not establish durable delivery.

## Fault windows — integrated

Each case captures a real signed/checkpointed SQLite envelope at a service
execution barrier, uploads through the actual Azure SDK against local HTTP,
destroys the local envelope before selected restore, and checks durable startup
quarantine. No handwritten crash-state row substitutes for service execution.

| Captured barrier | Required disposition |
| --- | --- |
| Local claim committed, before claim fence creation | Quarantine; no provider POST |
| Claim fence verified, before provider POST | Quarantine; no provider POST |
| Provider 202, before operation-reference persistence | Quarantine; never repeat POST |
| Operation reference persisted, before polling | Reference retained; quarantine |
| Terminal fence verified, before local final transaction | Quarantine absent complete local evidence |

Coordinator independently ran all five cases successfully. Review strengthened
checked destruction/absence and exact fence-object set, bytes and ETag equality
through restore and final replay attempts. Same-key token-free attempts return
exact `idempotency_in_progress` with no response projection; new-key old-token
attempts fail. Provider reads, POSTs and polls do not increase after restore.

## Production backup — reviewed, integrated and locally verified

The reviewed backup candidate adds an authenticated synchronous non-MCP endpoint,
shared admission drain, signed terminal checkpoint, fence high-water and durable
Blob envelope upload/readback receipt. Private-container policy and initial SDK
upload/restore tests independently pass locally.

Coordinator independently verified actual disk-source and sidecar destruction,
empty successful staging, service-Date capture through the real fence adapter,
receipt-selected restore and real scan, retained staging/no receipt on HTTP
partial-upload/read-back failures, bounded cancellation/concurrent triggers and
configuration rejection. Source review returned missing checkpoint trust in the
actual startup selector: the reproduced `audit-terminal is missing` failure is
fixed by explicit operator-retained checkpoint key/ID. Actual startup restore now
checks observable private-container policy before destination writes; public or
unobservable policy is rejected. The real SDK test reaches this actual selector,
not only its lower-level helper. Passing local HTTP tests is not live acceptance.

Reviewed deltas were imported byte-identically from isolated candidates. Backup
configuration and receipt mapping are documented in
`WAVE3_BACKUP_MAINTENANCE_LOCAL.md`. No Blocking/Important finding remains within
this continuation's reviewed source; whole-integration release approval remains
separate. Final frozen combined gates all pass.

## Preservation and gate scope

Original index binary-patch SHA remains
`90d8ea068f8fd5bd10f16a767910ce5f91bd61b4491b7673de836d3d7b163699`.
Existing staged/unstaged/untracked integration work was preserved; only reviewed
owned deltas were imported. No applied migration rewrite is authorized.

Normal full suite, full race suite, full GOARCH=386 suite, amd64/386 builds, vet
and pinned lint (zero issues) passed for the evidence/fault checkpoint before
backup import. Evidence/backup lint issues were returned to the owner and fixed
without behavior changes or suppressions; isolated backup lint is zero issues.
Final frozen combined gates independently PASS:

- `go test ./... -count=1`
- `go test -race ./... -count=1` (transfer package 283.253 seconds)
- `GOARCH=386 go test ./... -count=1`
- `go build ./...` and `GOARCH=386 go build ./...`
- `go vet ./...`
- Exact pinned `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --timeout=5m ./...`: zero issues.
- Empty `gofmt -l .`, `git diff --check`, unchanged original index and migration bytes.

Go source/module fingerprint before and after gates is identical:
`a72a41c589963afee01c3ba7e09e0da638e3eb71af2089373d6802d343bf5b8a`.
Only documentation and external snapshot artifacts changed afterward.

Recoverable final snapshot:
`/home/agapantha/northern-lights-workspace/evidence/wave3-export-backup-fault-final-20261007/`.
It records the tracked binary patch, original index patch, all 65 untracked files,
canonical planning/handoff members and independently verified hashes. No prior
snapshot was removed. This is not committed-checkout or remote CI evidence.
