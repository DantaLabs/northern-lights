# Copilot Studio test runbook

Browser steps for the "Copilot Studio integration and auth" milestone. Sam
runs these; the repo side (server, connector file, verify script, KQL) is
already prepared. Tick each box only when its pass condition holds.

First run: 2026-09-17. Results are recorded in `BACKLOG.md` item 3. The
setup rules marked **Required** below come from that run; skipping any of
them makes the test fail in a way that looks like a server problem.

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

Cloudflare can also drop a Quick Tunnel while `cloudflared` keeps running.
On 2026-09-17 it was deregistered after under 8 hours, and the log filled
with `Tunnel not found`. Before each session, confirm the tunnel with
`curl https://<random>.trycloudflare.com/healthz` (expect 200). If it fails,
restart `cloudflared` and follow the section below.

### If `cloudflared` restarts

The URL changes. Update the host in the custom connector (Power Apps,
Custom connectors, Edit, General, Host), save the connector, then remove
the Northern Lights tool from the agent and add it again. Connector changes
take effect only after the tool is re-added. Also regenerate the
`.local.yaml` copy so it matches.

A dead tunnel is easy to miss from the chat. If the agent has web search or
general knowledge turned on, it answers from those sources instead of
reporting a tool error. Turn both off on the test agent.

## Step 1: import the connector

- [ ] In Edge, open Power Apps and select the environment `dev-as-d9867869`
      (Developer environment; test-chat messages are not billed).
- [ ] **Required:** sign in with the account that will also build the agent.
      Custom connectors are private to their creator until shared.
- [ ] Custom connectors (in the address bar, replace everything after the
      environment ID with `/customconnectors`), New, Import OpenAPI. Import
      `northern-lights-connector.local.yaml` (the copy with the real host).
- [ ] Pass: the connector saves with a single `POST /mcp` operation marked
      `x-ms-agentic-protocol: mcp-streamable-1.0` and an API-key security
      definition on the `Authorization` header.

## Step 2: add the MCP tool and check discovery

- [ ] In Copilot Studio, create or open the test agent and turn on
      generative orchestration.
- [ ] **Required:** create the agent as "Agente (Estándar)" (standard). The
      other option uses the GitHub Copilot harness, where testing consumes
      Copilot Credits, and an agent cannot be moved between harnesses later.
- [ ] Tools, Add a tool, Model Context Protocol, choose Northern Lights.
- [ ] When creating the connection, enter `Bearer <key>` as the key. The
      server also accepts the raw key; on the first run Copilot Studio sent
      both forms.
- [ ] **Required:** set the tool's credentials to the maker's
      (`connectionProperties: mode: Maker`). With the default, invoker mode,
      the test chat shows "Let's get you connected first" and never calls
      the server.
- [ ] Pass: 7 tools are listed: `workiva_list_spreadsheets`,
      `workiva_read_range`, `workiva_search_fields`, `workiva_get_field`,
      `workiva_update_field`, `workiva_sync_mapping`, `workiva_audit_trail`.
      If any is missing, its schema is being hidden; run
      `go test ./internal/mcpserver/tools -run TestToolSchemas` and report.

## Step 3: bearer flow and actor header (first user)

- [ ] **Required:** on the tool's Inputs, set `nl-actor` as a formula,
      `=System.User.Email`. Entered as text, Copilot Studio sends the literal
      words instead of the email.
      Copilot Studio adds headers only through the connector's OpenAPI
      file; `X-` header names do not work with MCP connectors, which is why
      the header is `nl-actor`.
- [ ] In the test chat, ask the agent to list the Workiva spreadsheets.
- [ ] Pass: the server log (`nl-debug-headers` lines) shows a
      `tools/call` with status 200, `authorization=present` with the key's
      length (with or without a `Bearer ` prefix), `key_sha256` equal to the
      `configured key_sha256` printed at startup, and `nl-actor` equal to the
      signed-in user's email. The tool result shows an `nl_audit_id`.
      A `key_sha256` that differs from the configured one means the
      connection holds the wrong key, whatever the status.
- [ ] Decision: no proxy is needed if the header arrives in either accepted
      format. A proxy is needed only if `Authorization` is absent or
      mangled in the log.

## Step 4: second user and malformed key

Developer environments are intended only for their owner, so sharing the
agent there may fail. On 2026-09-17 this check was not possible in
`dev-as-d9867869` and moved to the Sandbox phase. If sharing fails, run it
in a Sandbox environment (or `Sic Mundus Inc. (default)`), repeating the
Required setup from steps 1 to 3 there.

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
- [ ] Pass: each Northern Lights tool call in the query results lines up by
      time, tool and user with a Northern Lights audit record
      (`bin/workiva-mcp audit export -db ./northern-lights.db`, or the
      `workiva_audit_trail` tool). Every `nl_audit_id` shown in the test
      chat's activity view matches an audit record, and for each confirmed
      write that record also carries the Workiva operation URL.
      Application Insights does not store tool outputs, so `nl_audit_id`
      itself does not appear in the query results; matching there is by
      time until `traceparent` is recorded on audit rows (`BACKLOG.md`).

## Hand back

Report: the tunnel URL used, which boxes passed, the actors seen in the log,
and whether a proxy is needed. Leave `cloudflared` running; stopping it
changes the URL, and the connector Host must then be updated and the tool
re-added to the agent.
