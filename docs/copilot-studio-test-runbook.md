# Copilot Studio test runbook

Browser steps for the "Copilot Studio integration and auth" milestone. Sam
runs these; the repo side (server, connector file, verify script, KQL) is
already prepared. Tick each box only when its pass condition holds.

## Before you start

- [ ] Northern Lights is running on port 8090 with `NL_DEBUG_HEADERS=1` and
      `NL_DISABLE_LOCALHOST_PROTECTION=true` (the tunnel host is not
      localhost). `scripts/verify-mcp.sh` passes against
      `http://localhost:8090/mcp`.
- [ ] `cloudflared tunnel --url http://localhost:8090` is running and its log
      shows the `https://<random>.trycloudflare.com` URL. `verify-mcp.sh`
      passes against `https://<random>.trycloudflare.com/mcp`.
- [ ] `deployments/copilot-studio/northern-lights-connector.local.yaml`
      exists (git-ignored) and its `host` is the current tunnel hostname.
      The committed `northern-lights-connector.yaml` keeps
      `YOUR_TUNNEL_HOST`; never commit a real host.

Quick Tunnel limits: no SSE (the server answers POST with JSON and refuses
GET with 405 for this reason), at most 200 requests in flight, no uptime
guarantee, and a new random URL on every restart.

### If `cloudflared` restarts

The URL changes. Update the host in the custom connector (Power Apps,
Custom connectors, Edit, General, Host), save the connector, then remove
the Northern Lights tool from the agent and add it again. Connector changes
take effect only after the tool is re-added. Also regenerate the
`.local.yaml` copy so it matches.

## Step 1: import the connector

- [ ] In Edge, open Power Apps and select the environment `dev-as-d9867869`
      (Developer environment; test-chat messages are not billed).
- [ ] Custom connectors, New, Import OpenAPI. Import
      `northern-lights-connector.local.yaml` (the copy with the real host).
- [ ] Pass: the connector saves with a single `POST /mcp` operation marked
      `x-ms-agentic-protocol: mcp-streamable-1.0` and an API-key security
      definition on the `Authorization` header.

## Step 2: add the MCP tool and check discovery

- [ ] In Copilot Studio, create or open the test agent and turn on
      generative orchestration.
- [ ] Tools, Add a tool, Model Context Protocol, choose Northern Lights.
- [ ] When creating the connection, enter `Bearer <key>` as the key (the
      server also accepts the raw key, so either form works).
- [ ] Pass: 7 tools are listed: `workiva_list_spreadsheets`,
      `workiva_read_range`, `workiva_search_fields`, `workiva_get_field`,
      `workiva_update_field`, `workiva_sync_mapping`, `workiva_audit_trail`.
      If any is missing, its schema is being hidden; run
      `go test ./internal/mcpserver/tools -run TestToolSchemas` and report.

## Step 3: bearer flow and actor header (first user)

- [ ] On the tool's Inputs, set `nl-actor` to `System.User.Email`.
      Copilot Studio adds headers only through the connector's OpenAPI
      file; `X-` header names do not work with MCP connectors, which is why
      the header is `nl-actor`.
- [ ] In the test chat, ask the agent to list the Workiva spreadsheets.
- [ ] Pass: the server log (`nl-debug-headers` lines) shows
      `authorization=present prefix="Bearer "` (or a raw key of the right
      length) and `nl-actor="sigmundo@SicMundusInc.onmicrosoft.com"`.
      The tool result shows an `nl_audit_id`.
- [ ] Decision: no proxy is needed if the header arrives in either accepted
      format. A proxy is needed only if `Authorization` is absent or
      mangled in the log.

## Step 4: second user and malformed key

- [ ] Share the agent with `Sigmundus@SicMundusInc.onmicrosoft.com`, sign
      in as that user, and run the same prompt.
- [ ] Pass: the logged `nl-actor` changes to the second UPN and the audit
      record for that call (`workiva_audit_trail` or
      `bin/workiva-mcp audit export`) carries that actor.
- [ ] Change the connection to use a malformed key (for example
      `Bearer Bearer <key>` or a wrong value) and run the prompt again.
- [ ] Pass: the agent reports a failure, the server log shows a 401 for
      that request, and no Workiva request follows it (no
      `workiva_list_spreadsheets` audit entry is written for it).
- [ ] Restore the correct key on the connection afterwards.

## Step 5: Application Insights and audit matching

- [ ] In Azure subscription 1, create an Application Insights resource and
      copy its connection string.
- [ ] In the agent: Settings, Advanced, Application Insights. Paste the
      connection string, turn on "Enable logging" and "Log conversation
      details".
- [ ] Run the write test in the test chat, in this order:
      1. Preview: ask to update a mapped field (expect
         `awaiting_confirmation` with a `confirm_token`).
      2. Confirm: approve the change (expect `written` and a
         `workiva_op_url`).
      3. Poll: the confirm call already waits for the Workiva operation;
         the result status is the outcome.
      4. Read the value back uncached with `workiva_read_range`.
      5. Restore the original value with another preview and confirm.
- [ ] In Application Insights, Logs, run
      `deployments/copilot-studio/traces.kql`.
- [ ] Pass: every tool call in the query results carries an `nl_audit_id`,
      and each ID matches a Northern Lights audit record
      (`bin/workiva-mcp audit export -db ./northern-lights.db`, or the
      `workiva_audit_trail` tool). For the confirmed write, the record with
      that ID also carries the Workiva operation URL.

## Hand back

Report: the tunnel URL used, which boxes passed, the actors seen in the log,
and whether a proxy is needed. Leave `cloudflared` running; stopping it
changes the URL and the connector would need re-importing.
