// Package mcpserver exposes the northern-lights Workiva functionality as an
// MCP server over streamable HTTP. It is also the public extension point:
// contributors implement Tool and register it on a Registry without
// touching the transport, auth, or audit plumbing.
package mcpserver

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/workiva"
)

// Deps bundles the services available to tools. Fields may be nil in tests
// that do not need them; tools should fail gracefully with a clear error
// rather than panic when a dependency they require is missing.
type Deps struct {
	Client *workiva.Client
	Store  *mapping.Store
	Audit  *audit.Log
	Cfg    *config.Config
}

// Tool is one MCP tool, decoupled from the SDK transport. A tool owns its
// typed schema and handler via RegisterSDK, which keeps the generic
// mcp.AddTool call next to the argument struct it unmarshals into.
//
// Implementations must use the same name in Name() and in the mcp.Tool
// passed to AddTool; Bind checks for collisions across registered tools.
type Tool interface {
	// Name is the MCP tool name, e.g. "workiva_search_fields".
	Name() string
	// Description is the human and LLM facing summary shown in tools/list.
	Description() string
	// RegisterSDK binds the tool onto an SDK server. deps carries the
	// shared services (Workiva client, mapping store, audit log, config).
	RegisterSDK(s *mcp.Server, deps Deps)
}

// Registry holds the tools served by one MCP server. It is the public
// extension point: create a Tool and call Register.
type Registry struct {
	tools  []Tool
	byName map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Tool)}
}

// Register adds a tool. Registering two tools with the same name is
// reported as an error by Bind, not here, so tooling can register
// unconditionally at startup.
func (r *Registry) Register(t Tool) {
	r.tools = append(r.tools, t)
	r.byName[t.Name()] = t
}

// Names returns the registered tool names in registration order.
func (r *Registry) Names() []string {
	names := make([]string, len(r.tools))
	for i, t := range r.tools {
		names[i] = t.Name()
	}
	return names
}

// Bind attaches every registered tool to the SDK server. It fails if two
// tools claim the same name, which would otherwise silently shadow one
// handler in the SDK.
func (r *Registry) Bind(s *mcp.Server, deps Deps) error {
	seen := make(map[string]bool, len(r.tools))
	for _, t := range r.tools {
		if seen[t.Name()] {
			return fmt.Errorf("mcpserver: duplicate tool name %q", t.Name())
		}
		seen[t.Name()] = true
		t.RegisterSDK(s, deps)
	}
	return nil
}
