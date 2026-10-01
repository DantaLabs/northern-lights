package tools

import "github.com/dantalabs/northern-lights/internal/mcpserver"

// All returns the eight currently implemented builtin tools in registration
// order. Later Phase 3 waves add the remaining five tools to reach 13.
func All() []mcpserver.Tool {
	return []mcpserver.Tool{
		ListSpreadsheets(),
		ReadRange(),
		SearchFields(),
		GetField(),
		UpdateField(),
		SyncMapping(),
		AuditTrail(),
		SnapshotReport(),
	}
}
