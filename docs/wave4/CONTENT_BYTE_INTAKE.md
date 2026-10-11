# Content byte handoff

The `ingest` phase of `workiva_content_placement` accepts the source itself together with reviewed extraction or draft candidates. The authenticated connector must send the exact attachment bytes as canonical padded base64 in `source.source_bytes_base64`, or the exact pasted text in `source.source_text`. It must select one representation and include its media type. Filenames, chat answers, attachment links and caller-supplied hashes cannot replace source bytes.

The server computes the source SHA-256, saves an immutable owned source, binds candidates to it, and returns a metadata-only sealed result. Candidate interpretation hints remain caller-provided information. They do not establish provider writability, target protection, scale semantics or verified source coordinates. Extraction stays in Copilot; this endpoint does not add PDF OCR or fetch external URLs.

## Synthetic requests

Use [the paste request](fixtures/byte-intake-paste.json) and [the PDF request](fixtures/byte-intake-pdf.json) as the tool's arguments. The PDF request contains the bytes of [the injection fixture](fixtures/revenue-table-injection.pdf), including its embedded instruction to change the reported value. Its reviewed candidate is `12.4`; ingestion submits no Workiva mutation. The fixture marks fine-grained provenance unavailable.

[Expected hashes and lengths](fixtures/byte-intake-expected.json) are computed from the original bytes. Compare these with the returned source metadata, then repeat the same request and idempotency key. A successful sealed replay must return the same artifact bindings without saving another source or submitting a provider write. Changing a request while retaining its key must fail.

The example keys are fixed for repeatability. Use a new key for an intentionally new intake. Neither example includes a placement confirmation.

## Connector boundary

Send `tools/call` with `name: workiva_content_placement` and the request as `arguments` over the existing delegated Entra MCP connection. The server derives tenant/object identity and permissions from the validated access token. Do not add actor IDs, permission claims, policy snapshots, access tokens or storage keys to tool arguments.

| Phase | Permission configured in the server and signed access policy | Contract capability |
|---|---|---|
| `ingest` | `content.ingest` | `content.ingest` |
| `stage` | `content.stage` | `content.placement.preview` |
| `confirm` | `content.confirm` | `content.placement.confirm` |
| `acknowledge` | `content.acknowledge` | `content.placement.acknowledge` |
| `reconcile` | `content.reconcile` | `content.placement.reconcile` |

The final column describes the contract vocabulary; it does not grant permissions. Configure the exact server permission values in the trusted scope/role mapping and operator-signed policy. API keys, application-only tokens and subject fallback cannot authorize content intake.

## Runtime configuration

The content tool defaults off, leaving the existing 13 tools unchanged. The opt-in runtime adds one tool, for a total of 14. It requires non-demo delegated Entra authentication, ready assurance, a shared audit/database and maintenance gate, and these settings:

| Setting | Purpose |
|---|---|
| `NL_CONTENT_PLACEMENT=true` | Explicitly opt into the content runtime; only exact `true` and `false` spellings are accepted. |
| `NL_CONTENT_ACCESS_POLICY_ID` | Select the actor/resource access policy in the operator-signed assurance bundle. |
| `NL_CONTENT_STORAGE_KEY_FILE` | Absolute path to a private regular file containing the 64 hexadecimal characters of a separate AES-256 source-storage key. |
| `NL_CONTENT_STORAGE_KEY_ID` | Stable identifier for that source-storage key. |
| `NL_DB_PATH` (or `db_path` in server configuration) | Absolute SQLite file path beneath a service-owned private `0700` directory; existing database and WAL/SHM files must be regular `0600` files. |
| `NL_ASSURANCE_BUNDLE_PUBLIC_KEY` (or `assurance_bundle_public_key`) | Verify the actual operator-signed content policy; it is not a caller-provided tool argument. |

The key file must have mode `0600` or `0400` and must not traverse symlinks. Keep its contents out of source control and tool requests. Configuration does not create a key, grant provider permissions or deploy a revision.

The HTTP boundary limits the entire JSON-RPC body to 1 MiB. The tool also bounds its argument object, pasted UTF-8 source, decoded attachment bytes, candidate collections and draft text. Base64 and JSON framing count toward the body limit, so an attachment at the decoded-byte ceiling may exceed the transport ceiling.

Source BLOBs use authenticated encryption bound to their tenant, actor, artifact ID, plaintext hash and key ID. Returned hashes and lengths describe the original bytes. Reopening those sources requires the same key and key ID; configured encrypted storage rejects historical plaintext source rows rather than silently treating them as protected. This protects source bytes, not the entire SQLite database: candidate text and metadata still require private database and encrypted backup handling. Key rotation and historical plaintext migration are not automatic.

The public runtime wires ingestion, staging and acknowledgement. New confirmation and reconciliation execution remain unavailable until content-specific restored-database/fence admission is implemented. They return a structured capability error without submitting a provider write. Existing private backend implementations and tests do not establish that public admission. Target protection, effective writability and writer-contract verification also remain prerequisites for placement.

## Evidence boundary

Local authenticated transport tests establish the server path. The earlier Copilot PDF extraction test establishes the bounded analysis result. Live byte handoff requires a connector/flow run that sends the attachment bytes and produces a correlated saved source hash and audit record. A completed extraction plan with zero tool steps does not establish that handoff.

Placement continues through a signed destination profile, an exact preview, explicit confirmation, at-most-one provider submission, uncached read-back, and a separate visual acknowledgement. Unknown provider facts block placement. Wave 4.3 saved workflows and Wave 5 production recovery acceptance remain separate.
