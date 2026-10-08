# Wave 3 production factory — local verification

Status: locally verified opt-in assembly; not production deployment or live
Phase 3 acceptance. No commit, push, CI, merge, or app revision was performed.

## Reviewed changes

- Durable Azure evidence storage with create-only writes, private-container
  observation, tenant/environment-bound opaque references and exact SHA/ETag
  read-back. Ambiguous uploads retain cleanup references. Unknown-version cleanup
  requires same-tenant admin, bounded SHA/length verification and observed-ETag
  conditional deletion; only actual 404 establishes absence. Nil successful GET
  body remains reconciliation-required. Fresh independent scoped review found
  zero Blocking/Important findings after EVID-01/02 fixes.
- Opt-in production transfer assembly uses the actual admitted durable restore
  fence, ready assurance, shared SQLite/audit/provider/drain gate, successfully
  configured backup/evidence delivery and matching recovery namespaces. Default
  OFF. Startup and periodic quarantine never resubmit provider work; periodic
  quarantine participates in the maintenance gate and shutdown joins it before
  closing SQLite. Fresh independent factory review found zero Blocking/Important.
- Full-Service pending-stage recovery and genuinely older backup cutoff tests
  distinguish a covered stale-row join from newer external fences. The latter
  refuses readiness, as existing production code already required. RECOVERY-03
  candidate withdrawn after source inspection and reproduction; no fix needed.
- Exact released Wave 2 source was independently reconstructed and built outside
  the public repository. Its binary creates and reopens a disposable database
  around current migrations, preserving semantic mapping columns and original
  audit hashes/rows; its append-only startup audit verifies. Opt-in normal/race
  compatibility tests passed using the coordinator's provenance-verified binary.

## Root gates on the frozen candidate

Source fingerprint before/after:
`1783cae178c46c5f76a1435b8adf01cdb32be00ce34f31a754596154715b0186`.
Calculated from sorted Go source, go.mod and go.sum file hashes.

- Empty `gofmt -l .` and `git diff --check`: PASS.
- `go test ./... -count=1`: PASS.
- `go test -race ./... -count=1`: PASS; transfer package 299.285 seconds.
- `GOARCH=386 go test ./... -count=1`: PASS.
- `GOARCH=386 go build ./...`, `go build ./...`, `go vet ./...`: PASS.
- Exact CI-pinned `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 run --timeout=5m ./...`: PASS, zero issues.
- Focused factory and evidence configuration tests normal/race: PASS.

An earlier full race attempt failed at the backup fixture's one-second timeout
covering the complete restore, including SQLite migrations, while full normal
and 386 gates ran concurrently. The exact test passed three times sequentially
before any change. Luna aligned only `blobFixture`'s timeout with production's
existing 90-second operation budget; explicit short-deadline coverage remains.
Root inspected the actual timeout scope/configuration and independently reran
race count=3 after the test-only change. The final full race then passed without
concurrent full architecture gates. No production timeout was increased.

## Acceptance boundary

Real Azure component backup/destroy/restore and evidence adapter probes passed
against private disposable namespaces using the acceptance operator credential
and synthetic local databases/principals. They are not deployed managed-identity
or authenticated public-tool/revision recovery proof. Private receipts and keys
remain outside the public repository.

Real Copilot Wave 2 prerequisite calls, approved local mapping registration and
live read correlate to the signed-in immutable bearer actor in the actual
in-container audit; the chain verifies. No Workiva cell mutation was performed.
This is not Phase 3 tool acceptance.

Remaining gates: safe runtime private-key provisioning; independently signed
disposable bundle/restore receipt; authenticated scheduled backup and receipt
retention/cadence; fresh whole-integration review; committed-only build, remote
CI/publication; immutable canonical app revision, deployed MI backup/recovery/
no-repeat/rollback; actual Phase 3 Workiva/Copilot flows and restoration. Genuine
read-only upstream-grant acceptance remains explicitly OPEN. Docker runtime
validation is unavailable, not passed. Production transfer remains disabled in
the running Azure deployment.
