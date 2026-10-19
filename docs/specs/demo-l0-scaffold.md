# Demo L0: scaffold for parallel lanes

## Goal

Create the flag, the tool registration, the shared dependency fields and the interface files for the demo pipeline, so lanes L1 to L4 can work in parallel without editing the same files. After L1 to L4 merge, add one end-to-end test that runs the whole demo flow in demo mode.

## Non-goals

- Implementing change sets, section recipes, text checks or the Workiva document writer. Those belong to L1, L2 and L3.
- Changing any of the 13 existing tools or their tests.
- Touching frozen packages: `internal/content/`, `internal/transfer/`, `internal/backupjob/`, assurance backup and restore.

## Users and scenarios

- An agent working on L1, L2 or L3 checks out `claude/demo-integration` after L0 merges and finds its package, its interface and its tool stub already registered behind the flag.
- Tom starts the server with `NL_DEMO_PIPELINE=true` and `NL_DEMO_MODE=true` and sees 17 tools; without the flag he sees the current 13.

## Requirements

R1: `internal/config` exposes `DemoPipelineEnabled bool`, set from `NL_DEMO_PIPELINE=true` and from YAML key `demo_pipeline_enabled`, default false.
R2: `tools.Demo()` returns four tools in this order: `workiva_prepare_section`, `workiva_confirm_change_set`, `workiva_stage_section_text`, `workiva_confirm_section_text`.
R3: `cmd/workiva-mcp` registers `tools.Demo()` after `tools.All()` only when `DemoPipelineEnabled` is true.
R4: With the flag off, `tools/list` returns exactly the current 13 tools and every existing test passes unchanged.
R5: Each demo tool stub has its final input and output schema (from the L1 to L3 specs) and returns an MCP error result "not implemented yet" until its lane replaces the handler.
R6: `mcpserver.Deps` gains nil-safe fields `ChangeSets changeset.Service`, `Recipes recipe.Service`, `Facts recipe.FactStore` and `SectionText sectiontext.Service`; tools return a clear error when a needed field is nil.
R7: Interface files exist with the types in "Interfaces and data": `internal/changeset/changeset.go`, `internal/recipe/types.go`, `internal/sectiontext/types.go`, `internal/workivaprovider/documents.go`.
R8: `internal/workiva/demo_client.go` routes request paths starting with `/documents` or `/content` to `demoDocumentsResponse` in a new file `internal/workiva/demo_documents.go` (owned by L3), and spreadsheet IDs starting with `demo-esg-` to `demoESGResponse` in a new file `internal/workiva/demo_esg.go` (owned by L4). Both return 404 JSON until their lanes fill them.
R9: In demo mode, when `-mappings` is set, `cmd/workiva-mcp` loads that file after the built-in demo mappings, so L4's `configs/demo/mappings-demo.yaml` can add the ESG fixture fields. Without `-mappings`, demo mode behaves as today.
R10: After L1 to L4 merge, `internal/demoe2e` has one test that, in demo mode with the flag on, calls prepare, confirm change set, stage text and confirm text in sequence and asserts the written values, the section text and one audit row per written field.
R11: `scripts/verify-mcp.sh` reads the expected tool count from `NL_EXPECT_TOOLS`, default 13, so it checks 17 when the flag is on.
R12: `internal/demoenv` defines `Env{DB *sql.DB; Store *mapping.Store; Audit *audit.Log; Client workivaprovider.Backend; Cfg *config.Config}`, and each lane package exposes a constructor with a stub body: `changeset.New(env) changeset.Service`, `recipe.New(env, changeset.Service) (recipe.Service, recipe.FactStore, error)`, `sectiontext.New(env, recipe.Service, recipe.FactStore) sectiontext.Service`. `cmd/workiva-mcp` calls them once when the flag is on and puts the results in `Deps`, so lanes never edit `main.go`.

## Interfaces and data

```go
// internal/changeset/changeset.go
type Item struct{ FieldName, Value, Source string }
type PreviewItem struct{ FieldName, SpreadsheetID, SheetID, Range, Before, After, Source string }
type Preview struct {
    ID, Token string
    Items     []PreviewItem
    ExpiresAt time.Time
}
type ItemResult struct{ FieldName, Status, ReadBack, AuditID, OperationURL string }
type Result struct {
    ID, Status string // Status: completed | partial | reconciliation_required
    Items      []ItemResult
}
type Service interface {
    Stage(ctx context.Context, actor string, items []Item) (Preview, error)
    Confirm(ctx context.Context, actor, id, token string) (Result, error)
}

// internal/recipe/types.go
type Finding struct{ Kind, Field, Severity, Message string } // Kind: missing | out_of_range | material_change | sign_change
type Fact struct {
    Key, Label, Unit         string
    Current, Prior           string // canonical decimal strings
    Change, ChangePct        string // canonical decimal strings, empty when prior is missing or zero
    Direction                string // up | down | flat | new
    Material                 bool
    Display                  []string // every approved display form, e.g. "1.24 GWh", "8%", "8.2%"
}
type VerbatimBlock struct{ ID, Title, Text string }
type Prepared struct {
    SectionID string
    Findings  []Finding
    Facts     []Fact
    Verbatim  []VerbatimBlock
    ChangeSet changeset.Preview
}
type SectionInfo struct{ ID, Title, Period, PriorPeriod, DocumentID string }
type Service interface {
    Prepare(ctx context.Context, actor, sectionID string) (Prepared, error)
    Sections() []SectionInfo
}
type FactStore interface {
    LatestFacts(ctx context.Context, actor, sectionID string) ([]Fact, []VerbatimBlock, error)
}

// internal/sectiontext/types.go
type Block struct{ Kind, Text, VerbatimID string } // Kind: narrative | verbatim
type Preview struct {
    ID, Token, SectionID, SectionTitle string
    Blocks                             []Block
    ExpiresAt                          time.Time
}
type Result struct{ SectionID, DocumentID, WorkivaSectionID, Status, AuditID string }
type Service interface {
    Stage(ctx context.Context, actor, sectionID string, blocks []Block) (Preview, error)
    Confirm(ctx context.Context, actor, id, token string) (Result, error)
}

// internal/workivaprovider/documents.go (optional capability, same pattern as ContentMetadataReader)
type DocumentWriter interface {
    EnsureSection(ctx context.Context, documentID, title string) (sectionID string, err error)
    WriteSectionText(ctx context.Context, documentID, sectionID string, paragraphs []string) (operationURL string, err error)
    ReadSectionText(ctx context.Context, documentID, sectionID string) ([]string, error)
}
```

## Constraints

- No new module dependencies.
- Follow the existing `Tool` pattern in `internal/mcpserver/registry.go` and the optional-capability pattern of `ContentMetadataReader` in `internal/workivaprovider`.
- Tool descriptions are written for Copilot: one sentence on what the tool does and one on when to call it.

## Acceptance scenarios

1. (R1, R3, R4) Start the server in demo mode without the flag: `tools/list` returns 13 tools. Existing tests pass with no edits.
2. (R1, R2, R3) Start the server in demo mode with `NL_DEMO_PIPELINE=true`: `tools/list` returns 17 tools, the last four in the R2 order.
3. (R5, R6) Call each demo tool with valid input before its lane lands: an MCP error result says "not implemented yet"; with a nil dependency it says which dependency is missing; nothing panics.
4. (R7) `go build ./...` and `go vet ./...` pass with the interface files present.
5. (R8) In demo mode, a GET on `/documents/x` returns 404 JSON from `demoDocumentsResponse` and a GET on `/spreadsheets/demo-esg-x/sheets` returns 404 JSON from `demoESGResponse`; existing demo fixture routes are unchanged.
6. (R9) Demo mode with `-mappings` pointing at a test YAML loads both the built-in demo fields and the YAML fields; demo mode without `-mappings` loads only the built-in fields.
7. (R10) The end-to-end test passes on `claude/demo-integration` after L1 to L4 merge.
8. (R11) `verify-mcp.sh` passes against a server without the flag and, with `NL_EXPECT_TOOLS=17`, against a server with the flag on.
9. (R12) With the flag on, the server starts, the three constructors are called once each, and the services reach the tools through `Deps`; lanes can replace stub bodies without touching `main.go`.

## Open questions

None

## Approval

Approved: pending Tom
