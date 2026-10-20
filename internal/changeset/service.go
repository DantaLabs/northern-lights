package changeset

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dantalabs/northern-lights/internal/demoenv"
	"github.com/dantalabs/northern-lights/internal/identity"
	"github.com/dantalabs/northern-lights/internal/sqlitedb"
)

const confirmationTTL = 10 * time.Minute
const confirmPermission = string(identity.PermissionWorkivaWriteConfirm)
const schema = `CREATE TABLE IF NOT EXISTS change_set_pending (
 tenant_id TEXT NOT NULL, id TEXT NOT NULL, token_digest TEXT NOT NULL,
 actor TEXT NOT NULL, required_permission TEXT NOT NULL,
 expires_at INTEGER NOT NULL, consumed INTEGER NOT NULL DEFAULT 0,
 items_json TEXT NOT NULL, PRIMARY KEY (tenant_id, id)
);`

type service struct {
	env     demoenv.Env
	initErr error
}
type stagedItem struct {
	PreviewItem
	FieldID int64
}

// New uses the existing shared database. Initialization errors are reported by
// Stage and Confirm because the scaffold constructor has no error return.
func New(env demoenv.Env) Service {
	s := &service{env: env}
	if env.DB == nil || env.Store == nil || env.Client == nil || env.Audit == nil {
		s.initErr = errors.New("change set dependencies are unavailable")
	} else {
		s.initErr = sqlitedb.Migrate(context.Background(), env.DB, "change_set", []string{schema})
	}
	return s
}

func (s *service) Stage(ctx context.Context, actor string, items []Item) (Preview, error) {
	if s.initErr != nil {
		return Preview{}, s.initErr
	}
	if len(items) == 0 || len(items) > 500 {
		return Preview{}, errors.New("change set requires 1 to 500 items")
	}
	seen := make(map[string]bool, len(items))
	staged := make([]stagedItem, 0, len(items))
	var unknown []string
	for _, item := range items {
		if seen[item.FieldName] {
			return Preview{}, fmt.Errorf("duplicate field: %s", item.FieldName)
		}
		seen[item.FieldName] = true
		field, err := s.env.Store.GetField(ctx, item.FieldName)
		if err != nil {
			return Preview{}, err
		}
		if field == nil {
			unknown = append(unknown, item.FieldName)
			continue
		}
		staged = append(staged, stagedItem{FieldID: field.ID, PreviewItem: PreviewItem{
			FieldName: field.Name, SpreadsheetID: field.SpreadsheetID, SheetID: field.SheetID,
			Range: field.CellRange, After: item.Value, Source: item.Source,
		}})
	}
	if len(unknown) > 0 {
		return Preview{}, fmt.Errorf("unknown fields: %s", strings.Join(unknown, ", "))
	}
	if actor == "" || actor == "unknown" {
		return Preview{}, errors.New("change set requires an authenticated actor")
	}
	// Check all targets before the first provider read.
	for _, item := range staged {
		if !s.allowed(item) {
			return Preview{}, fmt.Errorf("resource is not allowed by policy: %s", item.FieldName)
		}
		if _, err := boundedRange(item.Range); err != nil {
			return Preview{}, err
		}
	}
	preview := Preview{ExpiresAt: time.Now().UTC().Add(confirmationTTL)}
	for i := range staged {
		value, _, err := s.read(ctx, staged[i])
		if err != nil {
			return Preview{}, err
		}
		staged[i].Before = value
		preview.Items = append(preview.Items, staged[i].PreviewItem)
	}
	var err error
	if preview.ID, err = randomToken(); err != nil {
		return Preview{}, err
	}
	if preview.Token, err = randomToken(); err != nil {
		return Preview{}, err
	}
	payload, err := json.Marshal(staged)
	if err != nil {
		return Preview{}, err
	}
	_, err = s.env.DB.ExecContext(ctx, `INSERT INTO change_set_pending
 (tenant_id, id, token_digest, actor, required_permission, expires_at, items_json)
 VALUES (?, ?, ?, ?, ?, ?, ?)`, identity.StorageTenant(ctx), preview.ID, digest(preview.Token), actor, confirmPermission, preview.ExpiresAt.UnixNano(), string(payload))
	if err != nil {
		return Preview{}, err
	}
	var lines []string
	for _, item := range preview.Items {
		lines = append(lines, fmt.Sprintf("%s: %s -> %s (awaiting_confirmation)", item.FieldName, item.Before, item.After))
	}
	preview.Summary = strings.Join(lines, "\n")
	return preview, nil
}

func (s *service) allowed(item stagedItem) bool {
	return s.env.Cfg == nil || s.env.Cfg.ResourceAllowed(item.SpreadsheetID, item.SheetID)
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *service) Confirm(ctx context.Context, actor, id, token string) (Result, error) {
	items, err := s.consume(ctx, actor, id, token)
	if err != nil {
		return Result{}, err
	}
	if err := s.validateMappings(ctx, items); err != nil {
		return Result{}, err
	}
	return s.execute(ctx, actor, id, items)
}
