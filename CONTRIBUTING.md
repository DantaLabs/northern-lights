# Contributing to Northern Lights

## Adding a new tool

Northern Lights uses a simple tool interface. To add a new MCP tool:

1. Create `internal/mcpserver/tools/your_tool.go`.
2. Define an input struct with `json` and `jsonschema` tags (the MCP
   SDK uses these to generate the tool schema Copilot sees).
3. Implement the `mcpserver.Tool` interface:

```go
type Tool interface {
    Name() string
    Description() string
    RegisterSDK(s *mcp.Server, deps Deps)
}
```

4. `RegisterSDK` calls `mcp.AddTool[YourInput, YourOutput](s, &mcp.Tool{
   Name: t.Name(), Description: t.Description(), InputSchema: ...}, handler)`.
5. Register the tool in `cmd/workiva-mcp/main.go` by adding
   `reg.Register(tools.YourTool{})` to the registry.
6. Write a test in `your_tool_test.go` using the existing test helpers
   in `internal/mcpserver/tools/test_helpers_test.go`.

A minimal read tool (no external calls, no audit) is under 50 lines.
See `list_spreadsheets.go` for the simplest example.

## Code style

- `gofmt` on all Go files.
- No em dashes in code, comments, docs, or tool descriptions. Use
  commas or parentheses instead.
- TDD: write the failing test first, implement, verify green, commit.
- Never commit with failing tests.

## Running tests

```bash
export PATH=$HOME/sdk/go/bin:$PATH
go test ./... -race -count=1
```

## Lint

```bash
make lint   # go vet + gofmt -l
```

## Issues

Use GitHub issues. Include:
- Steps to reproduce (for bugs).
- Expected vs actual behaviour.
- Workiva API version in use.

## Security

See [SECURITY.md](SECURITY.md).
