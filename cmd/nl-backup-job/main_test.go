package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func validEnvironment() map[string]string {
	return map[string]string{
		envEndpoint:  "https://backup.example/maintenance/backup",
		envAudience:  "api://11111111-2222-4333-8444-555555555555/.default",
		envClientID:  "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		envPublicKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

func TestParseJobConfigRequiresExactTrustedValues(t *testing.T) {
	if _, err := parseJobConfig(validEnvironment()); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"missing endpoint": func(v map[string]string) { delete(v, envEndpoint) },
		"empty scope":      func(v map[string]string) { v[envAudience] = "" },
		"client ID trim":   func(v map[string]string) { v[envClientID] = " aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee " },
		"bad endpoint":     func(v map[string]string) { v[envEndpoint] += "?x=1" },
		"bad key":          func(v map[string]string) { v[envPublicKey] = strings.ToUpper(v[envPublicKey]) },
	} {
		t.Run(name, func(t *testing.T) {
			values := validEnvironment()
			mutate(values)
			if _, err := parseJobConfig(values); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestRunRejectsBadConfigurationBeforeCredentialConstruction(t *testing.T) {
	values := validEnvironment()
	values[envEndpoint] = "http://backup.example/maintenance/backup"
	called := false
	var out bytes.Buffer
	err := run(context.Background(), values, func(string) (azcore.TokenCredential, error) {
		called = true
		return nil, nil
	}, &out)
	if err == nil || called || out.Len() != 0 {
		t.Fatalf("err=%v credential_called=%v output=%q", err, called, out.String())
	}
}

func TestRunSanitizesCredentialConstructionErrors(t *testing.T) {
	values := validEnvironment()
	var out bytes.Buffer
	err := run(context.Background(), values, func(string) (azcore.TokenCredential, error) {
		return nil, errors.New("SDK secret material")
	}, &out)
	if err == nil || strings.Contains(err.Error(), "SDK secret") || out.Len() != 0 {
		t.Fatalf("credential construction error was not sanitized: %v output=%q", err, out.String())
	}
}

func TestRunRejectsNilDependencies(t *testing.T) {
	var nilContext context.Context
	if err := run(nilContext, validEnvironment(), nil, &bytes.Buffer{}); err == nil {
		t.Fatal("nil context/factory accepted")
	}
}
