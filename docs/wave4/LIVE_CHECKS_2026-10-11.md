# Wave 4 live checks — 2026-10-11

Luna workers performed browser and provider/storage checks; the coordinator reviewed their evidence and performed live runtime checks. This run does not close Wave 4.2 release acceptance.

| Check | Observed result | Limit |
| --- | --- | --- |
| Live service health and readiness | Both HTTP 200 | Availability only |
| Unauthenticated MCP initialize | HTTP 401, `authentication_required` | One unauthenticated request; not actor-isolation acceptance |
| Synthetic PDF attachment | Copilot test composer accepted the exact repository fixture; browser-side SHA-256 matched | Attachment is not analysis or authenticated source persistence |
| Copilot analysis | Send displayed `Unable to connect`; refresh recovery subsequently showed a blank page | No analysis result or attributable domain-tool trace; no independent no-write proof |
| Content runtime registration | Current branch registers the existing tool catalog; no production content handler is enabled | End-to-end placement remains unavailable |
| Workiva discovery and typed metadata | Read-only discovery and an uncached `B4` metadata read on the disposable scratch sheet passed | Protection and writability remain unknown; the configured writer contract is unavailable |
| Azure immutable fence objects | Claim/terminal creation, exact verification, terminal discovery and mismatched duplicate rejection passed | Two synthetic digest-only objects remain in a unique test environment prefix |
| Azure complete bounded inventory | Initially failed; after the ETag representation fix, worker and coordinator independently verified exactly two objects in the same isolated prefix | Adapter behavior only; complete application restore admission remains open |

The fixture is `docs/wave4/fixtures/revenue-table-injection.pdf`, SHA-256 `36fd8b46f80147ddd6e7185b8f9988973b45dc69a290a080ad4768ddac869af2`. Kimi's direct upload failed because file-URL access was off; an in-page `File`/`DataTransfer` upload successfully attached the same bytes without changing browser settings. The screenshot after Send shows an empty chat, not the transient error. The recovery screenshot shows the blank page. Those images do not independently prove the error or absence of provider writes.

GitHub run `38060337391` passed all package race tests. Lint failed because the v2.13.2 release binary could not decode Go 1.27 export data. The CI repair pins v2.14.0 while preserving every test and check. Its [official release workflow](https://raw.githubusercontent.com/golangci/golangci-lint/v2.14.0/.github/workflows/release.yml) builds with Go 1.27.0. The official Linux amd64 archive matched its published checksum (`ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab`); that release binary reports Go 1.27.0 and passes lint with zero issues, independently rerun by the coordinator. Vet and whitespace checks also pass. A new GitHub CI result remains separate from local validation.

Authenticated source-byte handoff, provider protection/writability and literal-write preservation, restore admission, source encryption/backup/retention, operator policy selections and content handler integration remain open. A successful storage adapter check would not close those gates.

The Azure failure came from equivalent ETags: the live list result omitted outer quotes and the download header included them. Last-modified timestamps, lengths and metadata matched. The fix accepts balanced quoted/unquoted forms only when their strong opaque tags are identical; it still rejects weak, malformed, control-containing, whitespace-containing, oversized or different tags. Regression coverage reproduces the actual service representation. Exact bytes, metadata, length and strict-before high-water checks remain enforced. Microsoft documents the [list response's Etag property](https://learn.microsoft.com/en-us/rest/api/storageservices/list-blobs) and [quoted download-header ETags](https://learn.microsoft.com/en-us/rest/api/storageservices/get-blob).
