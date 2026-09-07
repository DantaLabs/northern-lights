package audit

import (
	"context"
	"testing"
)

func appendEntry(t *testing.T, l *Log, target string) Entry {
	t.Helper()
	e, err := l.Append(context.Background(), Entry{
		Actor:      "copilot",
		Tool:       "workiva_update_field",
		Action:     "write",
		Target:     target,
		BeforeJSON: `{"value":"1"}`,
		AfterJSON:  `{"value":"2"}`,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return e
}

func TestRecentReturnsNewestFirst(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	first := appendEntry(t, l, "ss-1/sh-1/B3")
	second := appendEntry(t, l, "ss-1/sh-1/B4")
	third := appendEntry(t, l, "ss-1/sh-1/B5")

	got, err := l.Recent(ctx, 2, "")
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Recent returned %d entries, want 2", len(got))
	}
	if got[0].Seq != third.Seq || got[1].Seq != second.Seq {
		t.Errorf("Recent order = [%d %d], want newest first [%d %d]", got[1].Seq, got[0].Seq, third.Seq, second.Seq)
	}
	if got[0].Target != "ss-1/sh-1/B5" || got[0].BeforeJSON != `{"value":"1"}` {
		t.Errorf("Recent[0] = %+v", got[0])
	}
	_ = first
}

func TestRecentFiltersByTarget(t *testing.T) {
	ctx := context.Background()
	l := openTestLog(t)

	appendEntry(t, l, "ss-1/sh-1/B3")
	hit := appendEntry(t, l, "ss-1/sh-1/B4")
	appendEntry(t, l, "ss-1/sh-1/B5")

	got, err := l.Recent(ctx, 10, "ss-1/sh-1/B4")
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Recent returned %d entries, want 1", len(got))
	}
	if got[0].Seq != hit.Seq {
		t.Errorf("Recent[0].Seq = %d, want %d", got[0].Seq, hit.Seq)
	}
}

func TestRecentOnEmptyLog(t *testing.T) {
	l := openTestLog(t)
	got, err := l.Recent(context.Background(), 20, "")
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Recent on empty log = %d entries, want 0", len(got))
	}
}
