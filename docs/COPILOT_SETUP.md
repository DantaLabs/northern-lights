# Connecting Northern Lights to Microsoft Copilot Studio

Northern Lights is an MCP (Model Context Protocol) server that exposes
Workiva spreadsheet operations as tools Microsoft Copilot can call.

## Prerequisites

1. A running Northern Lights instance (Docker or local binary).
2. An `NL_API_KEY` value (any strong random string).
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

Microsoft also supports OAuth 2.0 for MCP servers. Use OAuth rather than the
shared API key before multi-user rollout so Northern Lights can validate a
per-user identity instead of trusting `X-NL-Actor` metadata.

Current Microsoft procedure:
<https://learn.microsoft.com/en-us/microsoft-copilot-studio/mcp-add-existing-server-to-agent>

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
