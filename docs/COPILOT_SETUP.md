# Connecting Northern Lights to Microsoft Copilot Studio

Northern Lights is an MCP (Model Context Protocol) server that exposes
Workiva spreadsheet operations as tools Microsoft Copilot can call.

## Prerequisites

1. A running Northern Lights instance (Docker or local binary).
2. Either an `NL_API_KEY` value for compatibility mode or a completed
   single-tenant Entra configuration from the README.
3. A Copilot Studio environment with connector creation rights.

## 1. Start Northern Lights

```bash
# Docker (recommended)
docker compose -f deployments/docker-compose.yml up -d

# Or local binary
export NL_API_KEY="your-secret-key"
export NL_WORKIVA_CLIENT_ID="your-workiva-client-id"
export NL_WORKIVA_CLIENT_SECRET="your-workiva-client-secret"
./workiva-mcp -mappings configs/example.yaml
```

The server listens on port 8080 by default and serves the MCP endpoint
at `/mcp`.

## 2. Add the MCP server in Copilot Studio

### API-key compatibility mode

Use the current MCP onboarding wizard:

1. Open the agent and go to **Tools**.
2. Select **Add a tool > New tool > Model Context Protocol**.
3. Enter a clear server name and description.
4. Set the server URL to the public HTTPS Streamable HTTP endpoint,
   `https://your-domain/mcp`. Copilot Studio cannot reach localhost.
5. Select **API key**, then **Header**.
6. Set the header name to `Authorization`.
7. When creating the connection, provide the full value
   `Bearer <NL_API_KEY>`, not the Workiva client secret.
8. Create the connection and add the MCP server to the agent. Copilot Studio
   should discover all seven Northern Lights tools automatically.

### Entra mode

For trusted per-user or app-only identity, start Northern Lights with
`NL_AUTH_MODE=entra` and the environment-only tenant, authority, audience, and
permission mappings documented in the README. Configure the Copilot connection
with `NL_ENTRA_AUDIENCE="<API-client-ID-GUID>"`. In the API app registration
manifest, set `requestedAccessTokenVersion` to `2`. Configure the Copilot
connection to use OAuth 2.0 and request a delegated scope in the form
`api://<API-client-ID>/<scope>`. The scope identifier is used by the client,
whereas Northern Lights validates `aud` exactly against the API client-ID GUID
that Entra emits in a v2 access token; it does not normalize an `api://` URI
into that GUID. Both `NL_ENTRA_TENANT_ID` and `NL_ENTRA_AUDIENCE` must be UUIDs
and are stored in lowercase hyphenated form. The authority URL must already use
that exact canonical tenant path; Northern Lights rejects a differently cased
or otherwise noncanonical path. The server requires the standard
`Authorization: Bearer <token>` shape; a raw key is not accepted in Entra mode
and an invalid JWT never falls back to `NL_API_KEY`.

The API registration must expose the delegated scopes and/or app roles named in
the configured mappings. Delegated-only deployments leave
`NL_ENTRA_ROLE_PERMISSIONS` and `NL_ENTRA_APP_CLIENT_PERMISSIONS` unset. For
app-only support, preserve the API/resource app registration's existing
optional claims and merge this entry into its `optionalClaims.accessToken`
array:

```json
{
  "optionalClaims": {
    "accessToken": [
      {
        "name": "idtyp",
        "source": null,
        "essential": false,
        "additionalProperties": []
      }
    ]
  }
}
```

Access-token optional claims belong to the resource/API registration that owns
the token. Microsoft documents that `idtyp` is `app` for app-only tokens; see
the [Optional claims reference](https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference)
and [claims-validation guidance](https://learn.microsoft.com/en-us/entra/identity-platform/claims-validation).
Do not set the role/client policy variables or otherwise enable app-only access
until a real access token issued for this API proves that `idtyp` is exactly
`app`. App-only tokens additionally require an explicit client-ID permission
policy. Inbound `nl-actor` and `X-NL-Actor` values are ignored; audit
attribution comes from the validated `tid` and `oid` claims. Mutable user names
are display metadata only.

Entra-mode MCP transport is intentionally stateless for Wave 1: initialization
does not issue `Mcp-Session-Id`, and subsequent requests must not depend on one.
Each request carries and revalidates its own bearer token. API-key compatibility
mode remains stateful. Do not enable stateful Entra sessions until a shared,
tenant-bound session store has passed cross-user and cross-replica acceptance.

This repository change has not been accepted against a live Entra app
registration or a live Copilot OAuth connection. Complete those live gates
before claiming production multi-user acceptance. Wave 2 tenant-scoped local
state, resource ownership checks, and actor/permission/target/value-bound
hashed confirmation tokens are implemented and locally tested. The runtime is
still single-replica: shared confirmation state, audit ordering, mappings, and
rate limiting remain Wave 3 work.

Current Microsoft procedure:
<https://learn.microsoft.com/en-us/microsoft-copilot-studio/mcp-add-existing-server-to-agent>

Microsoft token-validation references:

- <https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference>
- <https://learn.microsoft.com/en-us/entra/identity-platform/claims-validation>
- <https://learn.microsoft.com/en-us/entra/identity-platform/access-token-claims-reference>
- <https://learn.microsoft.com/en-us/entra/identity-platform/scenario-protected-web-api-app-configuration>

## 3. Configure the Conversation Start Prompt

In your copilot's **Topics** tab, edit the **Conversation Start** system
topic and set the greeting to:

```md
Hello! I am the Workiva Reporting Assistant, powered by Northern Lights.

I can help you:

- Search for mapped reporting fields by name or description
- Read live values from Workiva spreadsheets
- Update spreadsheet values (with a confirmation step for safety)
- Connect new spreadsheets to the mapping layer
- Show who changed what and when (audit trail)

Just ask in plain language. For example: "What is our Scope 2 energy
consumption?" or "Update the total emissions field to 42000."
```

## 4. Recommended Behaviour Settings

Under your copilot's **Generative AI** settings:

- Enable **Use generative answers** so Copilot can form natural-language
  responses from tool results.
- Under **Instructions**, add: "Always call `workiva_search_fields` first
  when the user refers to a reporting concept by name rather than by cell
  address. Use the returned field metadata to decide which tool to call
  next."

## 5. Testing the Connection

Ask your copilot: "List all connected Workiva spreadsheets."

The expected tool call sequence:
1. `workiva_list_spreadsheets` (no args)
2. Copilot formats the response into readable text.

See [EXAMPLE_PROMPTS.md](EXAMPLE_PROMPTS.md) for more test prompts.
