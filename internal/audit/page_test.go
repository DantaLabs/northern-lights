package audit

import (
	"context"
	"errors"
	"testing"
)

func TestRecentPageSequenceTargetAndCancellation(t *testing.T) {
	l, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ctx := context.Background()
	for _, target := range []string{"a", "b", "a", "a"} {
		if _, err := l.Append(ctx, Entry{Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := l.RecentPage(ctx, 2, "a", 0)
	if err != nil || len(page) != 2 || page[0].Seq != 4 || page[1].Seq != 3 {
		t.Fatalf("page=%v err=%v", page, err)
	}
	page, err = l.RecentPage(ctx, 2, "a", page[1].Seq)
	if err != nil || len(page) != 1 || page[0].Seq != 1 {
		t.Fatalf("page=%v err=%v", page, err)
	}
	page, err = l.RecentPage(ctx, 2, "a", 1)
	if err != nil || len(page) != 0 {
		t.Fatalf("page=%v err=%v", page, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.RecentPage(canceled, 2, "", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
