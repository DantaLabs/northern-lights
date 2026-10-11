package mutation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExecuteFaultWindowsNeverResubmit(t *testing.T) {
	boom := errors.New("injected fault")
	for _, tc := range []struct {
		name, reason, failAt string
		wantSubmit           int
		wantPersist          int
	}{
		{name: "lease before ownership", reason: "lease_renewal_failed", failAt: "lease", wantSubmit: 0},
		{name: "owned state", reason: "local_submitting_commit_failed", failAt: "mark", wantSubmit: 0},
		{name: "submit with known operation", reason: "provider_outcome_unknown", failAt: "submit", wantSubmit: 1, wantPersist: 1},
		{name: "operation persistence", reason: "operation_reference_persist_failed", failAt: "persist", wantSubmit: 1, wantPersist: 1},
		{name: "poll", reason: "poll_outcome_unknown", failAt: "poll", wantSubmit: 1, wantPersist: 1},
		{name: "readback unavailable", reason: "readback_unavailable", failAt: "readback", wantSubmit: 1, wantPersist: 1},
		{name: "readback mismatch", reason: "readback_mismatch", failAt: "validate", wantSubmit: 1, wantPersist: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			submits, persists, leases := 0, 0, 0
			hooks := Hooks{
				RenewLease: func(context.Context) error {
					leases++
					if tc.failAt == "lease" && leases == 1 {
						return boom
					}
					return nil
				},
				MarkSubmitting: func(context.Context) error {
					if tc.failAt == "mark" {
						return boom
					}
					return nil
				},
				Submit: func(context.Context) (string, time.Duration, error) {
					submits++
					if tc.failAt == "submit" {
						return "operation-known", 0, boom
					}
					return "operation-known", 0, nil
				},
				PersistOperation: func(_ context.Context, operation string) error {
					persists++
					if operation != "operation-known" {
						t.Fatalf("operation=%q", operation)
					}
					if tc.failAt == "persist" {
						return boom
					}
					return nil
				},
				Poll: func(context.Context, string, time.Duration) error {
					if tc.failAt == "poll" {
						return boom
					}
					return nil
				},
				Readback: func(context.Context) (string, error) {
					if tc.failAt == "readback" {
						return "", boom
					}
					return "value", nil
				},
				ValidateReadback: func(string) error {
					if tc.failAt == "validate" {
						return boom
					}
					return nil
				},
			}
			result, err := Execute(context.Background(), hooks)
			var failure *Failure
			if !errors.As(err, &failure) || failure.Reason != tc.reason {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if submits != tc.wantSubmit || persists != tc.wantPersist {
				t.Fatalf("submits=%d persists=%d, want %d/%d", submits, persists, tc.wantSubmit, tc.wantPersist)
			}
			if submits > 1 {
				t.Fatalf("provider submitted %d times", submits)
			}
			if tc.failAt == "submit" && failure.OperationReference != "operation-known" {
				t.Fatalf("known operation lost: %+v", failure)
			}
		})
	}
}

func TestExecuteRejectsEveryMissingHookBeforeMutation(t *testing.T) {
	complete := Hooks{
		RenewLease:       func(context.Context) error { return nil },
		MarkSubmitting:   func(context.Context) error { return nil },
		Submit:           func(context.Context) (string, time.Duration, error) { return "", 0, nil },
		PersistOperation: func(context.Context, string) error { return nil },
		Poll:             func(context.Context, string, time.Duration) error { return nil },
		Readback:         func(context.Context) (string, error) { return "", nil },
		ValidateReadback: func(string) error { return nil },
	}
	for _, name := range []string{"RenewLease", "MarkSubmitting", "Submit", "PersistOperation", "Poll", "Readback", "ValidateReadback"} {
		t.Run(name, func(t *testing.T) {
			hooks := complete
			calls := 0
			hooks.RenewLease = func(context.Context) error { calls++; return nil }
			hooks.MarkSubmitting = func(context.Context) error { calls++; return nil }
			hooks.Submit = func(context.Context) (string, time.Duration, error) { calls++; return "", 0, nil }
			hooks.PersistOperation = func(context.Context, string) error { calls++; return nil }
			hooks.Poll = func(context.Context, string, time.Duration) error { calls++; return nil }
			hooks.Readback = func(context.Context) (string, error) { calls++; return "", nil }
			hooks.ValidateReadback = func(string) error { calls++; return nil }
			switch name {
			case "RenewLease":
				hooks.RenewLease = nil
			case "MarkSubmitting":
				hooks.MarkSubmitting = nil
			case "Submit":
				hooks.Submit = nil
			case "PersistOperation":
				hooks.PersistOperation = nil
			case "Poll":
				hooks.Poll = nil
			case "Readback":
				hooks.Readback = nil
			case "ValidateReadback":
				hooks.ValidateReadback = nil
			}
			result, err := Execute(context.Background(), hooks)
			if !errors.Is(err, ErrInvalidConfiguration) || result != (Result{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if calls != 0 {
				t.Fatalf("called %d hooks before validation", calls)
			}
		})
	}
}

func TestExecuteCancellationStopsBeforeSubmission(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		marked, submitted := 0, 0
		result, err := Execute(ctx, Hooks{
			RenewLease:       func(context.Context) error { return nil },
			MarkSubmitting:   func(context.Context) error { marked++; return nil },
			Submit:           func(context.Context) (string, time.Duration, error) { submitted++; return "", 0, nil },
			PersistOperation: func(context.Context, string) error { return nil },
			Poll:             func(context.Context, string, time.Duration) error { return nil },
			Readback:         func(context.Context) (string, error) { return "", nil },
			ValidateReadback: func(string) error { return nil },
		})
		assertCancelledBeforeSubmit(t, result, err, marked, submitted, 0)
	})

	t.Run("cancelled after durable submitting transition", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		marked, submitted := 0, 0
		result, err := Execute(ctx, Hooks{
			RenewLease:       func(context.Context) error { return nil },
			MarkSubmitting:   func(context.Context) error { marked++; cancel(); return nil },
			Submit:           func(context.Context) (string, time.Duration, error) { submitted++; return "", 0, nil },
			PersistOperation: func(context.Context, string) error { return nil },
			Poll:             func(context.Context, string, time.Duration) error { return nil },
			Readback:         func(context.Context) (string, error) { return "", nil },
			ValidateReadback: func(string) error { return nil },
		})
		assertCancelledBeforeSubmit(t, result, err, marked, submitted, 1)
	})
}

func assertCancelledBeforeSubmit(t *testing.T, result Result, err error, marked, submitted, wantMarked int) {
	t.Helper()
	var failure *Failure
	if !errors.As(err, &failure) || failure.Reason != "mutation_context_cancelled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result != (Result{}) || submitted != 0 || marked != wantMarked {
		t.Fatalf("result=%+v marked=%d submitted=%d", result, marked, submitted)
	}
}

func TestInvalidReadbackNilCauseIsSafe(t *testing.T) {
	var invalid *InvalidReadback
	if got := invalid.Error(); got == "" {
		t.Fatal("nil InvalidReadback returned an empty error")
	}
	if got := (&InvalidReadback{}).Error(); got == "" {
		t.Fatal("nil-cause InvalidReadback returned an empty error")
	}
}
