# Security

## Credential handling

- `NL_API_KEY`: set via environment variable only, never in YAML or
  committed files. Used to authenticate MCP clients (Copilot) to
  Northern Lights via bearer token.
- `NL_WORKIVA_CLIENT_ID` and `NL_WORKIVA_CLIENT_SECRET`: set via
  environment variables only. Used to obtain OAuth2 access tokens from
  Workiva's token endpoint. Tokens are cached in memory only and never
  written to disk.

Northern Lights does not store credentials in its SQLite database.

## Workiva API grant scopes

Your Workiva API grant must include at minimum:
- `file:read` for read tools and field discovery.
- `file:write` for the update_field tool.

Scopes are configured by your Workiva workspace administrator.

## Known security considerations

- The MCP endpoint (`/mcp`) uses bearer token authentication. Deploy
  behind TLS in production.
- The SQLite database contains field mappings, cached cell values, and
  the audit log. Protect it like any sensitive data store.
- No outbound traffic except to the Workiva API and its identity
  service. No telemetry, analytics, or phone-home behaviour.

## Reporting vulnerabilities

If you discover a security issue, please report it responsibly:
- Email: admin@vadian.dev
- Do not open a public GitHub issue for security vulnerabilities.
- We will acknowledge within 48 hours and aim to patch within 7 days.
