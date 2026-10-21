# RB-02 checklist (2026-10-19)

The spike ran on `codex/demo-l3-section-text` from `claude/demo-integration` at 3bdcb35. It used a throwaway program in `.scratch/` (not committed) that calls `internal/workiva` `TokenProvider` and `Client` against the eu sandbox.

- [x] The annual report document was found. See `step-2.json`: `2025 Annual Report - EN` is the only document the grant can see.
- [x] Section creation returned a section ID. See `step-3.json`: `201`, `id` `<document-id>_244`.
- [x] The edit reached a terminal operation status. See `step-4.json`: `202` with a `Location` header, then the poll returned `completed`.
- [x] The read-back matches the two paragraphs. See `step-5.json`: `exact_match: true`.
- [x] The test section was deleted. See `step-6.json`: `DELETE` returned `204`, and the follow-up `GET` returned `404`.
- [x] `result.md` states "document". See `result.md`.
- [x] No token or secret appears in any saved file. Authorization headers are never recorded. All files were checked by grep for `Bearer`, `access_token` and `client_secret`.

The document, section, rich-text and operation IDs are placeholders, because the repository is public. The real values are in the uncommitted `step-*.local.json` files in this folder. `requests.txt` lists every request in order.

Result: **pass**.
