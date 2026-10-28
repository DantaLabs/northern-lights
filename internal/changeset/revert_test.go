package changeset

import (
	"context"
	"fmt"
	"testing"
)

func TestStageRevertRestoresBeforeValues(t *testing.T) {
	s, f, items := fixture(t, 4)
	p := stage(t, s, items)
	if out, err := s.Confirm(context.Background(), "analyst", p.ID, p.Token); err != nil || out.Status != "completed" {
		t.Fatalf("confirm: %+v %v", out, err)
	}
	if _, err := s.StageRevert(context.Background(), "someone-else", p.ID); err == nil {
		t.Fatal("another actor staged a revert")
	}
	r, err := s.StageRevert(context.Background(), "analyst", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 4 {
		t.Fatalf("items=%+v", r.Items)
	}
	for _, item := range r.Items {
		if item.Before == item.After || item.Source != "revert "+p.ID {
			t.Fatalf("item=%+v", item)
		}
	}
	if out, err := s.Confirm(context.Background(), "analyst", r.ID, r.Token); err != nil || out.Status != "completed" {
		t.Fatalf("revert confirm: %+v %v", out, err)
	}
	for i := range items {
		if got := f.values[fmt.Sprintf("spreadsheet/sheet-%d/%d/0", i%2, i)]; got != fmt.Sprint(i) {
			t.Fatalf("cell %d = %q after revert", i, got)
		}
	}
}
