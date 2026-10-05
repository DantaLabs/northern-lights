package tools

import "github.com/dantalabs/northern-lights/internal/mcpserver"

// All returns the eleven Wave 2 tools plus the registered Wave 3 relationship tool in stable registration order.
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
		ValidateReport(),
		ComparePeriods(),
		ExportEvidence(),
		DiscoverRelationships(),
	}
}
