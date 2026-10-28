package tools

import "github.com/dantalabs/northern-lights/internal/mcpserver"

// Demo returns the demo pipeline tools, registered after All() only when NL_DEMO_PIPELINE=true.
func Demo() []mcpserver.Tool {
	return []mcpserver.Tool{
		PrepareSection(),
		ConfirmChangeSet(),
		StageSectionText(),
		ConfirmSectionText(),
		CompareFields(),
		RevertChangeSet(),
		ReviewStatus(),
	}
}

// requireDemoDep fails with a clear message when a demo service is missing.
func requireDemoDep(present bool, name string) error {
	if present {
		return nil
	}
	return failMsg(name+" is not available", "server misconfiguration: start the server with NL_DEMO_PIPELINE=true")
}
