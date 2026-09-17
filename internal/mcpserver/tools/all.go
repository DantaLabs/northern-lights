package tools

import "github.com/dantalabs/northern-lights/internal/mcpserver"

// All returns the seven builtin tools in registration order. The binary
// and the schema tests register exactly this list.
func All() []mcpserver.Tool {
	return []mcpserver.Tool{
		ListSpreadsheets(),
		ReadRange(),
		SearchFields(),
		GetField(),
		UpdateField(),
		SyncMapping(),
		AuditTrail(),
	}
}
