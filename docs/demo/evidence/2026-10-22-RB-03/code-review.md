# Code review: section text JSON input

Result: no blocking findings in the current Go diff.

Reviewed `internal/mcpserver/tools/section_text.go` and its test diff against `AGENTS.md`, the demo scope and the authorized compatibility fix. This is a follow-up review by the implementing agent, not an independent review. No source edits or test runs were made during this review.

- Scope remains the two section text tool files. Existing array tests and schema checks are unchanged. No frozen package, registration, dependency or confirmation code changed.
- Registration still adds the four demo tools only when `DemoPipelineEnabled` is true (`NL_DEMO_PIPELINE=true`). Existing checks retain 13 tools with the flag off and 17 with it on.
- The schema requires `section_id` and offers optional `blocks` or `blocks_json`. The handler requires exactly one representation; raw JSON fields preserve property presence. Missing, both, null and empty inputs are rejected.
- `blocks_json` is a string bounded to 100,000 characters. Strict JSON decoding rejects malformed input and trailing values. The decoded value passes the same array schema as existing clients before typed decoding: 1–20 blocks, required `kind` and `text`, valid kinds, no additional block properties, text up to 4,000 characters and verbatim IDs up to 128 characters.
- Both representations reach the existing service through the same dependency and authenticated actor checks. Prepared facts for the actor and section, numeric facts and exact approved verbatim ID/text remain mandatory. The adapter does not fill in verbatim content. Preview, persistence and confirmation behavior are unchanged.
- Added tests cover JSON stage/confirm success, audit ID presence, read-back, guard refusals, schema failures, representation conflicts and accepted/rejected bounds. Refusal tests assert that no preview was persisted.

The existing [checks.json](checks.json) records passing focused and package race tests, the full race suite, vet and formatting. Its first full-suite attempt failed because the task-specific `TMPDIR` conflicted with existing OS temporary storage assertions; the subsequent run with standard OS temporary storage passed, with those checks unchanged. These recorded results were read during review and were not rerun.

The optional nested `blocks` input remains advertised for existing clients; Copilot behavior requires browser evidence. The parent reports that Act 3 JSON staging and confirmation passed; review of that browser evidence is outside this code review.
