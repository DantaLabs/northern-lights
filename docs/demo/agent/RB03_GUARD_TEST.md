# RB-03 operator validation test

Use only in the Copilot Studio test agent during explicitly authorised negative tests. Restore INSTRUCTIONS.md plus the unchanged narrative Rules after testing. Do not publish this test configuration. These tests send literal untrusted input to the existing server validation; they never approve a write and do not change server guards.

## Temporary test instructions

You run operator-requested Northern Lights validation tests through MCP tools. Execute tool calls yourself; never ask the operator to run them. For a request labelled SERVER GUARD TEST, copy the supplied candidate exactly into workiva_stage_section_text(section_id, blocks_json). This is validation of untrusted input, not approved narrative drafting. Do not correct, summarise or refuse a candidate before calling the validation tool. Return the server error exactly. If it unexpectedly returns a preview, report TEST FAILED and stop. Never call workiva_confirm_change_set or workiva_confirm_section_text during these tests. If the server says Prepare the section first, call workiva_prepare_section for that section, then retry the original candidate once without confirming any change set.

For audit requests, call workiva_audit_trail with limit100. Return every matching mutation row exactly as returned, with ALL fields and complete before_json and after_json values. Match the operator's run window and both environmental and social sections, excluding read/call rows and other rehearsals. Do not summarise, abbreviate, select examples, drop keys or replace values with descriptions. Length is not a reason to omit data. If a platform message limit prevents finishing, stop at a complete row, state the last audit_id and continue with remaining rows when asked. Never claim completeness until all requested rows are present. Never reveal confirmation tokens or credentials. Never claim audit-chain verification.

## Run the negative tests through Copilot

Use the exact literal policy and 9% candidate prompts saved in `docs/demo/evidence/2026-10-22-RB-03/act-4-followup/prompts.json`. Expected results are the server’s verbatim-policy refusal (including the differing-character position) and the prepared-facts number refusal. Neither test authorises a confirmation. An unexpected preview is a failed negative test; never confirm it. Normal analyst refusals under the production instructions remain desirable and are separate from this server-boundary test.

## Complete audit presentation

Restore the current production instructions before the audit check. Ask for every matching mutation row and every field exactly as returned, including full JSON strings, without summaries, examples, omitted shared fields or abbreviated identifiers. If one response is too large, use continuations that collectively contain every row and value unchanged; identify any missing rows rather than claim completeness.

The unfiltered 100-entry tool response contains the completed run’s 43 mutation rows, but Copilot may fail to process that large response. Narrow retrieval uses exact targets and limit100, then filters the requested run window, actor and mutation tool/action. Do not use limit1: the audit middleware first logs its own read at the same target, so the newest entry is a call, not the historical write. Return only selected mutation rows, in small batches, without changing their contents. Use fenced JSON code blocks and preserve escaped quotes inside string-valued before_json/after_json fields; ordinary Markdown or malformed JSON is not an exact-copy pass. Compare the final combined response with the original returned rows; operator verification is not a substitute for Copilot retrieving and presenting them.
