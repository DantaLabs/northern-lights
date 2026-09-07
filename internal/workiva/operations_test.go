package workiva

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWaitOperationPollsUntilCompleted(t *testing.T) {
	var polls int
	var mu sync.Mutex
	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/operations/op-1" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Write(loadFixture(t, "operation_started.json"))
			return
		}
		w.Write(loadFixture(t, "operation_completed.json"))
	}))

	resourceURL, err := c.WaitOperation(context.Background(), "/operations/op-1")
	if err != nil {
		t.Fatalf("WaitOperation: %v", err)
	}
	want := "https://api.eu.wdesk.com/spreadsheets/s-1/sheets/sh-1/data"
	if resourceURL != want {
		t.Errorf("resourceURL = %q, want %q", resourceURL, want)
	}
	if polls != 2 {
		t.Errorf("polls = %d, want 2 (started then completed)", polls)
	}
}

func TestWaitOperationFailedReturnsError(t *testing.T) {
	var polls int
	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"op-1","status":"failed","error":{"message":"cell range is invalid"}}`))
	}))

	_, err := c.WaitOperation(context.Background(), "/operations/op-1")
	if err == nil {
		t.Fatal("expected error for failed operation, got nil")
	}
	if !strings.Contains(err.Error(), "cell range is invalid") {
		t.Errorf("error = %v, want it to contain the operation error message", err)
	}
	if polls != 1 {
		t.Errorf("polls = %d, want 1 (no polling after failure)", polls)
	}
}

func TestWaitOperationRespectsRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var delays []time.Duration
	var polls int

	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			w.Write(loadFixture(t, "operation_started.json"))
			return
		}
		w.Write(loadFixture(t, "operation_completed.json"))
	}))
	c.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		delays = append(delays, d)
		mu.Unlock()
		return nil
	}

	_, err := c.WaitOperation(context.Background(), "/operations/op-1")
	if err != nil {
		t.Fatalf("WaitOperation: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delays) == 0 {
		t.Fatal("no sleep recorded between polls")
	}
	if delays[0] != 7*time.Second {
		t.Errorf("first poll delay = %v, want 7s from Retry-After", delays[0])
	}
}

func TestWaitOperationTimesOut(t *testing.T) {
	orig := operationPollTimeout
	operationPollTimeout = 100 * time.Millisecond
	t.Cleanup(func() { operationPollTimeout = orig })

	c, _ := setupFastClient(t, tokenResponder(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "operation_started.json"))
	}))

	start := time.Now()
	_, err := c.WaitOperation(context.Background(), "/operations/op-1")
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout test took %v, want fast return", elapsed)
	}
}
