// Command nl-backup-job invokes the application's sealed-backup endpoint once.
// It is intended to run as a dedicated, explicitly assigned managed identity.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/dantalabs/northern-lights/internal/backupjob"
	"github.com/google/uuid"
)

const (
	envEndpoint  = "NL_BACKUP_ENDPOINT"
	envAudience  = "NL_BACKUP_SCOPE"
	envClientID  = "NL_BACKUP_MI_CLIENT_ID"
	envPublicKey = "NL_BACKUP_CHECKPOINT_PUBLIC_KEY_HEX"
)

type jobConfig struct {
	backup   backupjob.Config
	clientID string
}

func parseJobConfig(values map[string]string) (jobConfig, error) {
	endpoint, okEndpoint := values[envEndpoint]
	audience, okAudience := values[envAudience]
	clientID, okClientID := values[envClientID]
	publicKey, okKey := values[envPublicKey]
	if !okEndpoint || !okAudience || !okClientID || !okKey || endpoint == "" || audience == "" || clientID == "" || publicKey == "" {
		return jobConfig{}, errors.New("required backup job configuration is missing")
	}
	id, err := uuid.Parse(clientID)
	if err != nil || id.String() != clientID {
		return jobConfig{}, errors.New("managed identity client ID must be a canonical UUID")
	}
	cfg := backupjob.Config{Endpoint: endpoint, Audience: audience, CheckpointPublicKeyHex: publicKey}
	if err := backupjob.ValidateConfig(cfg); err != nil {
		return jobConfig{}, err
	}
	return jobConfig{backup: cfg, clientID: clientID}, nil
}

type credentialFactory func(string) (azcore.TokenCredential, error)

func run(ctx context.Context, values map[string]string, makeCredential credentialFactory, out io.Writer) error {
	if ctx == nil || makeCredential == nil || out == nil {
		return errors.New("backup job dependencies are unavailable")
	}
	cfg, err := parseJobConfig(values)
	if err != nil {
		return err
	}
	credential, err := makeCredential(cfg.clientID)
	if err != nil || credential == nil {
		return errors.New("explicit managed identity credential could not be created")
	}
	client, err := backupjob.NewClient(cfg.backup, credential, nil)
	if err != nil {
		return errors.New("backup caller configuration is invalid")
	}
	event, err := client.RunOnce(ctx)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(event); err != nil {
		return errors.New("sealed receipt event could not be written")
	}
	return nil
}

func environment() map[string]string {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		for i := 0; i < len(item); i++ {
			if item[i] == '=' {
				values[item[:i]] = item[i+1:]
				break
			}
		}
	}
	return values
}

func main() {
	makeCredential := func(clientID string) (azcore.TokenCredential, error) {
		return azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(clientID)})
	}
	if err := run(context.Background(), environment(), makeCredential, os.Stdout); err != nil {
		// Do not print SDK errors, response bodies, tokens, endpoint values, or keys.
		_, _ = fmt.Fprintln(os.Stderr, "nl.backup.failed: "+safeFailure(err))
		os.Exit(1)
	}
}

func safeFailure(err error) string {
	// Internal errors are deliberately collapsed at the process boundary.
	_ = err
	return "sealed backup call failed; inspect secured operational telemetry"
}
