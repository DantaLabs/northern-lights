package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestLimiterThrottlesBurst(t *testing.T) {
	// Writes bucket refills one token every 50ms with burst 1, so the
	// third rapid call must block until a refill arrives.
	l := newLimiter(rate.Every(time.Second/20), rate.Every(time.Second/20), rate.Every(time.Second/20))

	ctx := context.Background()
	start := time.Now()

	if err := l.Wait(ctx, CategoryWrites); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := l.Wait(ctx, CategoryWrites); err != nil {
		t.Fatalf("second write: %v", err)
	}
	// The third call consumes the first refill slot, so it should block
	// for roughly one interval rather than returning instantly.
	if err := l.Wait(ctx, CategoryWrites); err != nil {
		t.Fatalf("third write: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 30*time.Millisecond {
		t.Errorf("third write returned after %s, expected it to block at least 30ms", elapsed)
	}
}

func TestLimiterWaitContextCancellation(t *testing.T) {
	l := newLimiter(rate.Every(time.Hour), rate.Every(time.Hour), rate.Every(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())

	// Exhaust the single burst token.
	if err := l.Wait(ctx, CategoryReads); err != nil {
		t.Fatalf("first read: %v", err)
	}

	// The next call must wait an hour; cancel the context so Wait
	// unblocks and reports the context error.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := l.Wait(ctx, CategoryReads)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestLimiterCategoriesAreIndependent(t *testing.T) {
	// Reads bucket is fast, writes bucket is nearly stopped. Reading must
	// not be throttled by an exhausted writes bucket.
	l := newLimiter(rate.Every(time.Millisecond), rate.Every(time.Hour), rate.Every(time.Hour))

	ctx := context.Background()
	if err := l.Wait(ctx, CategoryWrites); err != nil {
		t.Fatalf("first write: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, CategoryReads); err != nil {
		t.Errorf("read blocked by exhausted writes bucket: %v", err)
	}
}

func TestPresetLimits(t *testing.T) {
	// Sanity check the production presets exist and accept a token.
	l := NewLimiter()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, c := range []Category{CategoryReads, CategoryWrites, CategoryOperations} {
		if err := l.Wait(ctx, c); err != nil {
			t.Errorf("preset Wait(%v): %v", c, err)
		}
	}
}
