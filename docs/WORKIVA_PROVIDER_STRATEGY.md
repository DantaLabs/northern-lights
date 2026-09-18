# Workiva Provider Strategy

Status: Accepted

## Decision

Northern Lights remains the primary Workiva implementation. Its seven public MCP tools, semantic mapping, confirmation flow, policy enforcement, and audit contract remain stable regardless of the upstream transport.

The official Workiva MCP server is treated as an optional provider, not as a replacement roadmap commitment. We adopt an official capability only when evidence shows that it is more reliable or strategically unavoidable and preserves Northern Lights' governance guarantees.

## Architecture

`internal/workivaprovider` is the transport boundary used by Northern Lights tools.

- `Reader` covers discovery and spreadsheet reads.
- `Writer` covers accepted mutations and operation polling.
- `Backend` combines both capabilities.
- `Router` sends all capabilities to the Northern Lights REST client by default.
- Read and write backends can be replaced independently.

This allows an eventual official-MCP adapter to handle read-only functions while writes remain on the proven REST path. Tool names and schemas presented to Copilot do not change when a backend changes.

## Current routing

| Capability | Primary provider | Fallback |
|---|---|---|
| Spreadsheet discovery | Northern Lights REST | None |
| Sheet discovery | Northern Lights REST | None |
| Range and field reads | Northern Lights REST | None |
| Mapping synchronization | Northern Lights REST | None |
| Controlled writes | Northern Lights REST | None |
| Async operation polling | Northern Lights REST | None |
| Audit, confirmation, RBAC | Northern Lights | Not delegated |

No runtime switch to the official MCP exists yet. This prevents accidental dependency on a beta service.

## Rules for an official MCP adapter

1. Implement the smallest capability interface required. Do not bypass the provider router.
2. Translate official MCP responses into Northern Lights domain types at the adapter boundary.
3. Keep mapping, approval, authorization, audit IDs, and hash-chain evidence inside Northern Lights.
4. Use explicit per-capability configuration. Never silently fall back from a failed write to another provider because mutation outcome may be unknown.
5. Require contract tests against the live official tool catalog before enabling a route.
6. Preserve the REST backend as rollback until the official capability has completed a production observation period.

## Activation gates

An official capability may replace a Northern Lights REST capability only when:

- Workiva documents it as stable enough for the target deployment.
- Authentication and tenant identity can be validated end to end.
- Response contracts, pagination, rate limits, and error behavior are captured by tests.
- The capability preserves data residency and least privilege.
- Copilot acceptance and audit correlation pass.
- Operational metrics show a concrete advantage over the REST implementation.
- Rollback to REST is tested.

## Official MCP validation status

The EU official MCP endpoint authenticated and exposed its current tool catalog. The authenticated `list_workspaces` call returned `access denied: this user is not enabled for the MCP gateway`. Workiva must enable the test user before an official-provider live acceptance run can execute. This entitlement block is separate from the provider router, which is locally verified.

## Planned adapter work, not yet scheduled

- Obtain MCP Gateway entitlement for the test user.
- Inventory and contract-test the live official Workiva MCP tool catalog.
- Build an MCP client adapter for supported read capabilities.
- Add provider selection configuration and startup capability checks.
- Add shadow-read comparison mode with redacted mismatch metrics.
- Run a pilot before routing production reads.
- Evaluate official writes separately; never infer write safety from read success.
