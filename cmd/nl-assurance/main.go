// Command nl-assurance provisions signed Phase 3 server-owned bundles outside MCP.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "nl-assurance:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) < 2 || args[0] != "bundle" {
		return errors.New("usage: nl-assurance bundle validate|stage|apply [flags]")
	}
	switch args[1] {
	case "validate":
		validated, err := parseAndValidate(args[2:])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "valid bundle_id=%s bundle_version=%d tenant_id=%s content_hash=%s\n",
			validated.Bundle.BundleID, validated.Bundle.BundleVersion, validated.Bundle.TenantID, validated.ContentHash)
		return err
	case "stage":
		fs := flag.NewFlagSet("bundle stage", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dbPath := fs.String("db", "", "SQLite database path")
		bundlePath := fs.String("bundle", "", "bundle YAML/JSON path")
		signaturePath := fs.String("signature", "", "detached Ed25519 signature path")
		publicKeyText := fs.String("public-key", "", "hex Ed25519 public key")
		tenant := fs.String("tenant", "", "configured tenant ID")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		validated, err := loadValidated(*bundlePath, *signaturePath, *publicKeyText, *tenant)
		if err != nil {
			return err
		}
		store, db, err := openProvisioningStore(*dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		ctx := tenantContext(*tenant)
		if err := store.StageBundle(ctx, validated); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "staged bundle_id=%s bundle_version=%d content_hash=%s activation_requested=false\n",
			validated.Bundle.BundleID, validated.Bundle.BundleVersion, validated.ContentHash)
		return err
	case "apply":
		fs := flag.NewFlagSet("bundle apply", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dbPath := fs.String("db", "", "SQLite database path")
		tenant := fs.String("tenant", "", "configured tenant ID")
		bundleID := fs.String("bundle-id", "", "staged bundle ID")
		version := fs.Int("bundle-version", 0, "staged bundle version")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *bundleID == "" || *version <= 0 || *tenant == "" {
			return errors.New("apply requires -tenant, -bundle-id, and positive -bundle-version")
		}
		store, db, err := openProvisioningStore(*dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		if err := store.RequestActivation(tenantContext(*tenant), *bundleID, *version); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "activation_requested bundle_id=%s bundle_version=%s\n", *bundleID, strconv.Itoa(*version))
		return err
	default:
		return fmt.Errorf("unknown bundle command %q", args[1])
	}
}

func parseAndValidate(args []string) (*assurance.ValidatedBundle, error) {
	fs := flag.NewFlagSet("bundle validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bundlePath := fs.String("bundle", "", "bundle YAML/JSON path")
	signaturePath := fs.String("signature", "", "detached Ed25519 signature path")
	publicKeyText := fs.String("public-key", "", "hex Ed25519 public key")
	tenant := fs.String("tenant", "", "configured tenant ID")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return loadValidated(*bundlePath, *signaturePath, *publicKeyText, *tenant)
}

func loadValidated(bundlePath, signaturePath, publicKeyText, tenant string) (*assurance.ValidatedBundle, error) {
	if bundlePath == "" || signaturePath == "" || publicKeyText == "" || tenant == "" {
		return nil, errors.New("validate/stage requires -bundle, -signature, -public-key, and -tenant")
	}
	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("read bundle: %w", err)
	}
	signatureRaw, err := os.ReadFile(signaturePath)
	if err != nil {
		return nil, fmt.Errorf("read signature: %w", err)
	}
	signature, err := decodeSignature(signatureRaw)
	if err != nil {
		return nil, err
	}
	publicKey, err := assurance.ParsePublicKey(publicKeyText)
	if err != nil {
		return nil, err
	}
	return assurance.ValidateBundle(raw, signature, tenant, publicKey)
}

func decodeSignature(raw []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	if decoded, err := hex.DecodeString(trimmed); err == nil && len(decoded) == 64 {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil && len(decoded) == 64 {
		return decoded, nil
	}
	if len(raw) == 64 {
		return append([]byte(nil), raw...), nil
	}
	return nil, errors.New("signature must be 64 raw bytes, hex, or standard base64")
}

func openProvisioningStore(path string) (*assurance.Store, interface{ Close() error }, error) {
	if path == "" {
		return nil, nil, errors.New("database path is required")
	}
	db, err := sqlitedb.Open(path)
	if err != nil {
		return nil, nil, err
	}
	closeError := func(cause error) (*assurance.Store, interface{ Close() error }, error) {
		if err := db.Close(); err != nil {
			cause = errors.Join(cause, err)
		}
		return nil, nil, cause
	}
	if _, err := mapping.NewWithDB(db); err != nil {
		return closeError(err)
	}
	if _, err := audit.NewWithDB(db); err != nil {
		return closeError(err)
	}
	store, err := assurance.NewWithDB(db)
	if err != nil {
		return closeError(err)
	}
	return store, db, nil
}

func tenantContext(tenant string) context.Context {
	if tenant == identity.LegacyTenantID {
		return context.Background()
	}
	return identity.ContextWithPrincipal(context.Background(), identity.Principal{TenantID: tenant, ObjectID: "nl-assurance-cli"})
}
