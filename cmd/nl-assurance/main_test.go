package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dantalabs/northern-lights/internal/assurance"
)

func TestBundleCLIValidateStageAndApply(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := assurance.Bundle{
		SchemaVersion: 1, BundleID: "cli-bundle", BundleVersion: 1, TenantID: "legacy-api-key",
		Reports: []assurance.ReportRevision{{
			ReportID: "report", Revision: 1, Name: "Report", Owner: "owner", Status: "active", RetentionClass: "standard",
			ResourcePolicyHash: strings.Repeat("a", 64),
			Periods:            []assurance.Period{{Key: "2026", Label: "2026", Start: "2026-01-01", End: "2026-12-31"}},
			Fields:             []assurance.FieldDefinition{{FieldID: "f", ResourceID: "r", ExternalResourceID: "sp", SubresourceID: "sh", Locator: "A1", Kind: assurance.ValueText, Required: true, Order: 1}},
		}},
	}
	raw, _ := json.Marshal(bundle)
	canonical, _ := assurance.CanonicalJSONBytes(raw)
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "bundle.json")
	signaturePath := filepath.Join(dir, "bundle.sig")
	if err := os.WriteFile(bundlePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, []byte(hex.EncodeToString(ed25519.Sign(private, canonical))), 0o600); err != nil {
		t.Fatal(err)
	}
	common := []string{"-bundle", bundlePath, "-signature", signaturePath, "-public-key", hex.EncodeToString(public), "-tenant", "legacy-api-key"}
	var out bytes.Buffer
	if err := run(append([]string{"bundle", "validate"}, common...), &out); err != nil || !strings.Contains(out.String(), "valid bundle_id=cli-bundle") {
		t.Fatalf("validate output=%q err=%v", out.String(), err)
	}
	dbPath := filepath.Join(dir, "assurance.db")
	out.Reset()
	if err := run(append([]string{"bundle", "stage", "-db", dbPath}, common...), &out); err != nil || !strings.Contains(out.String(), "activation_requested=false") {
		t.Fatalf("stage output=%q err=%v", out.String(), err)
	}
	out.Reset()
	if err := run([]string{"bundle", "apply", "-db", dbPath, "-tenant", "legacy-api-key", "-bundle-id", "cli-bundle", "-bundle-version", "1"}, &out); err != nil || !strings.Contains(out.String(), "activation_requested") {
		t.Fatalf("apply output=%q err=%v", out.String(), err)
	}
}
