# Backlog

## Current release track — 2026-10-09 (authoritative bounded acceptance)

The detailed dated narratives below are preserved as history and are not the
current acceptance verdict. In particular, old audit4 selectors, blocked
cutover/preservation-pending statements, and connection-repair concerns are
superseded by the current evidence below. The current matrix is
[`docs/validation/WAVE3_CURRENT_ACCEPTANCE_2026-10-08.md`](docs/validation/WAVE3_CURRENT_ACCEPTANCE_2026-10-08.md).

- **Source and publication:** feature source remains
  `b0b8ca2d9253be289c75da090037ef2ca1a41503`; local integrated gates and
  independent source reviews passed, and draft PR11 run `37753676804` passed.
  Documentation publication was requested; the feature code/image remains
  `b0b8ca2`. Publication SHA/CI status must be verified separately in the
  operator handoff and Git. `main` remains unchanged unless that readback says
  otherwise; no merge is authorized or claimed here.
- **Canonical app cutover:** revision
  `ca-northern-lights-wave2--p3-b0b8ca2-maint-a54` cold-started from the final
  selected backup with the same `b0b8ca2` source and image. `/readyz` and the
  canonical route returned HTTP 200; the app managed identity remained
  `SystemAssigned`. Readback showed one active revision and eight older
  revisions inactive at zero replicas. Captured app body SHA-256
  `66443c02beadeff4d46c4b4a134d37127f1d55e8217a8f9bcae15be26b163250` matched
  the normalized template except for the authorized revision suffix and five
  final restore selectors. The final selector is envelope
  `5c8017c6ebb1c93ee1d6a4b139b89bf7` / audit high-water 54 / checkpoint
  `d1158818-928a-4c68-b88b-29873ced2de9`; the former envelope `0547...0419` /
  audit4 selector is historical and stale. Final deployment/readback receipt
  SHA-256: `1ec4172c818ce504eeb95bf61f564495e78a61f742b53d9b2a65ac8ffd1a289c`.
- **Final backup recovery:** final manual execution
  `job-nl-phase3-backup-ennjmo4` succeeded and sealed the selected envelope
  above. Receipt SHA-256:
  `9e8453b6b2ecc4c9a57a4f76fc5aae40ecc563f8dd9de84431816b88ce922938`.
  The exact envelope was independently SDK-restored and fence-scanned twice;
  latest proof SHA-256 is
  `a7e9e59a16ae38c83b83887578392d91fc3d8dc8234ef2793a97dee81245d765`.
  Signed v2 bootstrap, audit chain/high-water/checkpoint, all six sealed domain
  operations and rich links, JSON/CSV pins, active transfer/readbacks,
  reconciliation/visual evidence, exact Phase 2 restoration audit, and full
  immutable original-confirm replay with zero provider/fence calls passed.
  This is one exact backup/restore and restart sequence, not a cadence, RPO, or
  failure-recovery guarantee.
- **Backup schedule:** the job was temporarily changed to Manual to create the
  final stable backup. Root independently read back the restored schedule and
  verified it deep-equals the original `*/5 * * * *` UTC configuration, with
  job identity, image, and settings unchanged. This confirms configuration
  restoration, not a new scheduled execution or cadence/RPO guarantee; no new
  automatic execution is claimed.
- **Copilot and Workiva:** prior bounded acceptance remains recorded: all 13
  connector tools were discovered; audited reads/materializations and JSON/CSV
  storage checks passed; one human-approved transfer was confirmed once,
  reconciled without repeat, visibly acknowledged, and separately restored
  once to B4=0 with final uncached B2:B4 `1200/800/0`. No new Copilot invocation
  or Workiva call was made after the final maintenance restart, per the stop
  instruction. The evidence export audit remains limited:
  `completeness=unknown`, checkpoint missing, terminal anchor unverified, and
  final-row deletion not detectable; this is not complete audit-history proof.
- **Open and waived boundaries:** Workiva read-only-grant behavior remains
  OPEN. Genuine second-tenant/shared-store behavior remains WAIVED/UNVERIFIED.
  Preservation of the prior Wave 2 sandbox database was explicitly WAIVED, NOT
  PASSED; the nine-row audit archive is not a database backup or migration.
- **Disposition:** bounded Wave 3 delivery acceptance PASS for the reviewed
  source, final backup recovery, and maintenance revision startup/cutover, with
  the open and waived exceptions above. This is not unconditional full-gate,
  merge, or closure acceptance. Do not infer a commit SHA, push, merge, or
  release closure from this acceptance record.

## Historical backlog narratives (superseded for current status)

## Resumed pre-commit review — 2026-10-08

LATEST: MCP-NIL01/GRAPH-ERR01 independently RED-reproduced, narrow worker fixes
reviewed/imported byte-exact; focused race PASS. All three complete bounded
pre-commit reviews PASS with zero Blocking/Important. Root full sequential
normal/race/386/vet/amd64+386 builds/pinned2.13.2 lint0/format/diff PASS on
508ff68b3f2468a6adc506a4f186630f1260e9ffdca9201d02464316c84abaf8.
Fresh tracked-only normal/vet/amd64+386 builds PASS; actual released-Wave2
compatibility race PASS3.734s. Feature commit is eligible, not yet published. Stricter
secret scan's sole hit is an independently confirmed synthetic test credential.
Fresh debug marker probes still have empty output, not verified shell access.
Old DB-preservation authority remains unchanged; explicit user direction pending.

Publication held. All 138 reviewed scope paths explicitly staged; original
staged patch retained in private snapshots. Tracked-only normal/vet/amd64+386
builds and actual released-Wave2 compatibility race test PASS on fe30 source.
Transfer/provider pre-commit subset PASS. MCP-NIL01 (discover nil dependency)
and GRAPH-ERR01 (swallowed resource lookup DB error) independently RED-reproduced
by root; two Luna workers correcting narrow scopes before re-review/full gates.
Additional domain audit-gate interpretation is under requirement review.
No commit/push/CI/merge/deployment or wave closure yet.

Ordinary old-revision healthcheck exec succeeded; auxiliary debug-container
access remains broken. The portal shell error does not establish global exec
failure. Old whole-DB preservation is still required before maintenance cutover.
Deployed identity/job schedule/real Phase3 Copilot acceptance remain open.

## Authoritative latest — 2026-10-08

FINAL LOCAL: all root sequential full normal/race/386/vet/amd64+386 builds/
pinned2.13.2 lint0/format/diff PASS on
`fe30fc471347ceeafb41f39f895de457980e67efa30772d82f07b6f47baa6c27`;
fresh-context independent source review zero Blocking/Important. Root full race
transfer300.078s, assurance227.114s, tools185.872s. Fresh remote main unchanged.

Actual SAME selected Azure backup restored-copy continuation PASS7.664s:
ready/scan-ready true/findings0, normal signed v2 Bootstrap, audit verification,
exact definition/transfer pins, token-free same-key sealed replay and fresh-key
no-token refusal, provider_calls=0/fence_calls=0. Root full573-line harness review,
DIAG02 explicit-zero counters, independently safe race3.698s before live opt-in.
Receipt `/tmp/nl-live-selected-restore-diagnostic-4018546733/restored-copy-verification.json`.
No original source DB access/new backup/export/provider/blob mutations. Operator
Azure credential + synthetic trusted context, not real deployed Entra identity.
Backup-only removable legal hold remains applied/read back; evidence unchanged.

CUTOVER BLOCKED: user portal execution also failed ClusterExecFailure500; no safe
old whole-DB archive. Keep old Wave2 running. Audit-only export is not full DB
preservation; no inferred permission to discard state. Phase3 deployment/job
identity/scheduled HTTP/Copilot/CI/main/publication/closure still open. No commit
or push; original staged patch unchanged. Historical checkpoints follow.

LATEST: FENCE-ETAG01 reviewed/imported; root SDK RED-to-GREEN focused race2.229s.
Go fingerprint `fe30fc471347ceeafb41f39f895de457980e67efa30772d82f07b6f47baa6c27`;
root final full gates ACTIVE4063, not yet PASS. Same retained actual Azure backup
restore/fence admission PASS7.826s (ready/scan-ready true, findings0). Private
`/tmp/nl-live-selected-restore-diagnostic-494663593`. No new backup/export/blob
mutation/provider/sourceDBaccess. External post-restore Bootstrap/pin/token-free
replay harness follow-up delegated; not yet accepted. Hold remains applied.
Historical failures below now superseded; old whole-DB/deployed acceptance open.

- EXPSCHEMA02 imported/reviewed; full root normal/race/386/vet/both builds/
  pinned2.13.2 lint0/format/diff PASS on Go fingerprint
  `e47329c7982c1a49224a873f8624b2dec176f5b0ecd6d25db64cba1012c35223`.
- Actual MCP JSON/CSV export and fresh Azure hash/size reads PASS. Signed selected
  backup sealed, envelope `724aee62c0ebbbf9f7f5f92ef2962acd`; fence admission FAIL.
  Private receipts `/tmp/nl-live-transfer-recovery-continue-846676530`.
- Read-only diagnostic identifies FENCE-ETAG01: Azure list unquoted ETag versus
  download quoted same strong value. Luna correcting strict comparison and SDK
  fixtures. No repeat exports/backups/transfer/provider work.
- BKISO01/BKP-CFG01 physical evidence/recovery-container isolation imported after
  root RED, fresh scoped review, byte comparison and focused race PASS16.630s.
  Current Go fingerprint `7b05ad7f64a165c198cc784d024dc76e1a2a2abe825dc6186a04d395522ad010`;
  final full gate pending fence fix. Backup/fence paths create-only, no delete;
  enabled evidence cannot share configured recovery containers.
- Conditional backup-only removable legal hold applied/read back PASS: exact
  tag `nlphase3acceptance`, protected append false, private container. Evidence
  container independently unchanged/no hold. No destructive enforcement probe.
- Old Wave2 whole-DB preservation OPEN: both debug access attempts failed; audit
  JSONL is not a DB backup. Preserve running old revision before maintenance.
- No Phase 3 deployment/job live identity acceptance, commit/push/CI/merge/closure.
  Historical statuses below are superseded by this section.

## Live continuation — restoration PASS; delivery/recovery OPEN (2026-10-08)

Latest: reviewed runtime-key startup imported; root integrated focused race PASS.
Current source fingerprint `2cf57ab48f71d1ea5736dbb300acc9c6f3d8302c3e1b76e4313fc77f98d9194c`;
full combined release gates pending. Dedicated backup HTTP caller delivered in
isolation, fresh independent review before import. Recovery harness corrected
REC01–05; root safe race fixture PASS, independent review before live opt-in.
No additional Workiva writes or app/job deployment/publication. Older progress
statements below are history, not current worker/import status.

SUPERSEDING: dedicated backup caller independently reviewed/imported, focused
integrated race PASS; source now `ad8d654125ed4783948910cd7ae1b8950998985898f78168d96afd39f5b4250b`.
Root vet and both architecture builds PASS; final full gates pending EXPCTX01.
Live recovery sealed a pre-provision Azure backup and activated signed v2, then
JSON export failed because manifest readback drops trusted request context.
EXPCTX01 narrow TDD fix delegated. Retained receipts/public key under
`/tmp/nl-live-transfer-recovery-3043695691`. Do not rerun v1 provisioning: source
already v2. No Workiva provider calls. Existing-v2 continuation required after fix.

SUPERSEDING: EXPCTX01 corrected/reviewed/imported; root full sequential gates
PASS on `a9523bfef8e09a6812ea6a417ea139353268cc996362a72938e25f872a413a89`.
Live v2 continuation then found EXPSCHEMA02: sealed JSON export rejected by MCP
storage_ref max256, actual Azure refs ~400 (adapter cap512). No automatic retry;
new schema/golden/transport TDD correction and sealed-export diagnostic assigned.
Receipt `/tmp/nl-live-transfer-recovery-continue-205452411/export-json-outcome.json`.
User approved brief single-replica maintenance cutover. Old whole-DB backup and
backup/fence deletion protection still open; audit-only archive not a DB backup.
No deployment, job execution, commit or push. Final gates must cover schema fix.

SUPERSEDING: restoration PASS. Root reviewed the corrected narrow harness,
independently passed its race fixture (exact parser failure with zero POSTs,
verified identity overrides supplied actor header), then ran one fresh audited
restoration. B4 is numeric zero via uncached readback, B2 unchanged 1200, distinct
operation, one POST/poll, matching tenant/object rich audit and chain Verify.
Receipts retained `/tmp/nl-live-transfer-restore-3536558786`; original copy not
repeated. The initial failed attempt below remains historical, not current state.

Signed disposable seed/CLI + actual Azure production backup PASS after local
SEED05 principal regression. Retained source/receipt:
`evidence/production-seed-probe-1691347969`; independent backup public trust and
separate bundle key preserved. Operator Azure credential/synthetic context, not
deployed identity. Fresh live app remains Wave2/0000005.
Key startup is in final scoped re-review; HTTP-only dedicated job client is
delegated in isolation. Original transfer recovery needs explicit operator
reprovisioning because its harness failed before retaining the bundle public
key; no bypass of signatures, repeated copy, or claim of original-key recovery.

- Root's single authorized live transfer copied numeric B2=1200 to B4 and
  returned API-verified machine success. The separately audited Phase 2 restore
  call errored without an operation reference; no retry was issued. Fresh
  read confirms B4 is still numeric 1200. Restore to numeric zero before closure.
- External harness incorrectly used a qualified `Sheet1!B4` Phase 2 mapping;
  legacy write parsing expects `B4` and rejects before provider submission.
  Require exact local zero-POST reproduction plus narrow reviewed fresh
  restoration, not another full copy run or untracked REST write.
- Private forensic SQLite and receipts retained at
  `/tmp/nl-live-transfer-probe-2331302076`; live evidence/backup stages not reached.
  Local frozen source gates remain valid only for their previous source scope.
- Three current Luna tasks: isolated key admission, seed harness completion,
  and bounded restoration preparation. Root is independent reviewer/executor.
  Phase 3 Azure deployment, dedicated job execution and final release remain open.

## Production delivery and live acceptance — IN PROGRESS (2026-10-07)

User expanded scope beyond local tests: production delivery AND real live
acceptance are required before closure. Luna's durable Azure Blob evidence
adapter, fail-closed private-policy/tenant/reference checks, opt-in production
wiring and post-upload cleanup/replay regressions are independently inspected
and imported. Root focused tests PASS; full integrated gates are not yet complete.

- REAL Azure component recovery probe PASS: production backup operation against
  a disposable synthetic local DB, signed private Blob seal/readback, checked
  source destruction, actual startup restore/fence admission and checkpoint.
  This does not prove a deployed revision restart, live identity, or old-transfer
  no-repeat behavior. Exact private receipts/probes remain outside the public repo.
- Private test containers and container-only Blob data roles for the existing app
  managed identity and acceptance operator are provisioned/read back. The existing
  Wave 2 app/ephemeral DB is unchanged. Health/readiness/unauthenticated-boundary
  checks PASS; no Phase 3 deployment or Git publication occurred.
- Real Azure evidence-adapter probe PASS: synthetic create-only artifact and
  exact hash/ETag/bytes readback through a fresh adapter; cross-tenant/malformed
  opaque-reference denial. Operator credential, not deployed MI/tool proof.
- Full normal and 386 suites/builds, amd64 build, vet and pinned lint PASS before
  factory import. Subsequent full race FAILED at a one-second whole-restore test
  fixture deadline under concurrent gates. Exact race count=3 passed sequentially;
  reviewed test-only fixture now matches production's existing 90-second budget,
  explicit deadline test remains, after-fix count=3 PASS. No production timeout
  change. Final frozen full race PASS (transfer 299.285s); sequential full normal/
  386 suites, both builds, vet, format/diff and pinned lint v2.13.2 zero issues PASS.
  Frozen fingerprint unchanged; see WAVE3_PRODUCTION_FACTORY_LOCAL_2026-10-07.md.
- Workiva pinned-grant OAuth, discovery and narrow read PASS with no mutations.
  User approved Testing MCP/Sheet1 for bounded tests with every changed cell
  restored, and approved the existing grant while read-only-grant acceptance stays
  OPEN. Effective grant remains read/write, not a proven read-only grant.
- Browser signed in; fresh directory, billing/environment/bot and Invoker binding
  readbacks PASS. Old Maker connector is already disabled. Real Wave 2 browser
  calls reach backend, but B4 read is refused because tenant-owned mappings are
  absent. In-container audit export now proves actual immutable signed-in actor
  attribution; audit verify PASS. Normal Copilot mapping sync registered three
  approved local fields and subsequent read produced matching successful read
  audit. No Workiva cell writes. Browser final response still streaming/blank.
  User approved report/evidence permissions on sandbox Write; keep Read/admin
  separate. Defer this policy to Phase 3 revision: old binary rejects new names.
  Phase 3 acceptance remains open.
- Pending full-Service recovery and exact older released-binary tests are delegated
  complete and independently pass normal/race, including root rebuilt exact older
  source. Luna's completed factory main/helper/tests independently read/imported;
  focused command normal/race PASS. Actual admitted durable fence/shared gate,
  refusal, enabled assembly and cancel/join lifecycle covered. Opt-in defaults OFF;
  no deployment. Fresh scoped factory review PASS, zero Blocking/Important.
  Runtime key design read; user approved version-pinned KV/app-MI/private-file and
  exclusive signed disposable seed-and-backup path. Isolated Luna TDD key worker
  active, not imported. Native Luna completing external live harness after two
  incomplete CLI preparations; no opt-in enabled, no live transfer performed.
- EVID-01/02 fixes imported and independently focused/race PASS: admin-only
  verified ambiguous-object cleanup; genuine 404-only absence; nil successful body
  stays pending. Fresh independent scoped re-review PASS, zero Blocking/Important.
- RECOVERY-03 candidate withdrawn: direct source read/reproduction confirms
  newer-than-backup fences fail readiness closed, not silently skip. Regression
  covers actual backup-before-confirm timeline; no production fix required.
- User approved dedicated backup-job HTTP caller, separate from Copilot. Identity
  created, Application-only backup role and optional idtyp claim added/read back;
  exact role assignment created. No signing-key/KV/Blob access granted to caller.
  Role/client runtime policy prepared outside public repo, not deployed/applied.
  User reports completed sync/read results visible; bridge final reply still blank.
- Dedicated backup role assignment read-back PASS; caller Azure RBAC list empty.
  Approved backup/checkpoint key generated privately and stored in a new pinned
  KV version; only metadata/ID output. App MI existing KV read/ACR/two-container
  roles verified. No runtime key retrieval, job/deployment or new-source import.
  Three Luna tasks active: isolated key startup TDD and external live/seed harness.
  Root found/routed six live-harness safety/preflight corrections before any run.
- Frozen pre-provisioning local boundary saved outside public repo: full tracked
  binary patch, unchanged original index, all77untracked files count/byte verified,
  canonical planning archive. See external factory-frozen snapshot manifest.
- Remaining: frozen full gates/release review, safe runtime key provisioning and
  signed deployment bundle, committed-only review/gates, remote CI, immutable
  Azure revision destroy/restore/no-repeat/rollback, Workiva and Copilot acceptance.

## Latest Wave 3 continuation — local implementation/tests COMPLETE

Three Luna workstreams completed local transfer evidence export, opt-in production
backup wiring and all five exact fault-window tests. Root independently reviewed
and imported the deltas. Final full normal/race/386 suites, builds, vet and pinned
lint (zero issues), format/diff checks all pass on the frozen source tree. See
[`WAVE3_EXPORT_FAULT_BACKUP_CONTINUATION_2026-10-07.md`](docs/validation/WAVE3_EXPORT_FAULT_BACKUP_CONTINUATION_2026-10-07.md).

- Evidence export: independent raw MCP JSON/CSV, profile revocation, operation
  reference safety, targeted race/386 and integrated three-package tests pass.
  Production durable evidence storage remains open; memory storage is test-only.
- Backup wiring: authenticated same-tenant admin trigger, full shared drain,
  signed checkpoint/service-Date high-water, create-only Blob sealing, failure/
  cancellation/concurrent-trigger handling and receipt-selected actual startup
  restore/private-container admission pass locally. Source DB/sidecars/staging
  are destroyed before restore. Production configuration is opt-in, not deployed.
- Fault windows: all five exact windows pass with durable quarantine, checked
  local-envelope absence, exact replay refusal and unchanged fence bytes/ETags.
- Blocked native backup/fault workers were replaced by normal Hermes Luna
  sessions, then preserved stalled partial results were finished by the productive
  native Luna. No worker remains active on source. No sandbox or credential-directory
  changes were made. Original integration staging remains byte-identical.
- No commit, push, merge, deployment, or live Azure acceptance. Production
  transfer remains disabled. The accepted baseline below is historical evidence
  for that scope only. Live gates, durable evidence delivery, older-binary/pending
  full-Service compatibility and fresh whole-integration release review stay open.

## Current Phase 3 Wave 3 checkpoint — 2026-10-07

The requested **local recovery test pass is complete**, independently reviewed
and verified by the coordinator. RECOVERY-01/02 are fixed: restore claims bind
to the actual sealed confirmation reservation, and typed intent/readback/fence
hashing uses one canonical encoding. Missing, ambiguous, wrongly scoped and
mismatched evidence, including legacy readback-hash disagreement, still
quarantines. See
[`WAVE3_RECOVERY_BINDING_FIXES_2026-10-07.md`](docs/validation/WAVE3_RECOVERY_BINDING_FIXES_2026-10-07.md).

- Done locally: real Service/provider lifecycle through signed Blob backup,
  disposable destruction, restore/fence admission, token-free sealed replay,
  old-token rejection with a new key, and zero post-restore provider work.
  All six recovery fault-matrix cases and eleven confirmation-binding negatives
  pass. Full Go/race/386 suites, amd64/386 builds, vet, pinned lint (0 issues),
  format and diff checks pass. Existing staging and migration bytes preserved.
- Next implementation dependency: transfer evidence export and authenticated
  production backup trigger, authoritative private Blob selection and evidence
  delivery. Local drain/startup scan wiring and fake-HTTP Blob transport tests
  exist; they are not live operational acceptance.
- Remaining test/release gates: remaining crash/fault windows, pending full-Service
  execution and older released-binary compatibility; provider-dependent
  relationship spike; live Azure/Workiva/Copilot acceptance; fresh zero-Blocking/
  Important whole-integration review; committed-checkout and remote CI checks.
- Production transfer remains disabled. No commit, push, merge or deployment.
  Evaluate this position with the user before expanding scope or publishing.

Older entries below preserve historical checkpoint evidence and are superseded
by this section for current local recovery status. Full Wave 3 acceptance stays
open.

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

Still open from that pass: controlled provider-side 429 validation, a genuinely
read-only Workiva grant, and all multi-user/horizontal governance. The
multi-user negative baseline also proved that one actor can consume another
actor's confirmation token under the current documented single-user design.

The Copilot Studio integration and auth milestone ran on 2026-09-17 (see item
3). Four of its five checks passed; the second-user check is pending.

The Azure enterprise deployment milestone ran on 2026-09-18 (see item 8). It
replaced the disposable Quick Tunnel with a stable Azure Container Apps
endpoint and passed its live write round-trip. This entry closes the "stable
public ingress" gap from item 3.

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

3. **Copilot Studio integration, partial**: validated 2026-09-17 in the
   Developer environment `dev-as-d9867869` with a custom connector, agent
   "Northern Lights test" (standard harness) and maker credentials, over a
   Cloudflare Quick Tunnel. Setup rules learned on the first run (standard
   agent, `nl-actor` as a formula, maker credentials, connector creator builds
   the agent) are in
   [`docs/copilot-studio-test-runbook.md`](docs/copilot-studio-test-runbook.md).

   Passed:
   - Authenticated MCP connector, discovery of all 7 tools and schemas. The
     schema test found nothing to fix.
   - Key flow: Copilot Studio sent the key both with and without `Bearer `.
     The server accepts both and rejects other shapes with 401 before any
     Workiva call. No proxy shim is needed.
   - Write test on `scope_2_energy_kwh` (800, 12345, 800): 7 `nl_audit_id`
     values matched audit records; the two confirmed writes carry Workiva
     operations `a78a0b68-d397-40f7-a2a5-367338dc8e4a` and
     `6c87e2d0-ae03-4f40-866b-6aa646583189`. Audit chain intact.
   - Trace matching at time level: Application Insights showed 15 successful
     Northern Lights calls (16:05 to 16:17 UTC) that line up by time, tool and
     user with the audit log.

   Remaining:
   - **Second-user actor check**: `nl-actor` carried `System.User.Email` on
     every call, but only one user was tested. Developer environments are
     owner-only, so this moves to the Sandbox phase.
   - **Exact trace matching (optional)**: Application Insights does not store
     tool outputs, so `nl_audit_id` never appears there. Record `traceparent`
     (already visible in the debug log) on the audit row to join Copilot
     activity to audit records exactly instead of by time.
   - **Stable public ingress**: the Quick Tunnel was deregistered by Cloudflare
     after under 8 hours ("Tunnel not found"). The agent then answered from
     other knowledge sources without reaching the server. Use a named tunnel or
     real hosting for the Sandbox phase, and turn off web search and general
     knowledge on test agents so tool outages are visible.
   - **Unexplained update calls**: two `workiva_update_field` calls at 16:14
     UTC (`b692c4e9-fc71-47a7-a1b4-e08dc05a5b3f`,
     `362ebfeb-425c-417f-b493-21ed997ad830`) wrote nothing between the write
     and the restore. Check their results in the Copilot activity view.

4. **Rate limits are process-local** (adversarial review ISSUE-017): the
   token-bucket limiter lives in the server process, so multiple replicas
   each get their own budget while Workiva enforces workspace-wide limits.
   Single-replica deployments are unaffected. Horizontal scaling needs a
   shared limiter (e.g. Redis) or per-replica budget division. Documented
   here as a known limitation.

5. **Multi-user governance, Waves 1 and 2 implemented locally; live and Wave 3
   remain open**: the code now has explicit `api_key` and single-tenant `entra`
   modes, OIDC discovery/cached JWKS validation, immutable `tid`/`oid`
   principals, delegated-scope and explicit app-role/client authorization, the
   six-permission vocabulary, per-tool enforcement, trusted audit actors, and
   fail-closed 401/403 paths. API-key mode remains the default. This has not
   been validated with a live Entra app registration or Copilot OAuth
   connection. The Wave 1 v2 contract requires the API manifest's
   `requestedAccessTokenVersion` to be `2` and
   `NL_ENTRA_AUDIENCE="<API-client-ID-GUID>"`. Clients request scopes as
   `api://<API-client-ID>/<scope>`, while the server exactly validates the GUID
   that Entra emits in the v2 token's `aud`; it does not normalize the scope URI
   into an audience. Tenant and audience UUIDs are canonicalized, while the
   authority must already use the canonical tenant path. The Wave 1 app-only
   deployment contract is implemented and documented: the API manifest must
   add `idtyp` to `optionalClaims.accessToken` without discarding existing
   optional claims, and role/client policy variables stay unset for
   delegated-only deployments. App-only access must remain disabled until a
   real token issued for the API proves `idtyp=app`; that is a live acceptance
   gate and has not been completed here. Wave 2 now transactionally migrates
   mappings, snapshots, pending writes, and audit rows into tenant-scoped
   storage; preserves old rows under `legacy-api-key`; enforces mapped resource
   ownership before direct Workiva calls; and stores only SHA-256 digests of
   32-byte random confirmation tokens bound to tenant, immutable actor,
   permission, exact mapping target, exact value/digest, and expiry. Local
   tests prove that a different actor cannot consume the token, permission
   removal invalidates it without consumption, tenant audit reads are scoped
   before resource filtering, and 100 concurrent confirms cause one consume
   and one mutation. This is local evidence only: no live Entra/Copilot or
   Azure deployment acceptance was run for Wave 2. Wave 3 must still add shared
   mappings, confirmation state, audit ordering, sessions where needed, and a
   shared limiter before multiple replicas; no multi-replica claim is made.

6. **Stateful MCP sessions are process-local**: Wave 1 Entra mode is
   intentionally stateless and does not issue or rely on `Mcp-Session-Id`;
   API-key compatibility mode retains the Go SDK's stateful session map. Do not
   enable stateful Entra sessions until shared, tenant-bound session storage
   passes cross-principal and cross-replica acceptance without sticky routing.

   The final independent Wave 1 security review found no Blocking or Important
   findings. Its remaining non-blocking hardening items are intentionally open:
   add a committed JSON-RPC batch authorization regression test or reject batch
   arrays at the HTTP pre-check (the SDK middleware already denies every
   unauthorized batch element before tool execution and audits it, but returns
   HTTP 200 with a JSON-RPC error); add inbound `/mcp` rate limiting or JWKS
   refresh debouncing to limit pre-auth key-fetch amplification; document that
   `tenant.admin` is reserved and currently grants no tool capability; strip any
   configured custom actor header in Entra mode as defense in depth; and ensure
   deployments never set `MCPGODEBUG=allowsessionsinstateless=1`. These do not
   weaken the clean Wave 1 authorization verdict, but remain explicit follow-up
   work before broad production exposure.

7. **Rotate the test API key, partial**: `NL_API_KEY` was rotated 2026-09-22.
   New value lives in `deployments/.env`, Key Vault `kv-nl-70cff1d0`, and the
   Container App secret (revision `--rot1754`); the old key now returns 401 and
   the full verify suite passes with the new key. Still open: the old value
   remains in plain text in four local agent session logs from 2026-09-14/15
   (Codex and Hermes) — remove those log copies. `NL_DEBUG_HEADERS=1` is still
   enabled on the live Container App; disable it after the Sandbox acceptance
   run (it is useful for the two-user trace). Workiva client secret rotation
   is a separate Workiva-side step (regenerate the API grant secret, update
   Key Vault + Container App secret).

8. **Azure enterprise deployment, partial** (2026-09-18, previously
   unrecorded): provisioned in subscription `Azure subscription 1`, resource
   group `WorkivaTest`, Japan East: Container Registry
   `northernlights70cff1d0`, Container Apps environment
   `cae-northern-lights-test`, single-replica Container App
   `ca-northern-lights` (image `northern-lights:test-fc065f5`, system-assigned
   managed identity), Key Vault `kv-nl-70cff1d0` holding `nl-api-key`,
   `nl-workiva-client-id`, `nl-workiva-client-secret` (mirrored as Container
   App secrets), storage account `stnl70cff1d0`, Log Analytics workspace,
   Application Insights `WorkivaMCP`. Stable endpoint:
   `https://ca-northern-lights.braveriver-d67a1a27.japaneast.azurecontainerapps.io/mcp`.
   Passed: `/healthz` and `/readyz` 200, unauthenticated `/mcp` 401, 7-tool
   discovery, live Workiva discovery, and a full write round-trip
   (`0 -> 987654 -> 0`) with two Workiva operation IDs and intact audit chain.
   Re-verified healthy 2026-09-22. Remaining: run the full Copilot Studio
   acceptance against this endpoint from the new Sandbox, reconcile the two
   unexplained `workiva_update_field` calls from 2026-09-17, then rotate
   credentials and disable debug logging (item 7).

9. **Power Platform Sandbox created** (2026-09-22, operator-side):
   `northern-lights-sandbox` (`e6e63f00-6b6a-eef0-a8a1-aa33522d53b0`), type
   Sandbox, region Japan, Dataverse Yes, state Ready, agent
   `Workiva_Sandbox_Test` created inside it. Billing plan `TestWorkiva` now
   lists `Northern-Lights-Sandbox` as a target. Both Microsoft users are in
   the environment and the agent was shared. Remaining: fix second-user actor
   propagation and complete acceptance.

10. **Copilot Sandbox two-user read test, closed with root cause**
    (2026-09-22): Sigmundus@ (agent owner) called the server with
    `nl-actor=Sigmundus@SicMundusInc.onmicrosoft.com`, HTTP 200. Sigmundo@
    (second user) received the Workiva spreadsheet list through the same
    shared maker connection, HTTP 200, but `nl-actor` was empty on every
    request. Root cause identified by the operator: sigmundo@ has no
    Microsoft 365 license, hence no mailbox, so `System.User.Email` evaluates
    empty in Power Fx. Verdict: shared-connectivity PASS, two-user
    attribution EXPLAINED-NOT-PROVEN. For unlicensed users the tool input
    needs a fallback identity expression (for example `System.User.Id` or
    `System.User.PrincipalName`) or Entra-backed principals (item 5). No
    production multi-user claim is made.

## Closed by adversarial review pass

1. **401 token refresh retry** (internal/workiva/client.go): `Do` now
   invalidates the cached token and retries once with a freshly fetched token
   after a 401 response. Repeated 401 responses return `APIError{401}`.

## Closed by the Copilot Studio milestone (2026-09-17)

- Redacted `/mcp` diagnostics behind `NL_DEBUG_HEADERS=1`: JSON-RPC method,
  status and error text, User-Agent, header names, key scheme, length and
  8-character SHA-256 fingerprint, actor and tracing headers. No characters
  of the key and never the request body. (An earlier version logged the first
  7 characters, which exposed the start of raw keys; fixed.)
- `scripts/test-session.sh start|status|requests|stop` runs a full local test
  session: server, Quick Tunnel, local connector copy, verification, and a
  redacted request table.
- Authorization accepts `Bearer <key>` (any case) or the raw key; empty,
  double-`Bearer` and other schemes return 401 before the MCP handler.
- Actor read from `nl-actor`, falling back to `X-NL-Actor`; trimmed, 256-char
  cap, control characters rejected. Writes (preview and confirm) are refused
  without an actor; reads record `unknown`.
- `nl_audit_id` in every tool result (structured content and first text
  block), stored on the audit row as `audit_id` and included in the hash chain.
- Schema test fails on `$ref`, array-valued `type`, numeric exclusive bounds,
  or a tool count other than 7; warns on enums.
- `POST /mcp` answers `application/json` (no SSE); `GET /mcp` returns 405.
- Connector OpenAPI file, `traces.kql`, `scripts/verify-mcp.sh` and the test
  runbook added under `deployments/copilot-studio/`, `scripts/` and `docs/`.

## Disposition of the Phase 2 Wave 2 independent review (2026-09-24)

Fixed and verified by tests (strict TDD: failing test observed first):

- IMPORTANT-1 (legacy consume bypass): the unbound `ConsumePendingWrite`
  wrapper was removed; `ConsumePendingWriteFor` now fails closed with
  `ErrPendingWriteBindingMismatch` on any row stored without actor/permission
  bindings, so migrated pre-Wave-2 rows can never be consumed and must be
  restaged. `TestPendingWriteMigrationPreservesRowsButFailsClosed` proves the
  migrated row is unconsumable with both tool and empty bindings, keeps its
  data, and still expires.
- MINOR-2 (raw-token forensic residue): superseded by the final fresh-review
  resolution below. The initial `VACUUM`-only fix was incomplete because the
  version-5 marker committed before the scrub and no truncating WAL checkpoint
  was required.
- MINOR-3 (startup janitor single-tenant): the startup janitor now calls the
  tenant-agnostic `DeleteExpiredPendingWritesGlobal`; the per-tenant
  `DeleteExpiredPendingWrites` remains for tenant-scoped callers.
  `TestDeleteExpiredPendingWritesGlobalSpansTenants` proves expired rows from
  two tenants are both removed while an unexpired row survives.
- MINOR-4 (spreadsheet-level ownership): `ResourceOwnership` now also unions
  `spreadsheets`, so a spreadsheets-only row asserts ownership.
  `TestResourceOwnershipSpreadsheetRowAloneAssertsOwnership` (store level)
  and `TestEntraSyncDeniedWhenOnlySpreadsheetRowIsForeign` (tool level, zero
  Workiva calls) cover it.
- MINOR-5 (silent token burn): when a consumed token's write fails before the
  mutation is submitted to Workiva, the returned error explicitly states the
  confirmation token was consumed and the write must be restaged.
  `TestUpdateFieldReportsBurnedTokenWhenPreWriteReadFails` asserts the
  message, zero mutation calls, and that re-presenting the burned token is
  rejected as unknown.
- MINOR-6 (docs): `docs/compliance/AUDIT.md` now documents that verify/export
  span all tenant chains, the complete migrated-v1 relabeling limitation (as
  corrected by the final fresh review below), and that confirmation
  consumption precedes the final Workiva write so a pre-write failure burns
  the token and requires restaging.
- MINOR-7 (test gaps): `TestForeignTenantConfirmationFailsAndLeavesOwnersTokenIntact`
  proves a foreign-tenant confirmation attempt fails with zero Workiva calls
  and leaves the owner's token intact and usable by the owner.

Residual risks, still open:

- Wave 2 has no live validation: no live Entra/Copilot or Azure deployment
  acceptance was run; all evidence is local tests.
- Exactly-once consumption and sync-claim guarantees hold for a single
  replica only; Wave 3 shared state is required before horizontal scaling.
- In API-key mode the actor header is caller-asserted, not
  identity-verified; only Entra mode binds the actor to a verified
  principal.
- Migrations are tested against synthetic pre-Wave-2 schemas built in tests,
  not against a production database backup.
- The concurrent-confirm exactly-once test
  (`TestOneHundredConcurrentConfirmsCauseOneMutation`) exercises
  `executeConfirmedWrite` directly, not the tool's registered handler with
  middleware; the middleware path (auth, actor extraction, audit wrapping)
  is not under that concurrency test.

## Disposition of the Phase 2 final fresh review (2026-09-24)

Fixed and verified by focused RED-to-GREEN tests:

- IMPORTANT/P2 (crash-safe pending-write forensic purge): version 5 now marks
  only the transactional raw-token-to-digest schema rewrite. A separate
  version-6 scrub-completion marker is inserted only after both `VACUUM` and
  `PRAGMA wal_checkpoint(TRUNCATE)` succeed. A busy checkpoint fails startup
  with version 6 absent, so the next `Open` retries. The marker write happens
  after truncation and contains no raw token bytes. The flow is additive:
  migrated pending-write data and the version-5 marker remain intact.
  `TestPendingWriteScrubRetriesVersion5DatabaseAndMarksCompletion` reconstructs
  a legitimate version-5/no-6 crash state, proves the scrub occurs, inspects
  the database, WAL, and SHM for raw bytes while the store remains open, and
  proves a clean idempotent reopen. `TestPendingWriteScrubCheckpointBusyLeavesMarkerAbsentAndRetries`
  holds a real WAL reader open, observes startup failure and an absent marker,
  then proves successful retry. `TestPendingWriteVersion5CollisionRollsBackDataAndMarker`
  proves a version-5 collision preserves the original row and records neither
  marker; `TestPendingWriteScrubMarksMemoryDatabase` covers in-memory startup.
- IMPORTANT/P2 (audit export hash version): `audit.Entry.HashVersion` is now an
  exported `json:"hash_version"` field used by every append, scan, verification
  switch, JSONL export, and MCP audit-trail payload. A migrated v1 row followed
  by a new v2 row exports exact values `1` and `2`, and the mixed chain still
  verifies. The MCP field is additive and is covered by the focused audit-tool
  payload test.
- MINOR (silent double-consume defense): `ConsumePendingWriteFor` now requires
  `RowsAffected()==1` before committing and returning a write. A real SQLite
  trigger forces a zero-row DELETE; the focused test proves consumption fails,
  returns no write, and leaves the row intact. The existing 100-way concurrent
  confirm test remains the end-to-end single-mutation coverage.
- MINOR (v1 relabeling documentation): `docs/compliance/AUDIT.md` now states
  that any entire all-v1 chain can be relabeled wholesale without detection,
  partial relabels break chain integrity, and v2 rows bind tenant identity.

Remaining risks and acceptance limits:

- No live Entra, Copilot Studio, Workiva, Azure deployment, or production-data
  migration acceptance was performed; evidence remains local and synthetic.
- No multi-replica acceptance was performed or is claimed. Mapping state,
  confirmation consumption, audit ordering, sessions where needed, and rate
  limits remain process-local until Wave 3 supplies shared coordination.
- The failure-path test forces a genuine busy truncating checkpoint with an
  open WAL reader. It does not inject disk-full or I/O failure during
  `VACUUM`, or a failure of the final version-6 marker insert; the reconstructed
  durable version-5/no-6 state covers retry after interruption between the
  schema commit and scrub completion.
- The hash chain still cannot prove completeness: deleting the final row of a
  tenant chain is undetectable, and an entire chain made only of v1 rows can be
  relabeled wholesale as documented above.

## Wave 2 PR CI follow-up (2026-09-24)

- The first PR #8 `test` run failed in `TestReadRangeCachesUnboundedRowsFromZeroOrigin`: `GetCachedCells(..., 0)` occasionally missed the snapshot when the call crossed a one-second storage timestamp boundary. Three read-range tests and one get-field cache assertion now query a one-minute fresh window, which matches their intent and avoids clock-boundary flakiness. The failing CI run is the RED evidence. The focused repeated test, full race suite, 32-bit tests/build, vet, and lint passed locally after the change; remote PR CI must pass before acceptance.

## Disposition of the 2026-09-08 adversarial review (ADVERSARIAL_REVIEW.md)

Fixed and verified by tests:

- ISSUE-001 demo values endpoint returned data on parse error -> 400 now
- ISSUE-002 PATCH/POST/PUT no longer retried on 5xx/transport (429 only)
- ISSUE-003 write audit entries record the caller identity (now `nl-actor`,
  falling back to `X-NL-Actor`)
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
- ISSUE-018 actor header sanitized (trim, control chars; cap raised to 256
  chars on 2026-09-17)
- ISSUE-019 CI runs golangci-lint
- ISSUE-020 repository published: github.com/DantaLabs/northern-lights

## Wave 3 transfer local service stage (2026-10-03)

- Added/continued `internal/transfer` local service implementation behind the
  existing `internal/workivaprovider.Router`; provider reads use its uncached
  route and mutations/polling use the routed writer. The local `Fence` seam is
  injected. `FakeFence` reports `Durable() == false`, so production constructor
  `NewService` fails closed unless a verified durable fence is supplied. No
  Azure Blob backend/durability is implemented or claimed (planned Wave 5).
- Focused tests exercise stage's authoritative source/target reads with no POST,
  token-free replay, stale source with zero POST, actor/permission/token CAS,
  verified fake claim/terminal fence path, 100 concurrent confirms at-most-one
  POST, ambiguous send timeout freezing/no repeat, persisted operation reference
  on poll timeout, and exact target spreadsheet/sheet/cell edit payload.
- Fixed target routing so the parsed locator coordinates and exact staged sheet
  ID, rather than A1/sheet-empty constants, reach the existing router.
- Evidence: `go test ./internal/transfer ./internal/workivaprovider -count=1`
  passed after fixes; focused transfer tests passed again after adding the poll
  timeout/reference test. No commit or push.
- Remaining: run full race/static/test suite; independent review the local
  service/fence protocol; finish missing local failure-injection cases and
  make constructor/wiring enforce production fail-closed configuration.
  Wave 5 create-only Blob fence, backup/restore, fence scan/join, and live Azure
  durability acceptance remain unimplemented and unverified.

Rejected after verification:

- ISSUE-008 (write proceeds when before-read fails): FALSE POSITIVE.
  `executeWrite` aborts when `readFieldValue` errors; the failing-test
  claim does not reproduce. `gridValue` ignores its error only after a
  successful read, yielding an empty before value for empty cells, which
  is correct behavior.

Wave 3 relationship implementation, partial (2026-10-02): registered
`workiva_discover_relationships`, connected `graph.Store` to the shared assurance
DB, added capability authorization and bounded persisted-graph lineage. Refresh
and provider discovery fail closed with `spike_required`; no provider results
are fabricated. Focused relationship-store tests pass. MCP/tool tests currently
fail because existing contract fixtures assert Wave 2 must have 11 tools and
Wave 3 registration is fixture-only; update those tests/goldens as part of the
contract registration work. Not implemented/verified: exact §4.6 output parity
(raw result fields have gaps), discover resource-policy enforcement, trusted
Entra-only refresh+idempotency ordering, refresh=false idempotency contract,
required negative selector/zero-call tests, recursive schema closure tests,
raw JSON-RPC success/error validation, and full build/test gate. Uncommitted
assurance/migration v14 and graph worktree changes were preserved. No commit or
push.

Wave 3 resource authorization follow-up (2026-10-03): added assurance migration
v15 and `assurance_relationship_resource_allowlist`, a tenant+actor+capability+
resource keyed server-owned table. Relationship traversal requires an active
`assurance.relationship.read` record for the root and both endpoints of every
returned edge/node. Root misses fail the call before response data; denied
non-root branches are omitted with `resource_not_allowlisted` reason and do not
emit their edge. No access is inferred from graph edges. `ProvisionAllowlist` is
a server-side operator provisioning API, not exposed via MCP; no rows/default
grants are created by migration. Raw MCP lineage test verifies persisted data
is served with zero provider API calls. Discovery/refresh remains
`spike_required` pending provider verification. Focused tests passed for relationships, MCP tools, assurance, and SQLite migration. `go test ./...`, `go vet ./...`, and `git diff --check` all pass. No commit or push.

## Wave 3 integrated acceptance review (open, push blocked)

Integration candidate: `versions/wave3-integrated`, branch
`feat/phase3-wave3-integrated`, based on main `129fa64`. Shared conflicts are
reconciled locally: relationship migration v14, transfer migration v15, both
signed bundle extensions/dependencies, and all thirteen tool registrations.
Recovered the missing untracked `internal/relationships/graph.go` and
`graph_test.go` from the relationship worktree. The original branch commit did
not include those required files.

Existing tests, race tests, 386 tests/build, vet, build, formatting, diff checks,
and golangci-lint 2.14.0 pass. Independent requirement probes fail 23 tests;
these are recorded outside the candidate's default suite and are not waived.
Not every failure represents public exploitability: public transfer execution
and provider relationship refresh remain disabled.

Acceptance blockers and bounded remediation:

- T-01 through T-07: trusted confirm identity/capability, pre-provider stage
  reservation/replay, unrelated transfer/token IDs, mapping/policy rechecks,
  persisted verified read-back, shared rich audit/finalization, malformed lease
  quarantine.
- T-08 through T-13: classification-specific reconciliation evidence, closed
  typed scalar values, canonical decimal equality, checked coordinate bounds,
  duplicate conversion-policy rejection, route content-hash verification.
- R-01 through R-09: discovery readiness, allowlist reference validation,
  resource-kind accuracy, honored discovery limits, refresh capability denial,
  exact-scope graph preservation, persisted schema bounds, locator narrowing,
  denial response contract.
- Public MCP stage/confirm/ack/reconcile execution, distinct acknowledgement and
  reconciliation permissions, successful raw transport coverage, transfer
  evidence export, full crash/fence/lease semantics remain incomplete.
- Azure create-only Blob fences, verified coordinated backup/restore,
  startup fence scan/join/quarantine and Wave 5 remain external/unverified.
- Interrupted model reviews count as no approval; the executed Hermes review
  rejects Wave 3 acceptance. No push, main merge or deployment.

Exact mechanisms, reproduced test names, conflict decisions and evidence paths:
`docs/validation/WAVE3_INTEGRATION_REVIEW.md`. Close findings using RED/GREEN
repros in bounded subsystem passes, then require a fresh independent approval
and the full gate before commit/push. Do not enable public writes just to
replace the placeholder while these findings remain open.

## Wave 3 integrated remediation progress (local only)

The dirty integration candidate now passes full Go, race, 386, vet, build,
formatting, diff, and pinned golangci-lint (0 issues), independently rerun in
the integration worktree. Earlier 23 red probes are addressed except the
contradictory verified-readback fixture: it stages source 2 while its mock
returns target 1 after POST, so reconciliation is the correct outcome; an
isolated corrected equal-value fixture passes. Applied migration SQL v1–v13
remains unchanged; v14 and v15 are additive.

Implemented locally: bundle/readiness/relationship scope/caps and exact-scope
graph preservation; trusted transfer identity, independent token/transfer IDs,
typed values, route/policy recheck, reservation/replay, rich audit and typed
read-back transactions, lease quarantine and renewal, and explicit unknown/
no-repeat responses. The create-only Azure Blob fence adapter passes local HTTP
contract tests. A signed SQLite backup/restore primitive exists, but its drain
gate is not wired across production writers. Test-injected raw MCP stage,
confirm, acknowledge and reconcile success/denial/replay paths validate against
the advertised schema; production main supplies no transfer service. Bulk and
provider-dependent relationship refresh remain disabled. Azure still runs the
accepted Wave 2 image.

Fresh independent review rejected acceptance: private Blob backup/evidence
upload, complete production drain, restore-on-start fence scan/join/quarantine,
and fault-injected destroy/restore remain absent. A later review found two
reconciliation exits needing correction: generic failed operation status does
not prove no mutation, and confirmed_applied needs fresh reconciliation read-back
evidence rather than an older equal row. Both now fail closed or require fresh
persisted read-back via additive v16; confirmed_not_applied remains disabled
until provider no-effect semantics are verified. Transfer evidence export,
relationship live spike, Copilot Studio and Workiva sandbox end-to-end, and
fresh zero-BLOCKING/IMPORTANT integrated review remain unverified. No integration
commit, push, main merge, or deployment occurred.
