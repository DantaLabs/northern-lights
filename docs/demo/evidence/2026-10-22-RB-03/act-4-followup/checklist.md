# Act 4 follow-up — 22 October 2026

Tom requested explicit invalid inputs through Copilot and complete audit presentation without summaries. The earlier evidence remains in the parent folder; this follow-up does not erase failed attempts. Timing is informational under Tom's revised recording plan.

| Check | Result | Evidence and reasoning |
|---|---|---|
| Copilot submits the literal invalid policy to server validation | PASS | `prompt-4.1a-round0.txt`, `guard-policy.png`, `guard-tool-trace.json`: the stage call rejected the changed approved wording at character 1. |
| Copilot submits the literal invented 9% to server validation | PASS | `prompt-4.2a-round0.txt`, `guard-number.png`, `guard-tool-trace.json`: the stage call rejected a number absent from prepared facts. |
| Rejections are actual server calls rather than operator simulation | PASS | `guard-audits.json`, `guard-results.json`: both returned audit IDs match staging calls. The operator did not call the staging API. |
| Negative tests make no Workiva writes or section previews | PASS | `before.json`, `guard-results.json`: mutation and preview counts are unchanged. Neither confirmation tool was authorised. |
| Normal instructions restored before audit testing | PASS | `production-instructions.txt`, `test-instructions.txt`: the temporary negative-test configuration was replaced before audit requests. It was not published. |
| Complete audit data reaches the tool response | PASS | `tool-result-size-check.json`: the copied tool result contains 100 complete entries, including all 43 requested mutations. This does not prove what context the model received. |
| Copilot presents every field of all 43 mutation rows unchanged | FAIL | `audit-comparison.json`, `parallel-interruption.json`: 33 rows match every field exactly; 10 were not shown. Six sequential batches returned 30 rows; the backward parallel chat returned three. Both pending test feeds then displayed only a greeting. |
| All prompt times recorded, including responses over 30 seconds | PASS | `response-times.json`, `response-times.md`: 14 sends, 12 settled answers, 10 settled responses over 30 seconds. Two interrupted prompts have send times and observed wait, with settled time and response seconds explicitly NOT SHOWN. |
| Final redaction scan and screenshot review | PASS | `redaction-scan.json`, `screenshot-review.md`: 41 text files and 16 PNGs reviewed with no exposed secret. Screenshot positioning and historical recapture limitations are explicitly recorded; no recording was reviewed. |
| Server and tunnel stopped after follow-up | PASS | `shutdown.json`: stop script exited 0, local port8090 is closed, this run’s ngrok process is stopped, the same SQLite database is retained and mutation count remains118. |
| Tom's recorded run | NOT SHOWN | This follow-up captures screenshots and tool evidence, not a recording. |

## Attempts and diagnosis

- The original analyst prompts were refused by Copilot before staging. Those desirable drafting refusals did not demonstrate server validation. The isolated test instructions in `../../../agent/RB03_GUARD_TEST.md` tell Copilot to send the untrusted literal candidate to validation and never approve a write.
- The first complete-audit request produced a claim of truncation. The tool UI contained all requested records; a server return limit is not established by that claim. Model-context truncation versus model interpretation remains unproven.
- Exact-target retrieval with `limit: 1` failed because the audit middleware records its own read before the handler returns. The newest row was the read call. Corrected retrieval uses `limit: 100` and filters the run window, actor and mutation actions.
- Plain rendered JSON contained unescaped quotes in string-valued `before_json` and `after_json`. It failed exact-copy verification. Mandatory fenced JSON with escaped strings fixed the first six batches.
- Five cumulative Act 4 fix rounds are exhausted. Remaining batches continue the fifth strategy without further fixes. Any failed continuation stops the run.
- Verification compares Copilot's actual answer fields against the original records. It does not insert database data into the answer, and it does not verify audit-chain integrity.

## Scope

The requested run has 41 completed field writes and two completed section writes, under the fixed API-key demo service actor. The negative tests use the same server and SQLite database. Audit reads add local call rows but make no Workiva writes. Successful output across continuations is distinct from an ordinary single prompt displaying all rows unaided.

## Parallel follow-up requested by Tom

After 30 exact rows, Tom requested splitting the audit follow-up from both ends. A second independent test pane was checked before sending: the original retained its history and the new pane was blank. One browser operator serialized tab selection while two Copilot requests ran concurrently. The backward final-three-row batch passed in 37.821 seconds; the next backward five-row request and the original forward five-row request did not settle. Exact-tab inspection later showed only a greeting in both panes, with neither pending prompt nor history visible. The local poller was terminated without resending. Cause is not established; this is a browser/application conversation-state or harness interruption, not proof that the server rejected audit retrieval. No additional fix round was attempted. See `parallel-interruption.json` and the greeting observations.

Screenshot review found older batch captures at the wrong transcript scroll position. The full saved response texts establish the exact comparisons. Corrected later captures are identified in the screenshot review; unavailable historical recaptures must remain NOT SHOWN, not be reconstructed.

## Final browser and capture state

The 7,964-character production instruction source was restored and the editor matched it. `production-instructions-final.txt` records that source; `production-instructions.txt` remains the earlier restored version rather than being silently overwritten. Focus emulation and device metrics were cleared on both tabs; no agent was published and no user tab was closed. `pending-h-visible-greeting.png` and `pending-i-greeting-message.png` support the visible reset state. `pending-i-visible-greeting.png` is an earlier blank viewport capture and is not supporting evidence. Historical answer recapture is NOT SHOWN because the histories were no longer visible.

## Acceptance decision

**ACCEPTED BY TOM, WITH RECORDED LIMITATION.** See [Tom’s acceptance](tom-acceptance.md). The strict 43-row presentation check above remains historically FAIL at 33/43; Tom explicitly accepted this audit evidence despite the 10 rows NOT SHOWN. No further collection is required for this run.
