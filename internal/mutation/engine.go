// Package mutation contains the shared, post-fence provider mutation sequence.
package mutation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidConfiguration = errors.New("mutation: required hook is missing")

// Hooks bind the common mutation sequence to domain-owned state, provider, and
// readback validation. RenewLease is called before each potentially durable or
// external step; MarkSubmitting must durably fence submission before Submit.
type Hooks struct {
	RenewLease       func(context.Context) error
	MarkSubmitting   func(context.Context) error
	Submit           func(context.Context) (operationReference string, initialDelay time.Duration, err error)
	PersistOperation func(context.Context, string) error
	Poll             func(context.Context, string, time.Duration) error
	Readback         func(context.Context) (string, error)
	ValidateReadback func(string) error
}

// Failure carries the stable domain reason for an unknown mutation outcome.
type Failure struct {
	Reason             string
	OperationReference string
	Cause              error
}

// InvalidReadback marks a successful provider read whose value or cache
// provenance failed domain validation.
type InvalidReadback struct{ Cause error }

func (e *InvalidReadback) Error() string {
	if e == nil || e.Cause == nil {
		return "mutation: invalid readback"
	}
	return e.Cause.Error()
}
func (e *InvalidReadback) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (f *Failure) Error() string { return f.Reason }
func (f *Failure) Unwrap() error { return f.Cause }

type Result struct {
	OperationReference string
	Readback           string
}

// Execute runs one submission at most. It never retries a provider mutation.
func Execute(ctx context.Context, hooks Hooks) (Result, error) {
	var result Result
	for _, check := range []struct {
		name    string
		missing bool
	}{
		{"RenewLease", hooks.RenewLease == nil}, {"MarkSubmitting", hooks.MarkSubmitting == nil},
		{"Submit", hooks.Submit == nil}, {"PersistOperation", hooks.PersistOperation == nil},
		{"Poll", hooks.Poll == nil}, {"Readback", hooks.Readback == nil},
		{"ValidateReadback", hooks.ValidateReadback == nil},
	} {
		if check.missing {
			return result, fmt.Errorf("%w: %s", ErrInvalidConfiguration, check.name)
		}
	}
	fail := func(reason string, err error) (Result, error) {
		return result, &Failure{Reason: reason, OperationReference: result.OperationReference, Cause: err}
	}
	if err := ctx.Err(); err != nil {
		return fail("mutation_context_cancelled", err)
	}
	renew := func() error { return hooks.RenewLease(ctx) }
	if err := renew(); err != nil {
		return fail("lease_renewal_failed", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("mutation_context_cancelled", err)
	}
	if err := hooks.MarkSubmitting(ctx); err != nil {
		return fail("local_submitting_commit_failed", err)
	}
	if err := renew(); err != nil {
		return fail("lease_renewal_failed", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("mutation_context_cancelled", err)
	}
	op, delay, submitErr := hooks.Submit(ctx)
	result.OperationReference = op
	if op != "" {
		if err := hooks.PersistOperation(ctx, op); err != nil {
			return fail("operation_reference_persist_failed", err)
		}
	}
	if submitErr != nil {
		return fail("provider_outcome_unknown", submitErr)
	}
	if op == "" {
		if err := hooks.PersistOperation(ctx, op); err != nil {
			return fail("operation_reference_persist_failed", err)
		}
	}
	if err := renew(); err != nil {
		return fail("lease_renewal_failed", err)
	}
	if err := hooks.Poll(ctx, op, delay); err != nil {
		return fail("poll_outcome_unknown", err)
	}
	if err := renew(); err != nil {
		return fail("lease_renewal_failed", err)
	}
	value, err := hooks.Readback(ctx)
	if err != nil {
		var invalid *InvalidReadback
		if errors.As(err, &invalid) {
			return fail("readback_mismatch", err)
		}
		return fail("readback_unavailable", err)
	}
	result.Readback = value
	if err := hooks.ValidateReadback(value); err != nil {
		return fail("readback_mismatch", err)
	}
	return result, nil
}
