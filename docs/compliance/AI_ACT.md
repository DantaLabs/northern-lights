# EU AI Act Risk Classification

## Product overview

Northern Lights is an open-source MCP server that mediates between a
large language model (via Copilot Studio) and the Workiva platform. It
maps reporting field names to spreadsheet cells, translates natural-language
queries into API calls, and maintains a hash-chained audit log of every
tool invocation and data mutation.

## Risk classification under the AI Act

Northern Lights itself is not an AI system within the meaning of the
AI Act. It is a deterministic software tool (rule-based mapping, exact
API calls, hash-chain logging) that an AI system (the Copilot LLM) calls
as an external tool.

The deployer (the organisation operating Northern Lights alongside
Copilot Studio) is the party responsible for AI Act obligations relating
to their use of the AI system. Northern Lights is designed to make that
compliance easier, not to substitute for it.

### High-risk assessment (Art. 6, Annex III)

Northern Lights is not listed in Annex III because it is not used for:
- biometric identification or categorisation
- management of critical infrastructure
- education or vocational access
- employment or worker management
- access to essential private or public services
- law enforcement
- migration, asylum, or border control
- administration of justice or democracy

However, the deployer's use of AI-assisted reporting may fall under
governance and compliance obligations (Art. 4) depending on sector
regulation (e.g. EU CSRD sustainability reporting). Northern Lights
supports compliance in those cases through its audit trail.

### General-purpose AI model (GPAI) considerations

Northern Lights is not a GPAI model. It is a tool invoked by a GPAI
(Copilot/GPT). The GPAI provider (OpenAI/Microsoft) bears the relevant
GPAI obligations under Title VIII. The deployer bears the obligation to
ensure human oversight of AI-assisted decisions (Art. 14).

## What Northern Lights provides to support compliance

| AI Act Article | Requirement | Northern Lights implementation |
|---|---|---|
| Art. 12 | Record-keeping | Hash-chained audit log in SQLite, exportable as JSONL, integrity-verifiable via CLI |
| Art. 13 | Transparency | This document, tool descriptions visible to the LLM, public source code |
| Art. 14 | Human oversight | Two-phase write confirmation (configurable, on by default); all writes audited before and after |
| Art. 15 | Accuracy / robustness | Rate limiting, retry handling, Ones-scale caveat documented; no LLM interpretation of raw data values |

## What Northern Lights does NOT control

- The LLM's interpretation of user queries (Copilot/GPT behaviour).
- The deployer's Workiva permission model (who can access what data).
- Network security between the deployer's environment and the Workiva API.
- The accuracy of source data in Workiva spreadsheets.

These remain the deployer's responsibility.
