# Agent instructions

Every coding agent working in this repository (Claude Code, Codex, subagents) reads this file first. It is the same file for all of them, so work from different agents fits together.

## What we are building right now

A recorded and live-capable demo for the KPMG Workiva advisory team, due Monday 2 November 2026. An analyst in Teams asks Copilot questions about Workiva data, gets cited answers, prepares the ESG environmental section with one approval, and gets a consultant-quality narrative written into the Workiva report document. Read `docs/demo/DEMO.md` before writing any code.

## Source of truth, in order

1. `docs/demo/DEMO.md`: goal, demo script, scope, frozen work, calendar.
2. `docs/demo/LANES.md`: who owns which files and branches. Never edit a file another lane owns.
3. `docs/specs/demo-*.md`: one spec per lane, with numbered requirements and acceptance scenarios. Your ticket is your lane's spec.
4. `CONTEXT.md`: vocabulary. Use these terms in code, comments and commit messages.
5. `docs/demo/CODEMAP.md`: where the existing code you need lives. If a Graphify output folder (`graphify-out/`) exists in your checkout, read its report too.
6. `docs/adr/`: decisions already made. Do not reopen them without asking Tom.

`BACKLOG.md`, `docs/validation/` and `docs/wave4/` are history. Read them only when a spec points to them. Do not add to them.

## Rules

1. Work only on your lane branch, inside the files your lane owns. If you need a change in another lane's file, write it in the "Requests to other lanes" section of `docs/demo/STATUS.md` and continue with a fake or stub.
2. Never push to `main`, never force push, never merge pull requests. Tom merges. Lane pull requests target `claude/demo-integration`.
3. Never weaken a test, lint rule or check to get a green result.
4. Pacing against Workiva is the job of the server's rate limiter (600 requests per minute, retries on 429). Do not ration, batch by hand or refuse calls to "avoid overloading the server". Sandbox writes are allowed on the scratch ranges listed in `docs/demo/DEMO.md`.
5. New demo tools stay behind the `NL_DEMO_PIPELINE=true` flag. With the flag off, the server must expose exactly the current 13 tools, so existing tests stay unchanged.
6. Do not add a model or LLM call inside the server. Copilot drafts text; the server computes numbers, checks text and writes. See `docs/adr/0001-copilot-drafts-server-verifies.md`.
7. Wave 4.2 content placement, Wave 4.3 workflows, Wave 5 and the official MCP adapter are frozen until after the demo. Do not modify `internal/content/`, `internal/transfer/`, `internal/backupjob/` or the assurance backup and restore files.

## Testing

1. While working: `go test ./internal/<your package>/... -race`, `go vet ./...`, `gofmt -l .` (must print nothing).
2. The full suite (`go test ./... -race`) and golangci-lint v2.14.0 run in CI on every pull request. Run them locally only before opening the pull request.
3. Tests against the live Workiva sandbox through the API are allowed when your spec asks for them and Tom's local `.env` is present. Never print tokens or secrets.
4. Browser and Copilot Studio tests are done by Tom, not by agents. If your work needs one, add or update a runbook in `docs/demo/runbooks/` and stop there. Tom drops evidence in `docs/demo/evidence/` and a reviewer agent checks it.
5. Do not write new validation reports, SHA-256 receipts or acceptance narratives. A green CI run, the spec's acceptance scenarios as tests, and runbook evidence are enough for the demo.

## Reporting

When you finish a work session, update your lane's row in `docs/demo/STATUS.md`: status, branch, last commit, what is blocked. One line each. Commit messages say what changed and why, in plain words.
