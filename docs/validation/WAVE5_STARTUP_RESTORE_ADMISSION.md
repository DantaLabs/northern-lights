# Wave 5 startup restore admission (bounded opt-in)

Default startup remains the existing Wave 2 opener and does not construct a production transfer service. No migration v1-v17 bytes were changed. The opt-in is `NL_STARTUP_RESTORE_ADMISSION=true` (exact lowercase spelling); any other nonempty value fails closed. The production database path must be empty (including SQLite sidecars). An opt-in failure exposes only `/healthz=200`; `/readyz` and `/mcp` return 503. It does not open the application database, construct the transfer service, submit, or poll Workiva.

Admission requires `NL_AUTH_MODE=entra`, a verified/configured `NL_ENTRA_TENANT_ID`, assurance enabled, a non-demo profile, DefaultAzureCredential with access to a pre-existing private Blob container, and these deployment-owned values:

- `NL_RESTORE_BLOB_SERVICE_URL`: HTTPS Azure Blob account endpoint, not a container URL.
- `NL_RESTORE_BLOB_CONTAINER`: private container shared by backup and fence adapters.
- `NL_RESTORE_ENVIRONMENT_DIGEST`: exact environment namespace bound into the signed fence mark and objects.
- `NL_RESTORE_ENVELOPE_ID`: immutable selected envelope ID (32 lowercase hex).
- `NL_RESTORE_DATABASE_SHA256`: trusted expected database digest (64 lowercase hex), obtained independently of the candidate Blob.
- `NL_RESTORE_MIN_AUDIT_SEQUENCE`: independently retained positive audit rollback floor.
- Exactly one of `NL_RESTORE_ED25519_PUBLIC_KEY_HEX` (32-byte public key) or `NL_RESTORE_HMAC_KEY_HEX` (at least 32 secret bytes). Keep secret material out of YAML and logs; use a secret reference/environment injection for HMAC.

The Blob transport verifies signature, database hash, schema, row counts, audit, and signed fence high-water. The fence adapter lists and reads the entire bounded tenant/environment namespace through that mark. A missing, contradictory or orphan fence is committed to startup quarantine with rich audit before admission; an inventory, persistence, or audit error leaves startup unready. A clean join opens the application DB, but does not enable the public transfer service: the full Wave 5 backup, evidence, drain, and destroy/restore release gates remain unaccepted. On a later readiness loss, transfer-capable POST is refused. Startup never resumes a transfer or automatically polls. Retrying an unsuccessful admission against the same restored DB path is intentionally refused: investigate the durable quarantine and provision a fresh empty ephemeral destination under controlled recovery.

Open release gates: no authenticated production drain/backup scheduler or trusted selector/high-water publication channel is wired; supply the signed private envelope, independent selector/digest/audit floor, credentials and dedicated empty ephemeral DB at deployment. Local tests cover the fake Blob upload/restore and fence-scan/crash-window matrix as separate suites; a single combined fake Blob backup -> destroy -> restore -> join test through the production startup entrypoint has not been implemented. The backup/fence fake HTTP and SQLite tests are local evidence only, not live Azure durability, provider exactly-once, production RPO/RTO or Copilot/Workiva acceptance. A complete deployed stop/drain, sealed backup, revision destroy, fresh restore, join, rollback, and live-provider no-repeat exercise is still required before production enablement.
