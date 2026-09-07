// Package ratelimit provides per-category token-bucket rate limiting for
// Workiva API calls. Limits are workspace-wide, so every outbound request
// funnels through a single Limiter.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"
)

// Category identifies which Workiva rate limit bucket an API call
// consumes.
type Category int

const (
	// CategoryReads covers sheetdata and values reads (600 req/min).
	CategoryReads Category = iota
	// CategoryWrites covers SheetUpdate mutations (60 req/min).
	CategoryWrites
	// CategoryOperations covers polling of async operations (1 req/sec).
	CategoryOperations
)

func (c Category) String() string {
	switch c {
	case CategoryReads:
		return "reads"
	case CategoryWrites:
		return "writes"
	case CategoryOperations:
		return "operations"
	default:
		return fmt.Sprintf("unknown(%d)", int(c))
	}
}

// Limiter gates outbound Workiva requests by category. Each category has
// its own token bucket with burst 1, which serializes requests within a
// category and spaces them at the configured rate.
type Limiter struct {
	buckets [3]*rate.Limiter
}

// Preset rates matching the Workiva documented limits.
const (
	// readsPerMinute is the sheetdata read limit per workspace.
	readsPerMinute = 600
	// writesPerMinute is the SheetUpdate write limit per workspace.
	writesPerMinute = 60
	// operationsPerSecond is the async operation polling limit.
	operationsPerSecond = 1
)

// burst is deliberately 1 so calls within a category are spaced at the
// configured rate instead of arriving in clumps.
const burst = 1

// NewLimiter returns a Limiter with the production preset rates: reads
// 600/min, writes 60/min, operations 1/sec, each with burst 1.
func NewLimiter() *Limiter {
	return newLimiter(
		rate.Every(time.Minute/readsPerMinute),
		rate.Every(time.Minute/writesPerMinute),
		rate.Every(time.Second/operationsPerSecond),
	)
}

// newLimiter builds a Limiter from explicit per-interval rates. Tests use
// it to exercise throttling behavior with short intervals.
func newLimiter(reads, writes, operations rate.Limit) *Limiter {
	return &Limiter{
		buckets: [3]*rate.Limiter{
			rate.NewLimiter(reads, burst),
			rate.NewLimiter(writes, burst),
			rate.NewLimiter(operations, burst),
		},
	}
}

// Wait blocks until the bucket for category has a token, consuming it.
// It returns ctx.Err() if the context is cancelled while waiting.
func (l *Limiter) Wait(ctx context.Context, category Category) error {
	if category < CategoryReads || category > CategoryOperations {
		return fmt.Errorf("ratelimit: invalid category %d", int(category))
	}
	if err := l.buckets[category].Wait(ctx); err != nil {
		// rate.Limiter wraps the context error; return it unwrapped so
		// callers can use errors.Is directly.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}
