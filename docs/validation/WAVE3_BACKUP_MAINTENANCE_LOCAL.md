# Backup maintenance configuration — local implementation

This documents the opt-in implementation, not a deployment or live acceptance.
Production transfer remains disabled. Do not put private keys, bearer tokens or
SAS URLs into the repository, handoff, logs or receipts.

## Backup configuration

`NL_BACKUP_MAINTENANCE` must be exactly `true` to enable the non-MCP
`POST /maintenance/backup` endpoint. Unset or exactly `false` disables it;
present-but-empty and other values fail startup. Enabling requires non-demo Entra
authentication, assurance enabled, a trusted tenant and the existing verifier.
Only a verified same-tenant `tenant.admin` principal can start the operation.
Request bodies cannot choose tenant, destination, storage or signing authority.

Required operator settings:

- `NL_BACKUP_BLOB_SERVICE_URL`: canonical HTTPS Azure account endpoint without
  path, credentials, port, query (including an empty `?`) or fragment.
- `NL_BACKUP_BLOB_CONTAINER`: pre-existing private container; no container creation.
- `NL_BACKUP_ENVIRONMENT_DIGEST`: explicit bounded environment namespace.
- `NL_BACKUP_SIGNING_KEY_FILE`: absolute private regular 0600/0400 file containing
  a consistent 64-byte Ed25519 private key in hex; symlinks and oversized files
  are rejected. Linux uses no-follow opening. Never commit this file.
- `NL_BACKUP_TEMP_PARENT`: existing canonical absolute private 0700 directory
  without symlink traversal, outside the repository.

Azure adapters use `DefaultAzureCredential`. Constructing them does not certify
live authorization or storage policy. Each operation checks observable private
container policy, drains the shared gate, signs a terminal audit checkpoint,
captures storage-service high-water, snapshots, uploads create-only objects and
reads them back before returning a synchronous sealed receipt. Failed sealing
returns no success receipt and retains private staging; admission remains closed
if shared SQLite writability cannot be observed.

## Independently retained restore authority

Keep the authenticated receipt and the trusted manifest verification public key
outside the selected envelope. Do not obtain rollback authority from the Blob
manifest being restored. Configure the existing explicit startup restore path:

| Retained authority | Startup setting |
| --- | --- |
| Envelope ID | `NL_RESTORE_ENVELOPE_ID` |
| Database SHA-256 | `NL_RESTORE_DATABASE_SHA256` |
| Audit high-water | `NL_RESTORE_MIN_AUDIT_SEQUENCE` |
| Checkpoint public key | `NL_RESTORE_CHECKPOINT_PUBLIC_KEY_HEX` |
| Checkpoint ID | `NL_RESTORE_CHECKPOINT_ID` |
| Creation-time minimum (optional) | `NL_RESTORE_MIN_CREATED_AT` |
| Trusted manifest verification public key | `NL_RESTORE_ED25519_PUBLIC_KEY_HEX` |

The backup implementation currently uses the configured Ed25519 key for manifest
and checkpoint signatures, but restore keeps both verification inputs explicit.
The existing HMAC manifest verification alternative remains mutually exclusive
with the Ed25519 manifest verification key; it does not replace checkpoint trust.

Restore also requires `NL_STARTUP_RESTORE_ADMISSION=true`, explicit
`NL_RESTORE_BLOB_SERVICE_URL`, `NL_RESTORE_BLOB_CONTAINER` and
`NL_RESTORE_ENVIRONMENT_DIGEST`, and an empty destination configured by `NL_DB_PATH`.
Private-container validation and the real fence join must succeed before normal
startup; refusal leaves readiness and POST admission closed. A clean restore does
not enable production transfer writes.

## Acceptance boundary

Local tests use real SQLite, cryptographic verification and the actual Azure SDK
against disposable HTTP fixtures. Live Azure backup/destroy/restore, operator
credential and retention validation, Workiva/Copilot acceptance, durable evidence
delivery and whole-integration release review remain separate gates. No live
backup or deployment is implied by this document.
