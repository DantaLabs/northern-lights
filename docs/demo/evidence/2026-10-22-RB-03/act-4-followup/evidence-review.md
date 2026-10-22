# RB-03 evidence review — 22 October 2026

Reviewed the demo runbook and the saved evidence folder. No tests, browser actions, API calls, or other operations were run for this review.

## Main evidence checklist

1. **PASS — Act 1 comparisons.** The four final prompt records and tool trace document the comparisons; the UI comparison and source screenshots support the reported matches. Evidence: `../prompt-1.1-round2.txt`, `../prompt-1.2-round2.txt`, `../prompt-1.3-round0.txt`, `../prompt-1.4-round0.txt`, `../act-1-tool-trace.json`, `../act-1-ui-comparison.json`, `../act-1-1.png` through `../act-1-4.png`.
2. **PASS — Act 2 findings before approval.** The final prompt record and Act 2 evidence document the findings and ordering before the approval question. Evidence: `../prompt-2.1-round7.txt`, `../act-2.json`, `../act-2.png`.
3. **PASS — Act 3 writes and document content.** Audit matches and value comparison support completion of the 25 field writes, while the section evidence shows the approved environmental content. Evidence: `../act-3-audit-matches.json`, `../act-3-values-comparison.json`, `../act-3-values.png`, `../act-3-section.png`.
4. **PASS — Act 3 narrative rules.** The final narrative and value comparison support the reported movement-first structure and use of the supplied cause without introducing missing-water facts. Evidence: `../act-3-round3-result.json`, `../act-3-values-comparison.json`, `../prompt-3.2a-round3.txt`.
5. **FAIL — Strict Act 4 complete audit presentation.** The follow-up proves both invalid candidates reached server validation and were rejected, but the final comparison has only 33 exact rows out of 43, with 10 absent; the newer screenshots do not establish complete presentation. Tom separately accepted these results with the 33/43 limitation, which records acceptance but does not change the strict checklist result. Evidence: `checklist.md`, `audit-comparison.json`, `technical-review.md`, `guard-results.json`, `guard-tool-trace.json`, `tom-acceptance.md`, `audit-batch-5-fenced-2.png`, `audit-batch-6-fenced-2.png`.
6. **PASS — Act 5 social section.** The final prompts, value comparison, and section evidence support the completed 16 fields and approved narrative/policy. The section screenshot has a visible template-logo overlap, which limits presentation quality but does not contradict the reported content match. Evidence: `../prompt-5.2a-round0.txt`, `../prompt-5.3-round0.txt`, `../act-5-values-comparison.json`, `../act-5-values.png`, `../act-5-section.png`.
7. **PASS — Timing evidence (informational).** Prompt timings and responses over 30 seconds are recorded; under Tom's clarified scope the former eight-minute target is informational, while the historical failed timing verdict remains documented. Evidence: `../act-times.json`, `../response-time-selections.json`, `../response-times.json`, `../response-times.md`, [RB-03 runbook](../../../../demo/runbooks/RB-03-demo-acts.md).
8. **NOT SHOWN — No secret in the recording and screenshots.** No recording is present, so the recording criterion cannot be checked; the text and image scans report no exposed secret. Evidence: `../redaction-scan.json`, `../screenshot-review.md`, `redaction-scan.json`, `screenshot-review.md`.

## Act 4 follow-up checklist

- **PASS — Literal invalid policy submitted to server validation.** The saved prompt and guard trace show a staging call and server rejection. Evidence: `prompt-4.1a-round0.txt`, `guard-policy.png`, `guard-tool-trace.json`.
- **PASS — Invented 9% submitted to server validation.** The saved prompt and guard trace show a staging call and server rejection. Evidence: `prompt-4.2a-round0.txt`, `guard-number.png`, `guard-tool-trace.json`.
- **PASS — Server-call attribution and no Workiva writes or previews.** Audit matching and before/after result records support server-side rejections with no mutation or preview increase. Evidence: `guard-audits.json`, `guard-results.json`, `before.json`.
- **PASS — Production instructions restored before audit requests.** The production and test instruction artifacts show the temporary negative-test instructions were removed before audit collection. Evidence: `production-instructions-final.txt`, `production-instructions.txt`, `test-instructions.txt`.
- **PASS — Complete audit data reached the tool response.** The copied tool result records 100 entries, including all 43 requested rows; this establishes tool output completeness, not model-context completeness. Evidence: `tool-result-size-check.json`.
- **FAIL — Strict 43-row Copilot presentation criterion.** Exact comparison found 33 matching rows and 10 rows not shown; the unsettled parallel prompts have no settled answer artifact. Tom explicitly accepted the partial result (33/43, 76.7%) with the limitation and did not request another collection; this acceptance does not convert the strict row-coverage result to PASS. Evidence: `audit-comparison.json`, `parallel-interruption.json`, `technical-review.md`, `tom-acceptance.md`.
- **PASS — Timing records captured.** Fourteen sends and twelve settled answers are recorded, including ten settled responses over 30 seconds; the two interrupted prompts explicitly lack settled times. Evidence: `response-times.json`, `response-times.md`.
- **PASS — Redaction and screenshot review completed.** The final scan reports no private value matches and no visible credentials or tokens. Some audit IDs and hashes are visible as metadata. Evidence: `redaction-scan.json`, `screenshot-review.md`.
- **PASS — Server and tunnel stopped.** The shutdown record reports successful stop and closed local port. Evidence: `shutdown.json`.
- **NOT SHOWN — Tom's recorded run.** This folder contains screenshots and tool evidence but no recording. Evidence: `checklist.md`, `screenshot-review.md`.

## Secret and capture review

No actual token, key, credential, tunnel URL, or document ID was identified in the reviewed evidence. The main-folder scanner reports one bearer-shaped text-pattern hit classified as prose in `fix-rounds.json`, not a credential; visible database audit IDs are metadata. The Act 4 audit screenshots have capture-position limits: older batches 1–4 may not show their matching answers, and the later batch 5/6 frames show answer segments rather than proving full-result completeness. The pending greeting screenshots show a reset state; `pending-i-visible-greeting.png` is blank and does not support a claim. Historical answer recapture is not shown because the chat history was no longer visible.

## FAIL and NOT SHOWN items

- **FAIL (strict criterion; accepted with limitation by Tom):** Act 4 complete audit presentation — 33 of 43 exact rows shown; 10 not shown. Tom approved the partial evidence and no optional rerun was requested; the 10 rows remain NOT SHOWN.
- **NOT SHOWN:** No-secret recording criterion — no recording is present.
- **NOT SHOWN:** Tom's recorded run.
