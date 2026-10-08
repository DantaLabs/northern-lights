# Phase 3 Wave 3 integration and independent evaluation

Verdict: REJECTED for Wave 3 acceptance and push. This is a local integration candidate, not a release, merged wave, or completed transfer implementation.

## Baseline and ownership

- Remote main verified by `git ls-remote origin refs/heads/main`: `129fa64a94087d513665f3fff5a2b3db770643f4`.
- Relationships: `3a4dc774b76438c318db9168df2c7b85c98496b1`.
- Transfer: `5febe85947dc86f2bd9b69fc0a017cc5eb3d4d39`.
- Integration worktree: `/home/agapantha/northern-lights-workspace/versions/wave3-integrated`, branch `feat/phase3-wave3-integrated`.
- Canonical requirements: `planning/phase3/PHASE_3_MASTER_PLAN.md` in the workspace, especially sections 4, 7, 9, Wave 3 exit, and C-021–038/C-041/C-045/C-055–063.
- No push, deployment, or main merge was performed. Partial worker edits and original branches were preserved.
- Codex gpt-6.1-sol launcher rejected the model. The configured gpt-5.6-luna worker then performed partial reconciliation before subscription quota exhaustion. Both separate fresh-context reviewers also exhausted quota before writing their verdicts. Claude Code fallback authentication was expired. No interrupted review is counted as passed.
- Hermes independently reran the preserved candidate, inspected production semantics and ran executable negative probes; this review rejects acceptance. It is independent of the integration worker, but is not a completed fresh-model approval.

## Why the branches conflicted

Both started from the same Wave 2 baseline and added independent work into the same extension points. This is primarily disjoint functionality plus mutually incompatible final-count assumptions, not evidence that one feature should replace the other.

### Conflict decisions

1. `internal/assurance/migrations.go`, migration constant/list hunk: same-question/different-answer on the next version number. Both claimed v14. Keep relationship schema as v14 and place transfer schema at v15; retain all original v1–v13 SQL exactly. Do not combine two independent applied SQL versions under one marker.
2. `internal/assurance/migrations.go`, table inventory hunk: disjoint intent. Retain graph state/allowlist/revision tables and all transfer/fence/evidence/route/conversion tables.
3. `internal/assurance/report.go`, bundle fields hunk: disjoint intent. Preserve relationship allowlist plus transfer routes and conversion policies in the same signed bundle.
4. `internal/mcpserver/registry.go`, import hunk: disjoint intent. Keep both relationships and transfer package imports.
5. `internal/mcpserver/registry.go`, dependency hunk: disjoint intent. Keep both `Relationships` and `Transfer` dependencies; neither replaces the other.
6. `internal/mcpserver/tools/all.go`, registration hunk: disjoint intent. Register both new names after the eleven Wave 2 tools, in canonical order.
7. `internal/mcpserver/tools/schema_test.go`, contract-name/test hunk: same-question/different-answer. Neither isolated branch's twelve-tool assertion describes the integration. Require the exact thirteen canonical names, including both additions.
8. `internal/mcpserver/tools/schema_test.go`, final registration hunk: same-question/different-answer. Require relationship at index 11 and transfer at index 12; neither remains fixture-only in discovery.

Shared non-conflicted changes also needed semantic adjustment:

- Migration assertions in assurance and SQLite tests now expect 15, not either branch's isolated count or an accidental intermediate 16.
- Raw JSON tools/list tests expect 13.
- Transfer is included in audit-required tool classification and has a permission mapping, but that mapping still needs phase-specific correction noted below.
- Existing backlog narratives are preserved as history; they are not acceptance evidence.

### Additional build blocker recovered

The relationship commit imported `internal/relationships` but did not track its `graph.go` or `graph_test.go`. A checkout containing only the committed branch cannot build. Both files still existed untracked in the relationship worktree. They were recovered into the integration candidate without altering that original worktree. Their presence locally does not mean they were previously pushed.

## Verified local integration gates

The candidate passes:

- `go test ./... -count=1`.
- `go test -race ./... -count=1`.
- `GOARCH=386 go test ./... -count=1` and `GOARCH=386 go build ./...`.
- `go vet ./...` and `go build ./...`.
- `golangci-lint 2.14.0 run --timeout=5m ./...`: zero issues.
- Empty `gofmt -l .`, `git diff --check`, and cached diff check.
- The existing raw MCP/schema suite, including thirteen-name discovery.
- v13-to-v15 migration test: old resource row retained, reapplication succeeds, transfer object collision leaves marker v14 and rolls back partial v15 objects.
- Programmatic comparison: all thirteen applied migration constants are unchanged; every changed source path from both branch diffs exists in the candidate.

These green gates do NOT prove canonical behavior. The independently added requirement probes produce 23 failing tests on the same candidate. They run in a separate evaluation copy, not in the integration source tree. Several cover dormant internal paths or helper preconditions, so 23 failures are not represented as 23 externally exploitable vulnerabilities.

## Reproduced acceptance findings

### Transfer: blocking before any public execution wiring

- T-01: `service.go:166–178` trusts request `TenantID`, `ActorID`, and `Permission` without checking the context principal. An unauthenticated context and a preview-only principal each reached one fixture provider POST. Derive identity from trusted context and enforce confirm capability in the service as well as transport. Current public placeholder does not expose these internal POST paths yet.
- T-02: `service.go:57–110` reads both providers before `Store.Stage` performs replay lookup. Same-key stage replay increases reads from 2 to 4. Implement the authoritative reservation/sealed-envelope lifecycle before execution-only reads; races, conflicts and in-progress must do zero provider work.
- T-03: `service.go:107` derives the persisted transfer ID from the first twelve bytes of the raw confirmation token. The persisted intent includes the first 24 hex token characters. Generate unrelated IDs; digest-only storage prohibits raw-token prefixes, not just the whole token.
- T-04: confirm never re-resolves or verifies the staged mapping/route/policy against active signed state. Changing the route content hash after stage still permits a POST. Bind frozen revisions and verify current authority before submitting.
- T-05: `finishVerified` seals `api_verified` without persisting the uncached read-back row. A fixture with an equal target returns verified but has zero `assurance_transfer_readbacks`. Commit read-back, operation-completion evidence, rich audit link and replay envelope atomically before returning success.
- T-06: a staged transfer has no shared `assurance_audit_links` entry. The private `transfer_audit_events` table is not the required tenant hash-chained rich audit/sealed replay transaction. Implement pre-execution fail-closed audit and atomic finalization, including post-side-effect failure classification.
- T-07: malformed/empty leases are excluded by `QuarantineExpired`; a claimed row remains claimed. Recovery must quarantine expired AND malformed in-progress state, once, without submit/poll.

### Transfer: additional important domain/contract gaps

- T-08: `ApplyReconciliation` requires equal persisted read-back before switching on classification. `still_unknown` without such evidence is rejected, contrary to the defined open disposition. Only applied classification requires matching intended evidence; other classifications need their own proof and exits.
- T-09: stage stores scalar provider JSON while `decodeScalar` accepts mostly typed objects and numbers. Ordinary text and both boolean scalars fail the write decoder. Preserve and project canonical closed typed values end to end; do not patch in unsafe literal/formula execution.
- T-10: `canonicalProviderValue` marshals provider bytes, not canonical typed decimals: `2.0` remains `2.0`, so equivalent values compare unequal.
- T-11: raw coordinate helper accepts oversized/wrapped input (`A18446744073709551617` maps to row zero). The current signed target regex limits this helper's public reach; this is a boundary/helper hardening finding, not a demonstrated current public write exploit. Validate provider limits and checked arithmetic before multiplication/narrowing.
- T-12: bundle conversion-policy duplicate check reads the materiality `policies` map instead of `conversionPolicies`; duplicate conversion ID/revision passes validation.
- T-13: `ResolveTransferRoute` never recomputes route content hash. Altering stored `route_json` while retaining the old hash resolves a tampered target sheet.

### Relationships: important acceptance gaps

- R-01: `ApprovedRelationshipResources` lacks the readiness check: a store explicitly marked not-ready still returns approved discovery nodes.
- R-02: signed allowlist entries can reference a resource absent from all report definitions. Candidate validation accepts unresolved references.
- R-03: discovery returns the field value kind (`number`) as resource kind, rather than spreadsheet resource kind.
- R-04: discover classifies `max_nodes`/`max_edges` as ignored, although the plan honors them. Two nodes are returned for `max_nodes=1` with completed status. Enforce request limits and explicit partial omissions.
- R-05: a read-only relationship principal requesting refresh gets `spike_required`, not capability denial. Require trusted identity and both capabilities before executing or revealing refresh capability state.
- R-06: graph refresh inactivates by source resource, not exact normalized scope digest. Refreshing scope `[a]` inactivates an edge recorded under `[a,b]`. This internal method is not currently called by public refresh, but must be corrected before enabling it.
- R-07: refresh stores node kind/external IDs beyond advertised schema bounds. Validate closed vocabulary and every persisted/output bound before commit.
- R-08: a caller locator is accepted when the persisted root locator is blank, without proving a narrowing selection; traversal proceeds at whole-resource scope. Either implement locator-aware narrowing or deny unsupported narrowing.
- R-09: the HTTP authorization-denial wrapper emits only the generic error body, not the relationship output arrays/operation/completeness. This is a response-contract acceptance gap, not an authorization bypass; explicitly settle HTTP-vs-tool schema policy and prove both paths.

### Additional inspected gaps, not counted as reproduced tests

- Public transfer handler deliberately performs no stage/confirm/ack/reconcile service action even when `Deps.Transfer` exists. Main does not construct the production service. Passing tools/list and unavailable replies is not successful execution acceptance.
- The integration's transport permission switch maps acknowledge/reconcile to `workiva.write.confirm`; canonical capabilities are `workiva.visual_ack` and `reconciliation.manage`, which are absent from the permission vocabulary. High-impact API-key restrictions and conditional phase field validation also need explicit successful/negative transport coverage.
- Confirm finalization does not persist all terminal state/audit/replay evidence, execution lease renewal and startup scheduling are incomplete, and pre-submit state is persisted after rather than before the provider request. Full crash-window/lease-owner tests are required before exposing it.
- Transfer export remains disconnected from the separate `transfer_intents` model; generic evidence handling currently has no transfer subject implementation.
- Relationship cycle/omission cardinalities and node/edge traversal completeness need a further large-graph semantic pass; the existing traversal loads adjacent edges before applying response caps.

## Original five open items

1. Branch integration: local reconciliation and build recovery complete; no accepted integration commit or push yet.
2. `refresh=true`: still intentionally `spike_required`; no live provider scope-completeness claim. The dormant graph refresh method also fails independent exact-scope tests.
3. Public transfer execution: still a placeholder. Wiring it now would expose reproduced service defects; do not enable it as a shortcut.
4. Azure Blob durable fences, backup/restore, fence scan/join and Wave 5: remain unimplemented/unverified here. FakeFence reports non-durable and must remain test-only. No Azure deployment/destruction was attempted.
5. Fresh integrated review: no approval. The interrupted model reviews do not qualify, and the completed Hermes executable evaluation rejects acceptance.

## Reproducible evidence and next acceptance contract

Evidence files outside the repository:

- `/home/agapantha/northern-lights-workspace/evidence/wave3-integrated-quality-gates.json`.
- `/home/agapantha/northern-lights-workspace/evidence/wave3-independent-eval-test-results.json` (JSON event stream and exact 23 failing test names).
- `/home/agapantha/northern-lights-workspace/evidence/wave3-integration-preservation-check.json`.
- `/home/agapantha/northern-lights-workspace/evidence/wave3-eval-probes/` (five Go probe files mirrored under internal package paths).

Replay: copy the current candidate into an isolated directory, overlay the mirrored probe files, then run `go test ./internal/assurance ./internal/relationships ./internal/mcpserver/tools ./internal/transfer -run 'Test(AuditProbe|IndependentEval)' -count=1 -v`. Existing helpers supply synthetic provider fixtures; no live Workiva mutation is needed or claimed.

Remediation must be bounded sequentially: (1) bundle/readiness/relationship scope and caps; (2) transfer identity, canonical typed intent and reservation lifecycle; (3) audit/readback/operation/recovery transactions; (4) public phase validation and successful raw MCP paths with production disabled until durable recovery gates pass; (5) fresh independent review and full gate. Applied migrations must remain unchanged. Push only after zero blocking/important findings for the explicitly delivered scope; never call a foundations-only push Wave 3 acceptance.
