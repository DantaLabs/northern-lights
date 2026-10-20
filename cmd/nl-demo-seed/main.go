// Command nl-demo-seed loads the synthetic ESG data into the Workiva sandbox for the KPMG demo.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("nl-demo-seed", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mappings := fs.String("mappings", "configs/demo/mappings-sandbox.yaml", "mapping file naming the spreadsheets and report document to write")
	data := fs.String("data", "configs/demo/data", "directory with esg-2025.csv and esg-2024.csv")
	sections := fs.String("sections", "configs/demo/sections", "directory with the section recipes")
	dryRun := fs.Bool("dry-run", false, "print the plan and send nothing (no network, no credentials)")
	reset := fs.Bool("reset", false, "also clear the target fields and delete the recipe sections from the report document")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: nl-demo-seed [flags]")
		_, _ = fmt.Fprintln(stderr, "Writes only to the spreadsheets and report document named in the mapping file and the section recipes.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	m, err := LoadMappings(*mappings)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	recipes, err := LoadRecipes(*sections)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	cur, err := LoadCSV(filepath.Join(*data, "esg-2025.csv"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	pri, err := LoadCSV(filepath.Join(*data, "esg-2024.csv"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	plan, err := BuildPlan(m, recipes, cur, pri, *reset)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if err := plan.Print(stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if *dryRun {
		return 0
	}
	wk, err := newWorkiva(ctx)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := Apply(ctx, plan, wk, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}
