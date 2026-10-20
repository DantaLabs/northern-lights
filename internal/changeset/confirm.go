package changeset

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dantalabs/northern-lights/internal/identity"
)

// Distinct errors let callers decide whether to authenticate, restage or
// reconcile without accidentally submitting a spent approval again.
var (
	ErrUnknownToken      = errors.New("unknown change set or confirmation token")
	ErrReusedToken       = errors.New("confirmation token already used")
	ErrExpiredToken      = errors.New("confirmation token expired")
	ErrForeignActor      = errors.New("change set belongs to another actor")
	ErrMissingPermission = errors.New("workiva.write.confirm permission is required")
	ErrMappingChanged    = errors.New("field mapping changed since staging; token consumed, restage the change set")
)

// consume retains a spent-token tombstone so reuse is distinguishable from an
// unknown token. The conditional update is the single-use fence even when
// confirmations arrive concurrently through separate service instances.
func (s *service) consume(ctx context.Context, actor, id, token string) ([]stagedItem, error) {
	if s.initErr != nil {
		return nil, s.initErr
	}
	if principal, ok := identity.PrincipalFromContext(ctx); ok && !principal.HasPermission(identity.PermissionWorkivaWriteConfirm) {
		return nil, ErrMissingPermission
	}
	tenant := identity.StorageTenant(ctx)
	var storedDigest, owner, permission, payload string
	var expiry int64
	var consumed int
	err := s.env.DB.QueryRowContext(ctx, `SELECT token_digest,actor,required_permission,expires_at,consumed,items_json
 FROM change_set_pending WHERE tenant_id=? AND id=?`, tenant, id).Scan(&storedDigest, &owner, &permission, &expiry, &consumed, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownToken
	}
	if err != nil {
		return nil, err
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(storedDigest), []byte(digest(token))) != 1 {
		return nil, ErrUnknownToken
	}
	if actor == "" || actor == "unknown" || owner != actor {
		return nil, ErrForeignActor
	}
	if permission != confirmPermission {
		return nil, ErrMissingPermission
	}
	if consumed != 0 {
		return nil, ErrReusedToken
	}
	if time.Now().UTC().UnixNano() >= expiry {
		return nil, ErrExpiredToken
	}
	var items []stagedItem
	if err := json.Unmarshal([]byte(payload), &items); err != nil {
		return nil, err
	}
	result, err := s.env.DB.ExecContext(ctx, `UPDATE change_set_pending SET consumed=1
 WHERE tenant_id=? AND id=? AND token_digest=? AND actor=? AND required_permission=? AND consumed=0 AND expires_at>?`, tenant, id, storedDigest, actor, confirmPermission, time.Now().UTC().UnixNano())
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		// Expiry may have elapsed during validation; otherwise another confirmer
		// won the single-use fence.
		if time.Now().UTC().UnixNano() >= expiry {
			return nil, ErrExpiredToken
		}
		return nil, ErrReusedToken
	}
	return items, nil
}

func (s *service) validateMappings(ctx context.Context, items []stagedItem) error {
	for _, item := range items {
		field, err := s.env.Store.GetField(ctx, item.FieldName)
		if err != nil {
			return err
		}
		if field == nil || field.ID != item.FieldID || field.Name != item.FieldName || field.SpreadsheetID != item.SpreadsheetID || field.SheetID != item.SheetID || field.CellRange != item.Range {
			return fmt.Errorf("%w: %s", ErrMappingChanged, item.FieldName)
		}
		if !s.allowed(item) {
			return fmt.Errorf("resource is no longer allowed: %s; token consumed, restage", item.FieldName)
		}
	}
	return nil
}
