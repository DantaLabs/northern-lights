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

## 2. Create a Custom Connector in Copilot Studio

1. Open Copilot Studio, navigate to your copilot.
2. Go to **Settings > Channels > Custom connectors** (or **Plugins**).
3. Select **Add a connector > Custom**.
4. Set the connection type to **Streamable HTTP**.
5. Enter your server URL: `https://your-domain/mcp` (or
   `http://localhost:8080/mcp` for local testing).
6. Under authentication, choose **API Key** and enter your `NL_API_KEY`
   value as a Bearer token in the Authorization header.
7. Save the connector. Copilot Studio will call `tools/list` and discover
   all seven Northern Lights tools automatically.

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
