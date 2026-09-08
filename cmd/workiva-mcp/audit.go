package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/dantalabs/northern-lights/internal/audit"
)

// runAudit handles the `workiva-mcp audit <verify|export>` subcommand
// family. out receives human readable results; JSONL exports are written
// to out as well.
func runAudit(args []string, out io.Writer) (err error) {
	if len(args) == 0 {
		return fmt.Errorf("audit: missing subcommand (verify|export)")
	}

	sub := args[0]
	fs := flag.NewFlagSet("audit "+sub, flag.ContinueOnError)
	fs.SetOutput(out)
	dbPath := fs.String("db", "./northern-lights.db", "path to the SQLite database")
	format := fs.String("format", "jsonl", "export format (only jsonl is supported)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	log, err := audit.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer func() {
		if closeErr := log.Close(); closeErr != nil {
			closeErr = fmt.Errorf("audit: close: %w", closeErr)
			if err == nil {
				err = closeErr
			} else {
				err = errors.Join(err, closeErr)
			}
		}
	}()

	switch sub {
	case "verify":
		if err := log.Verify(context.Background()); err != nil {
			return fmt.Errorf("audit chain broken: %w", err)
		}
		if _, err := fmt.Fprintln(out, "OK: audit chain intact"); err != nil {
			return fmt.Errorf("audit: write output: %w", err)
		}
		return nil
	case "export":
		if *format != "jsonl" {
			return fmt.Errorf("audit: unsupported export format %q (only jsonl)", *format)
		}
		return log.Export(out)
	default:
		return fmt.Errorf("audit: unknown subcommand %q (verify|export)", sub)
	}
}

// isAuditInvocation reports whether args request the audit subcommand
// family, e.g. os.Args[1:] == ["audit", "verify", ...].
func isAuditInvocation(args []string) bool {
	return len(args) > 0 && strings.EqualFold(args[0], "audit")
}
