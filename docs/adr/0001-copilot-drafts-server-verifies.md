# Copilot drafts, the server verifies and writes

For the KPMG demo and pilot, the server does not call any language model. Copilot (inside the firm's Microsoft tenant) asks questions and drafts narrative text; Northern Lights computes every number, checks drafted text against verbatim blocks and narrative facts, stages one change set per section and writes to Workiva after one human approval. We rejected server-side AI extraction in an Azure container because it adds a new model endpoint and a new data flow for the firm's IT team to review, and it would take weeks to build. Copilot already holds the firm's approval to process this data, so keeping the model there keeps the IT review to components that already exist.

## Addendum, 27 October 2026: arithmetic in chat answers

Decided by the owner after the first Teams run, where answers felt robotic and off-script questions were refused. In chat answers only, Copilot may do simple arithmetic across values that tools returned (sums, shares, differences), and must label the result as calculated and name the cells it used. Period comparisons still use the server's `workiva_compare_fields`. Nothing written to Workiva changes: change sets come from the section recipe, and the number guard still refuses any number in report text that is not a narrative fact, so a figure Copilot calculated cannot reach the report document.
