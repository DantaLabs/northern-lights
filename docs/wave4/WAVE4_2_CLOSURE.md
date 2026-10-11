# Wave 4.2 closure path

**Status:** The private backend and opt-in authenticated byte intake runtime are implemented. Live connector byte handoff, public provider-write admission and placement acceptance remain open. This is a closure checklist, not evidence that placement is enabled.

## Five actions to close the first useful slice

1. **Wire one feature-gated content tool.** Register `workiva_content_placement` only when enabled; keep `workiva_workflow` out of this phase. Require validated delegated Entra tenant/object identity and enforce authorization and artifact ownership before private reads, replay disclosure, or materialization. Map phases to the existing Go permissions and document their connector capability crosswalk:

   | Phase | Go permission constant and value | Wave 4 contract capability |
   |---|---|---|
   | `ingest` | `PermissionContentIngest` — `content.ingest` | `content.ingest` |
   | `stage` | `PermissionContentStage` — `content.stage` | `content.placement.preview` |
   | `confirm` | `PermissionContentConfirm` — `content.confirm` | `content.placement.confirm` |
   | `acknowledge` | `PermissionContentAcknowledge` — `content.acknowledge` | `content.placement.acknowledge` |
   | `reconcile` | `PermissionContentReconcile` — `content.reconcile` | `content.placement.reconcile` |

   The runtime handler and phase checks now use these exact Go permission values. The contract names are an explicit vocabulary crosswalk, not additional grants. Operator configuration must supply the trusted scope/role mapping and signed policy. The opt-in tool adds one entry to the default 13-tool catalog; public confirmation and reconciliation execution remain unavailable until actual content-specific restore/fence admission is implemented. See [runtime configuration and handoff fixtures](CONTENT_BYTE_INTAKE.md).

2. **Prove authenticated source handoff and safe private storage.** Send actual bounded paste and text-based PDF bytes through the authenticated flow/connector; verify server-computed hashes, immutable source and candidate/draft lineage, tenant/actor ownership and no external fetching. Private handling, access controls, retention enforcement, encryption protection for stored source, and body/token-free logs are implementation requirements before accepting real content. A filename, attachment-only screenshot, or fixture is not byte-handoff evidence.

   Local tests now send the actual fixture bytes through the real SDK/HTTP content handler with signed test JWTs and signed policies. Source bytes use authenticated encryption; owned reads verify their original hashes, and sealed replay preserves artifact/audit bindings. Startup checks the private SQLite directory, database/sidecar permissions and separate key file. This proves the local server path, not a live Copilot attachment-to-connector handoff. Candidate text and metadata remain subject to private database and encrypted backup handling; the source cipher does not encrypt the entire database.

3. **Load a real signed operator policy and verify the writer facts.** Use the configured trusted key and an operator-approved bundle that selects exact access, retention, resource, provider-contract and destination-profile records for one disposable cell. Confirm policy hash/revision bindings in the preview. Verify the actual configured writer's literal-write/format-preservation behavior and the target's protection/writability facts before permitting mutation; unknown critical facts keep that target blocked. A single analyst under an explicitly documented shared grant with signed actor/resource restrictions is within scope; do not claim native per-user Workiva enforcement.

4. **Pass the narrow live end-to-end path.** For the declared paste and text-PDF formats, show ingest → exact preview → explicit confirmation → at-most-one write → uncached read-back and machine result → separate visual acknowledgement. Exercise denied ownership, stale policy/target binding, and an uncertain outcome that remains frozen without a repeat. Record tested formats, target, policy revision/hash and sanitized audit correlation.

5. **Refresh closure evidence and review.** Run the applicable schema, focused provider/runtime and full local gates; obtain independent review with no Blocking/Important findings; update the execution and live-check records with the evidence and supported-format limits. GitHub [CI run 38078468982](https://github.com/DantaLabs/northern-lights/actions/runs/38078468982) is already green for vet, formatting, `go test ./... -race`, and pinned golangci-lint v2.14.0; it does not prove the live path.

## Scope and release boundary

Wave 4.3 saved sequential workflows are excluded from this closure pass. Wave 4.2 does not require five-user acceptance or native per-user Workiva authorization to start with one analyst under the documented shared-grant model, but trusted delegated identity and actor-bound backend access are still mandatory.

Wave 5 owns full production acceptance for changed-record backup/restore, fence consistency and restore admission, production Copilot end-to-end flows, production actor isolation and rollback. This is not a waiver of Wave 4.2 safety requirements: private storage, encryption protection, authorization-before-access, signed retention and no-repeat/reconciliation controls must be implemented before real content or writes are enabled. Wave 5 must still close its production recovery, isolation and operational-retention evidence before production release.
