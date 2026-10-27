# Northern Lights: IT and risk brief

For the firm's IT team. Status: demo and pilot, synthetic data only. All demo tools sit behind `NL_DEMO_PIPELINE=true` (ADR 0002).

## Data flow

1. An analyst talks to a Copilot Studio agent published to Teams, in the firm's own Microsoft tenant.
2. The agent calls Northern Lights over MCP (streamable HTTP, bearer token).
3. Northern Lights calls the Workiva REST API to read and write spreadsheets and the report document.

What moves where:

- **Copilot to Northern Lights:** tool requests (field names, section IDs, drafted section text) and the approval token.
- **Northern Lights to Copilot:** values read from Workiva, narrative facts, findings, verbatim blocks and previews.
- **Northern Lights to Workiva:** cell values and section text. Workiva credentials are never sent to Copilot.
- **Northern Lights locally:** one SQLite database on the operator's infrastructure holds field mappings, a read cache, pending approvals (token digest only) and the audit log.

No language model runs inside Northern Lights (ADR 0001). The server is rule-based code. Copilot, already inside the firm's tenant, drafts the text; the server computes every number and checks the draft. No new model endpoint is added. Optional Azure storage paths (backups, evidence delivery) exist in the code and are off unless configured; their acceptance for this deployment is open.

## Identity

- With `NL_AUTH_MODE=entra` the server accepts only Microsoft Entra access tokens for one tenant. It validates signature, issuer, audience, expiry and tenant before any tool runs.
- The analyst's delegated identity is used. The actor recorded in the audit log is the verified tenant ID and object ID from the token. Email, UPN and the `X-NL-Actor` header are never used for authorization.
- Permissions come from the token `scp` claim. Preview needs `workiva.write.preview`; confirm needs `workiva.write.confirm`.
- Northern Lights calls Workiva with one integration grant (OAuth2 client credentials), held in environment variables and cached in memory only. Workiva therefore sees one integration user, not the analyst. There are no per-user Workiva permissions; the Entra check is the only per-person control.

## What is written and how

1. **Preview.** `workiva_prepare_section` reads source and prior-period fields, computes narrative facts and findings, and stages one change set. The analyst sees every before and after value.
2. **One approval.** The analyst approves once. The change set carries a single-use confirmation token (32 random bytes, only its SHA-256 digest stored, 10-minute expiry), bound to the actor, the confirm permission and the exact targets. A reused, expired or foreign-actor token is refused and nothing is written.
3. **Write.** One Workiva update per sheet. Writes are not retried after an uncertain outcome; the status says so.
4. **Read-back.** Each field is read again without the cache. It is marked `completed` if it matches and `mismatch` if not.

For the report document, `workiva_stage_section_text` checks Copilot's draft before preview, and `workiva_confirm_section_text` creates the section if needed, writes the text and reads it back.

- **Verbatim blocks** must equal the stored approved wording character for character (line endings aside). Any change is refused with the position.
- **Number guard.** Every number in a narrative block must match a narrative fact. Other numbers are refused.

## Audit trail and evidence export

- Each tool call and write is recorded in an application-level append-only SQLite table with a SHA-256 hash chain per tenant. Changing a row, or deleting a row that has a successor, breaks the chain.
- A change set writes one audit row per field written, with actor, target, before value, after value and the Workiva operation URL.
- `./workiva-mcp audit verify` checks the chain and names the first broken sequence number. `audit export` emits JSON lines for a SIEM.
- `workiva_export_evidence` exports one authorized subject as manifest-v2 evidence; it needs the `evidence.export` permission.
- The chain does not detect deletion of the final row, and verification proves integrity of surviving rows, not completeness. Retention is operator-managed; the log never expires.

## Open risks

- **NL-LIVE-002, scope requests do not narrow the Workiva grant.** In the 15 September live validation, requesting `file:read` still returned a token with write access, so the scope string is not a permission boundary. Mitigation: use a dedicated integration user and API grant holding only the permissions the deployment needs (startup validation of effective scopes is recommended, not yet built).
- **No per-user Workiva permissions.** Every Workiva call runs as one integration grant, so Workiva history shows the integration, not the analyst. Mitigation: Entra permissions gate each tool and the audit log records the analyst per field, with the Workiva operation URL for cross-reference.
- **Entra permission mapping for the demo tools.** Prepare and stage need `workiva.write.preview`, both confirms need `workiva.write.confirm`, and the read-only `workiva_compare_fields` needs `workiva.read`; the delegated scopes `NorthernLights.Read` and `NorthernLights.Write` grant them. Verified on 27 October 2026: Copilot in Teams called the tools with a user's Entra token and the audit log recorded that user's tenant and object ID. Status: closed
- **Copilot drafting quality.** Copilot may write weak or wrong prose, or misread a request. Mitigation: the number guard and verbatim checks refuse unverified numbers and altered legal wording, and the analyst reviews the preview before the single approval.
- **Single-tenant, single-replica deployment.** State and audit ordering are in local SQLite; shared state across replicas is not supported. Status: open
- **Optional Azure paths not accepted.** Backup, restore and evidence blob delivery exist in code but are not accepted for this deployment. Status: open
