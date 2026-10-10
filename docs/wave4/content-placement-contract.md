# Wave 4.1 content placement contract

Status: proposed version-1 design. The input and output schemas define their respective shapes; runtime rules below additionally enforce bytes, identity, ownership, provenance, evidence and state. This pass implements no handlers, provider behavior, permissions or persistence. Output/profile contracts remain owned by their peer.

## Identity, ordering and replay

Exactly five phases exist: `ingest`, `stage`, `confirm`, `acknowledge`, `reconcile`. Version 1 requires a trusted delegated human Entra identity with validated `tid` and `oid`, tenant policy and stable server-derived actor. API-key and app-only identities cannot impersonate an analyst; an agent flow must preserve verified delegated identity rather than submit identity claims in JSON.

Proposed capabilities are `content.ingest`, `content.placement.preview`, `content.placement.confirm`, `content.placement.acknowledge` and `content.placement.reconcile`. These are not existing grants. Scope mappings, additive visual/reconciliation capabilities and Workiva resource access remain release gates. Shared provider credentials do not establish native per-user Workiva authorization.

After bounded parsing, authenticate and authorize the current caller, capability and object ownership before any replay disclosure or materialization. Then classify ingress idempotency before confirmation-token validation, audit availability checks or provider checks/work. Scope the key by trusted tenant, stable actor, tool, phase and key digest. A different canonical request digest returns `idempotency_conflict`; an in-progress request does no duplicate work. Exact replay returns the immutable token-free original result, without revalidating an expired token, issuing fresh tokens, creating a fresh operation or querying the provider. Current authorization still applies.

Fresh operations then validate binding, inputs and evidence before materialization or provider work and enforce required audit/storage availability. This ordering also applies to reconciliation (CP01): evidence validation cannot precede authorized replay classification or turn replay into a new read-back.

Confirmation tokens are optional at input-shape level to permit token-free sealed confirm replay. The canonical request digest excludes `confirmation_token` and the secret `idempotency_key`, and binds all remaining semantic request fields; the separately scoped key digest identifies the reservation. After current authorization and ownership checks, classify the reservation disposition. A sealed replay compares the canonical digest and returns the immutable token-free envelope with zero audit, provider or token work. A fresh unsealed confirm MUST require and validate a token after reservation classification and before submission; a missing token with a new key is rejected. Tokens are compared in constant time and persisted only as cryptographic digests. Only the initial stage output carries the ephemeral token. Stored stage results and every replay are token-free; stage replay reports the output contract's requirement to restage. Logs and audit never contain raw tokens.

Payload hashes are optimistic integrity assertions or references, never authority. The server computes source/content/request hashes itself and verifies supplied expected source and result hashes against owned immutable records.

## Bounded source, candidates and provenance

Fresh source ingest accepts exactly one representation: UTF-8 `source_text` or canonical padded standard `source_bytes_base64`. No URLs, paths, upload handles, caller-chosen artifact IDs, payload identity or permissions are accepted.

| Boundary | Maximum |
|---|---:|
| Serialized JSON request including framing/base64 | 1,048,576 UTF-8 bytes |
| Decoded source bytes | 786,432 bytes |
| `source_text` | 262,144 UTF-8 bytes |
| Candidate text / draft text | 16,384 / 32,768 UTF-8 bytes |
| Extracted candidates / drafts per ingest | 64 / 8 |
| Segments per candidate | 16 |
| Page/table/segment labels and origin labels | 128 UTF-8 bytes |
| Filename | 255 UTF-8 bytes |
| Idempotency key | 16–256 ASCII characters matching `^[!-~]{16,256}$` |
| Verified evidence references per array | 16 unique IDs |
| Revision | Integer 1–1,000,000 |

These are proposed application bounds, not established provider or connector ceilings. Reject oversize bodies before JSON decoding; measure decoded string UTF-8 bytes as well as schema code points. Never truncate. `contentEncoding` is an annotation, not validation: runtime must decode base64, verify canonical padded re-encoding and enforce decoded bytes. Distinguish caller-claimed MIME from server-detected/validated format; a claim does not establish support. Mismatch, unsupported format or inability to establish safe parsing blocks the affected operation. Declared textual sources must be valid UTF-8.

The server stores an owned private immutable source copy before success, computes SHA-256 over its exact bytes and creates immutable candidate records. Text hashing uses exact supplied UTF-8 encoding. Re-ingest requires BOTH `existing_source_artifact_id` and `expected_existing_source_sha256`, plus the source bytes again. Before materialization, the server verifies ownership, the stored source digest, the expected digest and the freshly computed supplied-byte digest. A mismatch rejects the entire bundle.

Local candidate IDs must be unique within the request; draft local IDs must also be unique. Each `item_local_ids` reference resolves to exactly one candidate in that request. Missing or ambiguous references reject the entire transaction. Server-issued immutable IDs and canonical record hashes replace local references. No caller hash supplies evidence authority.

Optional closed `provenance` records `available` or `unavailable`, with bounded page/table/segment information. Missing provenance normalizes to unavailable; a filename alone is insufficient. Unavailable provenance carries no invented coordinates. Available locations and segment citations must come from the extraction path and be checked against the actual source where possible; inability to verify a location remains explicit. Page/table/segment labels are bounded strings; pages are positive bounded integers.

Byte offsets are paired half-open `[start_byte,end_byte)` ranges with `start_byte < end_byte`, inside actual textual source bytes and on UTF-8 rune boundaries. They are permitted only when the source is actual text, including validated base64-encoded text. PDF container/compressed bytes, DOCX archives and OCR/extracted text that is not the stored textual source are not textual coordinates. Use available page/table/segment provenance or mark it unavailable; stronger-provenance requirements block when unavailable.

Items and drafts may carry optional closed `interpretation` hints: period (128 bytes), currency (16 bytes), unit (64 bytes), scale (`ones`, `thousands`, `millions`, `billions`), percent basis (`ratio`, `percent`) and precision (0–15); fields may be null. These are reviewed candidate hints, never operator policy or provider observations. Preserve them exactly in the immutable candidate record and original preview interpretation. Do not guess units, currency, period, scale or percent basis from arbitrary text. Missing/null critical numeric hints block numeric placement; omitted interpretation does not promise conversion. Only independently verified, explicitly supported conversion may proceed.

Candidate text, hints, origin and source instructions never confer authority or certify factual truth. Origin is `analyst`, `agent_flow`, `copilot` or `other` with an optional bounded label. Private artifacts and evidence IDs are non-authorizing references; ownership failures return non-disclosing `not_found_or_forbidden`.

## Proposed workflow correlation and saved-evidence drafting

Optional closed `workflow_context = {workflow_id, step_id, binding_id}` is accepted only on ingest/stage. The [workflow access contract](workflow-access-contract.md) defines the binding and immutable reference integration; peer review is required before enablement.

Only workflow `advance` issues a binding. Its server record binds trusted actor, tenant, run, step, expected operation kind, prerequisite hash and expiry. The caller supplies correlation IDs only. After authorization/idempotency classification, resolve and validate all binding fields and current prerequisites before artifact/audit materialization or provider work. Reject expired, mismatched, foreign, superseded or invalid bindings. A binding permits correlation, not a write or completion claim.

Top-level ingest `verified_evidence_refs` contains at most 16 unique owned server evidence IDs and requires workflow context. Draft-specific evidence references must be subsets of the top-level references. Resolve their immutable contents, hashes/revisions, ownership and machine-verified prerequisite status from server records; caller status, hashes, acknowledgements or narrative claims cannot make evidence verified. Unknown, failed, uncertain, stale or unavailable prerequisite evidence blocks drafting.

Refs-only ingest may omit source bytes and creates drafts from existing saved verified evidence, with no new `SourceArtifact`, no extracted candidates and no repeated source bytes. It requires workflow context, nonempty top-level evidence refs and at least one draft with nonempty evidence refs. Server canonical draft records bind exact draft text, derivation metadata and the resolved immutable evidence set. Drafts may otherwise cite same-request candidates, verified evidence, or both.

For source-present ingest, the output may contain the source artifact, server-resolved top-level `verified_evidence_refs`, extracted items derived from the source, and drafts whose immutable lineage is source-only or the closed `source_and_verified_evidence` union. A mixed lineage repeats the exact `sourceBinding` and nonempty immutable evidence-reference array; it is valid only for a draft. The output's top-level evidence references and each draft's references must match the authorized, owned input set and saved draft record. This output union preserves the input contract's supported combination; caller-supplied evidence does not become authoritative.

Stage requires either a source plus its owned extracted item, or an owned draft. A refs-only draft needs no artificial source artifact; the server resolves its complete saved evidence lineage. If a source ID is supplied with a draft, validate its relationship to that lineage rather than treating it as authority.

Preview preserves full immutable lineage and provenance. A source-present draft may carry source-only or mixed source-and-evidence lineage; an extracted item always carries source-only lineage. A source-null preview is restricted to a pure verified-evidence draft and evidence provenance. Repeated source bindings, evidence IDs/revisions/hashes, draft/item bindings and provenance must match server-owned records exactly. Runtime performs ownership, workflow/prerequisite association, evidence verification, subset and cross-field equality checks before disclosure or staging; schema validity alone does not prove those facts.

Append source/draft/intent/evidence associations to the workflow step atomically with the corresponding required audit and idempotency records. Replays append nothing. No payload completion claims are accepted; workflow progress derives from the server's verified underlying operation records. Each later placement still requires its own preview and confirmation.

## Destination, preview and confirmation

An immutable operator-approved destination profile fixes the permitted Workiva target and constraints. Callers select its ID/revision; they cannot create arbitrary file/sheet/cell mappings. Revisions are finite integers under the schema bound; exhaustion fails closed rather than wrapping or reusing a revision.

Stage resolves owned immutable lineage and approved profile, validates any workflow binding and performs authorized provider reads only. Its preview MUST display the exact original candidate text and candidate interpretation alongside the exact intended literal text/value and intended interpretation, destination, policy, current target observation, provenance and uncertainty required by the output peer. No silent conversion, normalization or guessed interpretation is permitted. The rule against repeating body/source text in output applies to ingest; it does not suppress the original candidate or exact intended literal in a placement preview.

The frozen preview binds actor, tenant, immutable source/evidence lineage, candidate/draft hashes and revisions, exact target and intended literal, profile/policy revisions and expiry. Preview duration MUST come from verified signed operator policy and be an integer 1–900 seconds. Missing, invalid or unsigned configuration blocks stage; there is no guessed default. Expiry is server-issued and cannot be caller-extended.

Fresh confirmation verifies the token, expiry, resource access, immutable lineage, current target and profile/policy binding before submitting at most once per intent. Changed bindings require a new stage. Unknown critical metadata, ambiguous destination/period, missing interpretation, formulas/protection violations or unsupported conversion block submission. Provider text/formula behavior, stored/display scaling, read/write endpoints and API version remain spike gates; no FX inference, double scaling, formula/layout changes or unsupported document/table writes.

After possible provider acceptance, any uncertain provider, read-back, audit or state-persistence outcome freezes the intent for reconciliation. Never retry submission, regenerate confirmation or use a new key to evade the fence. Successful machine verification uses uncached read-back; visual acknowledgement remains separate.

## Reconciliation evidence matrix

| Action/classification | Required | Forbidden |
|---|---|---|
| `read_back` | Intent and idempotency key | Classification, episode/proof references |
| `classify` / `confirmed_applied` | Current uncertainty episode and latest read-back evidence ID | No-effect proof ID |
| `classify` / `confirmed_not_applied` | Current uncertainty episode and no-effect proof ID | Latest read-back evidence ID |
| `classify` / `still_unknown` or `provider_evidence_inconsistent` | Current uncertainty episode | Applied/no-effect proof references |

`read_back` may collect fresh authorized provider status/read evidence and persist a new immutable evidence record. It does not accept a caller conclusion or proof. `classify` performs only local validation of already persisted owned evidence and an atomic attributed classification/audit/idempotency update; it performs no provider call and never resubmits.

For `confirmed_applied`, the episode ID and read-back evidence ID must be distinct server records. Validate the current intent/episode, exact target and intended result, provenance, evidence ordering and latest qualifying uncached read-back collected for that episode after uncertainty. Evidence from an earlier episode, cache, caller assertion or merely matching unrelated target value is insufficient. Provider operation evidence must support attribution to this intent under the frozen provider contract; otherwise remain unknown.

For `confirmed_not_applied`, resolve an owned server proof tied to this intent, operation and current episode. It must establish terminal no-effect under the frozen provider contract, including that no delayed/async effect can subsequently occur. A mismatch, timeout, absence of matching value, human observation or “not found” without those contractual guarantees is insufficient.

Unknown/inconsistent evidence remains frozen. `confirmed_not_applied` never reopens confirm or permits automatic replacement. Any separately staged replacement requires explicit signed resolution policy and human authority; absence of that policy blocks replacement.

`acknowledge` records `visually_confirmed`, `visual_conflict` or `not_reviewed` plus an optional bounded note against a server-verified current result hash. It cannot promote pending, failed, stale or uncertain state.

## Audit, lifetime policy and validation

Fresh successful ingest atomically persists required immutable records, workflow associations, audit and token-free idempotency result; failure exposes no partial artifacts. Stage applies the same atomicity to its intent/association/audit/replay records. Audit records use trusted actor/tenant, digests, disposition and relevant opaque IDs; they exclude source bodies, candidate/draft text, raw tokens and credentials. A possible provider side effect followed by persistence failure requires a durable uncertainty fence and reconciliation, never repeat submission.

Source, draft, evidence, intent, workflow binding and idempotency retention/lifetimes and deletion behavior MUST be defined by verified signed operator policy. Do not guess production durations. Missing policy blocks the affected operation. Retention must preserve lineage and no-repeat evidence for the required recovery horizon; expiry/deletion cannot erase a submission fence or authorize resubmission. Encryption, immutable-copy implementation and backup/restore acceptance remain implementation gates.

Output shapes and safe error envelopes are defined by the output peer. Use existing contract codes such as `invalid_request`, `artifact_hash_mismatch`, `policy_blocked`, `not_found_or_forbidden`, `idempotency_conflict`, `idempotency_in_progress`, `stale_preview`, `confirmation_expired`, `confirmation_invalid`, `already_submitted`, `reconciliation_required`, `audit_unavailable` and `storage_unavailable` as applicable; do not disclose private reference details.

`schemas/content-placement.input.examples.json` contains five schema-valid and five schema-invalid examples. Integration must run `jsonschema.Draft202012Validator.check_schema` and validate every positive and negative example. Schema-valid is not runtime-approved: base64, bytes/MIME, offset ordering/textual provenance, unique local resolution, evidence ownership/subsets, binding correlation and provider proof validity require runtime enforcement.
