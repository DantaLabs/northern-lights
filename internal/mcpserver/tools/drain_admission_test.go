package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dantalabs/northern-lights/internal/assurance"
	"github.com/dantalabs/northern-lights/internal/mcpserver"
)

// A provider call can outlive the SQLite claim. The admission token must
// span its whole HTTP call, not just the initial local transaction.
func TestDrainWaitsForLongProviderCallAndRejectsNewMCPPost(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	env := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/iam/v1/oauth2/token" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"test","expires_in":3600}`))
			return
		}
		close(entered)
		<-finish
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	gate := assurance.NewDrainGate()
	env.deps.DrainGate = gate
	reg := mcpserver.NewRegistry()
	reg.Register(ReadRange())
	handler, err := mcpserver.New(env.deps, reg, &mcpserver.Options{APIToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	client := &rawMCPClient{t: t, url: srv.URL + "/mcp"}
	if response, body := client.post("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "drain", "version": "1"}}); response.StatusCode != 200 {
		t.Fatalf("initialize %d %s", response.StatusCode, body)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.post("tools/call", map[string]any{"name": "workiva_read_range", "arguments": map[string]any{"spreadsheet_id": "sp-1", "sheet_id": "sh-1", "range": "A1"}})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(finish)
		t.Fatal("provider did not start")
	}
	drainDone := make(chan error, 1)
	go func() {
		release, err := gate.BeginDrain(context.Background())
		if err == nil {
			release()
		}
		drainDone <- err
	}()
	deadline := time.After(3 * time.Second)
	for !gate.Draining() {
		select {
		case <-deadline:
			close(finish)
			t.Fatal("drain did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case err := <-drainDone:
		close(finish)
		t.Fatalf("drain passed active provider call: %v", err)
	default:
	}
	if response, _ := client.post("tools/list", map[string]any{}); response.StatusCode != http.StatusServiceUnavailable {
		close(finish)
		t.Fatalf("new POST status %d", response.StatusCode)
	}
	close(finish)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not finish")
	}
	select {
	case err := <-drainDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain stuck")
	}
	if release, err := gate.EnterWrite(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

func TestDrainCancellationAndConcurrentBackupAdmission(t *testing.T) {
	gate := assurance.NewDrainGate()
	release, err := gate.EnterWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := gate.BeginDrain(ctx); done <- err }()
	deadline := time.After(time.Second)
	for !gate.Draining() {
		select {
		case <-deadline:
			t.Fatal("no drain")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := gate.BeginDrain(context.Background()); !errors.Is(err, assurance.ErrBackupDraining) {
		t.Fatalf("concurrent drain %v", err)
	}
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation %v", err)
	}
	release()
	if release, err := gate.EnterWrite(context.Background()); err != nil {
		t.Fatalf("not reopened: %v", err)
	} else {
		release()
	}
}
