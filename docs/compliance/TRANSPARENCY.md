# Transparency Notice

This document is provided per EU AI Act Art. 13 (transparency
obligations for providers of AI systems).

## What Northern Lights does

Northern Lights connects a natural-language interface (such as Microsoft
Copilot) to your organisation's Workiva spreadsheets. It lets authorised
users ask questions in plain language and receive live data, or request
spreadsheet edits that are applied through the Workiva API.

## What data is processed

- **Your Workiva spreadsheet data** is read and, if requested, written
  through the official Workiva API. All data remains in your Workiva
  workspace; Northern Lights does not transfer it to any third party.
- **Field mappings** (how spreadsheet cells relate to named reporting
  concepts) are stored locally in a SQLite database on your
  infrastructure.
- **Audit records** of every tool call and data mutation are stored in
  the same local database.

## What is NOT sent externally

Northern Lights never sends your data to its developers, to any cloud
service other than the Workiva API, or to any analytics provider. The
only outbound network traffic is:
1. OAuth2 token requests to Workiva's identity service.
2. Spreadsheet read/write requests to the Workiva API.
3. Responses to the MCP client (your copilot).

## How to verify what happened

Every read and write is recorded in a tamper-evident audit log. Ask your
administrator to run:

```bash
./workiva-mcp audit export -db /path/to/northern-lights.db -format jsonl
```

This produces a line-by-line record showing who requested what, when,
and what changed.

## Your right to human review

When write confirmation is enabled (the default), every spreadsheet edit
requires explicit approval before it is applied. You can request a
review of any edit by consulting the audit log entry for that mutation.
If you believe an edit was made in error, contact your reporting team
administrator to reverse it in Workiva.

## Contact

For questions about Northern Lights, its data handling, or to report a
concern, see [SECURITY.md](../SECURITY.md).
