package changeset

import (
	"context"
	"encoding/json"
	"errors"
)

// Reverter stages a change set that puts back the values a confirmed change
// set replaced. It is separate from Service so existing fakes stay unchanged.
type Reverter interface {
	StageRevert(ctx context.Context, actor, changeSetID string) (Preview, error)
}

var _ Reverter = (*service)(nil)

// ponytail: scans at most revertScanLimit recent audit entries; index audit by
// change set if the log grows past demo size.
const revertScanLimit = 5000

// StageRevert reads the caller's completed writes for changeSetID from the
// audit log and stages their before values. The revert goes through the same
// preview, approval and read-back as any other change set.
func (s *service) StageRevert(ctx context.Context, actor, changeSetID string) (Preview, error) {
	if s.initErr != nil {
		return Preview{}, s.initErr
	}
	if changeSetID == "" {
		return Preview{}, errors.New("change set ID is required")
	}
	var items []Item
	seen := map[string]bool{}
	var before int64
	for scanned := 0; scanned < revertScanLimit; {
		entries, err := s.env.Audit.RecentPage(ctx, 256, "", before)
		if err != nil {
			return Preview{}, err
		}
		for _, e := range entries {
			if e.Tool != "workiva_confirm_change_set" || e.Actor != actor {
				continue
			}
			var b, a struct {
				ChangeSetID string `json:"change_set_id"`
				Field       string `json:"field"`
				Value       string `json:"value"`
				Status      string `json:"status"`
			}
			if json.Unmarshal([]byte(e.BeforeJSON), &b) != nil || json.Unmarshal([]byte(e.AfterJSON), &a) != nil {
				continue
			}
			if b.ChangeSetID != changeSetID || a.Status != "completed" || seen[b.Field] {
				continue
			}
			seen[b.Field] = true
			items = append(items, Item{FieldName: b.Field, Value: b.Value, Source: "revert " + changeSetID})
		}
		scanned += len(entries)
		if len(entries) < 256 {
			break
		}
		before = entries[len(entries)-1].Seq
	}
	if len(items) == 0 {
		return Preview{}, errors.New("no completed writes by you were found for that change set")
	}
	return s.Stage(ctx, actor, items)
}
