# Dedicated sealed-backup job

This job makes one authenticated `POST /maintenance/backup` call and emits the
API's validated public sealed receipt. It does not access Key Vault or Blob
data, read signing keys, choose a restore candidate, or update application
control-plane state. The caller treats every ambiguous result as a failure and
does not retry it. A successful job event is not, by itself, an independent
restore validation.

## Trust and runtime configuration

Build and publish this image in the approved pipeline, then pin the deployed
image by immutable digest (`<registry>/<repository>@sha256:<IMAGE_DIGEST>`;
never deploy `latest`). Use a dedicated user-assigned managed identity. It
needs permission to invoke the application's protected maintenance API only;
do not grant it Key Vault or backup-container access. If the registry requires
authentication, use the registry's pull identity (`AcrPull` or its equivalent)
separately from the job's API identity.

Configure these job-only environment values from reviewed deployment
configuration (not from MCP/user input):

| Name | Value |
| --- | --- |
| `NL_BACKUP_ENDPOINT` | Exact HTTPS origin and `/maintenance/backup` path, e.g. `https://<APP_HOST>/maintenance/backup` |
| `NL_BACKUP_SCOPE` | `api://<API_AUDIENCE_UUID>/.default` |
| `NL_BACKUP_MI_CLIENT_ID` | Canonical UUID of the dedicated user-assigned identity |
| `NL_BACKUP_CHECKPOINT_PUBLIC_KEY_HEX` | Independently pinned 32-byte checkpoint public key as 64 lowercase hex characters |

The endpoint cannot include a query, fragment, user information, wildcard
host, or alternate path. The audience UUID, identity UUID, and checkpoint key
must be independently reviewed. Do not place credentials, private keys,
connection strings, or tokens in job configuration or logs.

## First run: manual

Create the job as `Manual` first. Substitute only values from the approved
deployment record; these placeholders intentionally contain no environment
IDs or public endpoint:

```sh
az containerapp job create \
  --name <BACKUP_JOB_NAME> \
  --resource-group <RESOURCE_GROUP> \
  --environment <CONTAINER_APPS_ENVIRONMENT> \
  --trigger-type Manual \
  --image <REGISTRY>/<REPOSITORY>@sha256:<IMAGE_DIGEST> \
  --cpu 0.5 --memory 1Gi \
  --replica-timeout 180 \
  --replica-retry-limit 0 \
  --replica-completion-count 1 \
  --parallelism 1 \
  --mi-user-assigned <USER_ASSIGNED_IDENTITY_RESOURCE_ID> \
  --registry-server <REGISTRY_HOST> \
  --registry-identity <REGISTRY_PULL_IDENTITY_RESOURCE_ID> \
  --env-vars \
    NL_BACKUP_ENDPOINT=https://<APP_HOST>/maintenance/backup \
    NL_BACKUP_SCOPE=api://<API_AUDIENCE_UUID>/.default \
    NL_BACKUP_MI_CLIENT_ID=<BACKUP_IDENTITY_CLIENT_UUID> \
    NL_BACKUP_CHECKPOINT_PUBLIC_KEY_HEX=<64_LOWERCASE_HEX_CHARS>
```

Review the resulting job configuration and image digest before starting one
manual execution:

```sh
az containerapp job show --name <BACKUP_JOB_NAME> --resource-group <RESOURCE_GROUP>
az containerapp job start --name <BACKUP_JOB_NAME> --resource-group <RESOURCE_GROUP>
```

Confirm completion and inspect the secured operational logs. The process emits
only `nl.backup.sealed` with the nine-field receipt, `received_at`, and API
host. Separately, an authorized operator must retrieve the matching successful
receipt from secured Log Analytics and verify it against the approved
checkpoint public key and production backup selector. Do not infer a trusted
backup from the newest blob, a job exit code alone, or this caller's event
alone. Preserve failed or ambiguous executions for investigation; the client
intentionally never retries.

## Schedule only after manual review

After the manual run and independent receipt review are accepted, apply the
same reviewed image, identity, endpoint, scope, key, timeout, and concurrency
settings as a scheduled job using cron `*/5 * * * *`, parallelism `1`,
completion count `1`, retry limit `0`, and replica timeout `180` seconds.
Reapply the full reviewed job resource/configuration, changing only its trigger
to `Schedule` and adding the cron expression; do not assume a partial update
preserves the manual-run configuration. For example, repeat the complete
`job create` command above with these trigger arguments (and all resource,
identity, image, and environment arguments unchanged):

```sh
  --trigger-type Schedule \
  --cron-expression '*/5 * * * *' \
  --replica-timeout 180 \
  --replica-retry-limit 0 \
  --replica-completion-count 1 \
  --parallelism 1
```

Azure CLI `job create` exposes `--trigger-type Schedule` and
`--cron-expression`, while the installed `job update` help does not expose a
trigger-type switch. Verify the read-back configuration says `Schedule` and
the exact cron before enabling it.

The five-minute cadence is an RPO target, not a guarantee. Thirty minutes is an
RTO target, not a demonstrated recovery time. Monitor successful sealed
receipts and independently validate restore readiness under the recovery
procedure. A job failure, missing receipt, malformed receipt, or lost response
requires operator investigation; do not add automatic retry or select another
backup candidate in this job.

References: [Container Apps Jobs](https://learn.microsoft.com/en-us/azure/container-apps/jobs), [Create a job with Azure CLI](https://learn.microsoft.com/en-us/azure/container-apps/jobs-get-started-cli), [Azure CLI job reference](https://learn.microsoft.com/en-us/cli/azure/containerapp/job?view=azure-cli-latest), and [Azure SDK for Go managed identity authentication](https://learn.microsoft.com/en-us/azure/developer/go/azure-sdk-authentication-managed-identity).
