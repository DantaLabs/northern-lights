# RB-03a HTTP sandbox dry run — 2026-10-21

Base: `4097d47` (`origin/claude/demo-integration`); offline social test: `8f34317`.

This is the API rehearsal requested for RB-03 acts 1–5. The exact demo prompts are attached to the corresponding HTTP MCP calls. Narrative blocks were drafted by the test operator from the prepared facts. Copilot Studio, Teams, screenshots, conversational answers and browser presentation were not exercised. Authentication used Tom's local API key over loopback; delegated Entra behavior is not established by this run.

- [x] Setup: synthetic seed reset and live Workiva client, sandbox mappings, `NL_DEMO_PIPELINE=true`, and real report document selected through `NL_DEMO_DOCUMENT_ID`. Seventeen tools listed (`step-0.json`, `step-1.json`).
- [x] Act 1 numeric reads: energy 1978/2030 MWh, employees 428/412, business travel 165/140 tCO2e, renewable electricity 490/520 MWh. All eight values match independent Workiva cell reads. Tool results contain the mapped source/prior cell addresses (`step-2.json` through `step-14.json`).
- [ ] Act 1 comparison tool: blocked. The current `workiva_compare_periods` schema requires immutable snapshot IDs and a materiality policy; field reads return none. Assurance is disabled in the local sandbox configuration. No subject IDs were fabricated, and this tool was not called (`step-5.json`).
- [x] Act 2: prepare returned the three expected findings and 25 proposed writes (`step-15.json`). The raw tool list is in recipe order; missing-first conversational presentation remains for Tom to verify.
- [x] Act 3 values: one confirmation completed all 25 environmental fields. Independent live cell reads and each returned per-field audit ID match the proposed values, targets, actor and Workiva operation (`step-16.json`, `step-19.json`).
- [x] Act 3 text: the draft leads with business travel rising 18% to 165.0 tCO2e; the remainder groups indicators without numbers. The preview preserves `env-policy` exactly. Confirmation returned section and audit IDs; live section title/text and audit text hash match the staged blocks (`step-20.json` through `step-22.json`).
- [x] Act 4 guards: edited policy and invented 9% both refused with the expected server messages; independent document reads after each refusal were unchanged (`step-23.json` through `step-26.json`).
- [ ] Act 4 evidence export: blocked. Earlier demo results contain no subject supported by `workiva_export_evidence` (`snapshot`, `validation_run`, `comparison`, `transfer`). The exact prompt permits stating that a subject is needed; no ID was invented and the tool was not called. No artifact or export-chain-verification claim is made (`step-27.json`).
- [x] Act 5: social prepare returned only the expected female-training material finding, with no missing values. All 16 confirmations completed and matched independent cell reads/audit rows. The draft leads with a 16% rise to 25.5 hours, preserves `social-policy`, and the confirmed Social disclosures section matches the draft and audit hash. No code/configuration change or server restart occurred between acts 3–5 (`step-28.json` through `step-33.json`).
- [x] Five-act wall time: **268 seconds**, including the harness interruption, measured from first to last MCP tool audit timestamp at one-second resolution. Seed setup is excluded. Blocked comparison/export were not executed (`step-34.json`, `step-35.json`).
- [x] Final independent audit verification: hash chain valid, exactly 41 per-field write rows and two completed section rows. Missing environmental water-discharge target remains empty with no write audit. No provider/result/audit mismatch found (`step-35.json`).
- [x] Redaction scan: credentials, generated approval tokens (including JSON nested inside MCP content strings), sandbox mapping IDs and the real report document ID are absent from the committed candidates. Private runtime files remain in ignored `.scratch/`.
- [ ] Copilot/Teams/Workiva UI presentation and screenshots — not exercised in this API run.

## Errors and steps above 60 seconds

- Sandbox seed reset: **113.8 seconds**, above the 60-second reporting threshold (`step-0.json`).
- The verification harness stopped after environmental confirmation because an empty `before` property was omitted from the preview JSON (`step-17.json`). The checker was corrected to treat omission as empty. The server was restarted on the same private SQLite database, and verification resumed without repeating writes (`step-18.json`, `step-19.json`). This was a harness error, not a provider mismatch. There was no restart or configuration change between the completed environmental text flow and Act 5.
- Comparison and evidence export remain blocked as described above; these need other lanes. No implementation outside `internal/demoe2e` was changed.

## Verification

- `go test ./internal/demoe2e/... -race -count=1`: pass, including the social test and both refusals.
- `go vet ./...`, `go build ./...`, `gofmt -l .`, and `git diff --check`: pass.
- `golangci-lint run --timeout=10m` with v2.14.0: zero issues.
- Lighter-agent code review: merge recommended. Evidence review: API write/text/guard/read-back/audit checks pass; comparison and export are blocked; conversational findings-first, Copilot answers/citations and UI behavior are not shown. Narrative-style rule 5 interaction remains for Tom.
- `go test ./... -race -timeout=30m -count=1`: pass, 25 tested packages (assurance 636.451s, transfer 542.152s; social/environmental package 12.004s). The longer harness timeout accommodates the existing frozen suites.
