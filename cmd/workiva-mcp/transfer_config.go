package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/transfer"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

const transferProductionEnv = "NL_TRANSFER_PRODUCTION"

func transferProductionOptIn(value string, present bool) (bool, error) {
	if !present {
		return false, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("NL_TRANSFER_PRODUCTION must be exactly true or false")
	}
}

func transferProductionEnabled() (bool, error) {
	value, present := os.LookupEnv(transferProductionEnv)
	return transferProductionOptIn(value, present)
}

type blobNamespace struct {
	accountURL, container, environment string
}

func samePhysicalBlobContainer(left, right blobNamespace) bool {
	return left.accountURL != "" && right.accountURL != "" && left.container != "" && right.container != "" && left.accountURL == right.accountURL && left.container == right.container
}

func transferNamespace(restore, backup, evidence blobNamespace) error {
	if restore.accountURL == "" || restore.container == "" || restore.environment == "" {
		return errors.New("transfer production requires startup restore Blob account, container, and environment")
	}
	if backup.accountURL == "" || backup.container == "" || backup.environment == "" {
		return errors.New("transfer production requires configured backup maintenance Blob namespace")
	}
	if evidence.accountURL == "" || evidence.container == "" || evidence.environment == "" {
		return errors.New("transfer production requires configured evidence delivery Blob namespace")
	}
	if restore.accountURL != backup.accountURL || restore.container != backup.container || restore.environment != backup.environment {
		return errors.New("transfer restore and backup Blob namespaces must match exactly")
	}
	if samePhysicalBlobContainer(evidence, backup) || samePhysicalBlobContainer(evidence, restore) {
		return errors.New("transfer evidence Blob container must be physically separate from backup and restore storage")
	}
	if evidence.environment != restore.environment {
		return fmt.Errorf("transfer evidence environment %q does not match restore environment", strings.TrimSpace(evidence.environment))
	}
	return nil
}

type productionTransferConfig struct {
	enabled                                              bool
	restoreNamespace, backupNamespace, evidenceNamespace blobNamespace
	authMode                                             config.AuthMode
	demoMode, assuranceEnabled                           bool
	startupFence                                         *transfer.AzureBlobFence
	backupConfigured, evidenceConfigured                 bool
	assuranceStore                                       *assurance.Store
	audit                                                *audit.Log
	db                                                   *sql.DB
	provider                                             *workivaprovider.Router
	gate                                                 *assurance.DrainGate
	quarantineInterval                                   time.Duration
}

// configureProductionTransfer assembles the opt-in service only after all
// startup admissions are already established. The returned stop function
// cancels and joins the periodic quarantine worker before its DB owner closes.
func configureProductionTransfer(ctx context.Context, cfg productionTransferConfig) (*transfer.Service, func(), error) {
	if !cfg.enabled {
		return nil, nil, nil
	}
	if ctx == nil || ctx.Err() != nil {
		return nil, nil, errors.New("transfer production startup context is unavailable")
	}
	if err := transferNamespace(cfg.restoreNamespace, cfg.backupNamespace, cfg.evidenceNamespace); err != nil {
		return nil, nil, err
	}
	if cfg.authMode != config.AuthModeEntra || cfg.demoMode || !cfg.assuranceEnabled || cfg.startupFence == nil || !cfg.startupFence.Durable() || !cfg.backupConfigured || !cfg.evidenceConfigured {
		return nil, nil, errors.New("transfer production requires non-demo Entra assurance, admitted durable restore fence, backup maintenance, and evidence delivery")
	}
	if cfg.db == nil || cfg.assuranceStore == nil || cfg.assuranceStore.DB() != cfg.db || cfg.audit == nil || !cfg.audit.SharesDB(cfg.db) || cfg.provider == nil || cfg.gate == nil {
		return nil, nil, errors.New("transfer production requires shared database, audit, provider router, and maintenance gate")
	}
	if err := cfg.assuranceStore.Ready(); err != nil {
		return nil, nil, errors.New("transfer production requires ready assurance")
	}
	store, err := transfer.NewWithDB(cfg.db)
	if err != nil {
		return nil, nil, fmt.Errorf("initialize transfer store and startup quarantine: %w", err)
	}
	service, err := transfer.NewService(store, cfg.assuranceStore, cfg.provider, cfg.startupFence)
	if err != nil {
		return nil, nil, fmt.Errorf("initialize production transfer service: %w", err)
	}
	interval := cfg.quarantineInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	janitorCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-janitorCtx.Done():
				return
			case now := <-ticker.C:
				release, gateErr := cfg.gate.EnterWrite(janitorCtx)
				if gateErr != nil {
					continue
				}
				if quarantineErr := store.QuarantineExpired(janitorCtx, now.UTC()); quarantineErr != nil {
					log.Printf("transfer quarantine failed: %v", quarantineErr)
				}
				release()
			}
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	return service, stop, nil
}
