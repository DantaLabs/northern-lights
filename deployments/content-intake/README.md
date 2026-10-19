# Content intake deployment

This image supports an explicitly user-authorized, intake-only deployment. Before starting it, require a fresh signed backup and an approved intake policy. The default catalog exposes 13 tools. Set `NL_CONTENT_PLACEMENT=true` to expose 14 tools, including `workiva_content_placement`, with trusted delegated `content.ingest` permission and an operator-signed policy. Confirmation and reconciliation are unavailable in this runtime, including when the flag is enabled.

Run one active writer at a time: stop the old writer before starting the new one. For rollback, set `NL_CONTENT_PLACEMENT=false` on the same reviewed new image and restore from a fresh, verified snapshot. Do not roll back to the old schema 17 image.

The Container App must provide `NL_CONTENT_STORAGE_KEY_BOOTSTRAP` as a Key Vault-backed secret reference and `NL_CONTENT_STORAGE_KEY_ID` as configuration. The entrypoint writes the bootstrap key to a private runtime file with mode 0600, unsets the bootstrap environment variable, and keeps the database parent private. Source keys stay separate. Never bake keys into source or images, and never print keys or secrets to logs. Use symbolic secret references; do not put secret values or a current Key Vault URI in tracked deployment artifacts.

Build from the repository root and set `VCS_REF` to the commit SHA in the ACR build; local builds use `dev` by default. The container starts `/workiva-mcp` with no extra arguments, matching the existing cloud command.
