// Command workiva-mcp is the northern-lights Workiva MCP server entrypoint.
// Wiring (config, store, server) lands in later tasks; for now it prints
// the version and exits.
package main

import (
	"flag"
	"fmt"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("workiva-mcp %s\n", version)
		return
	}

	fmt.Printf("workiva-mcp %s\n", version)
}
