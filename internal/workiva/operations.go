package workiva

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dantalabs/northern-lights/internal/ratelimit"
)

// operationPollTimeout bounds how long WaitOperation polls before
// giving up. Declared as a variable so tests can shorten it.
var operationPollTimeout = 60 * time.Second

// operationResponse is the decoded body of the operations endpoint.
type operationResponse struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	ResourceURL string          `json:"resourceUrl"`
	Error       *operationError `json:"error,omitempty"`
}

// OperationInspection is the public read-only operation evidence returned by
// InspectOperation. It contains no credentials and is never used to submit.
type OperationInspection struct {
	Reference   string
	Status      string
	ResourceURL string
}

type operationError struct {
	Message string `json:"message"`
}

// OperationFailedError means Workiva reported a documented terminal failed
// status, so the mutation outcome is definite rather than unknown.
type OperationFailedError struct{ ID, Message string }

func (e *OperationFailedError) Error() string {
	return fmt.Sprintf("operation %s failed: %s", e.ID, e.Message)
}

// WaitOperation polls an async operation URL until the operation
// completes and returns the resourceUrl of the mutated resource.
// Polling honors the Retry-After response header (defaulting to one
// second, matching the operations rate limit) and gives up after
// operationPollTimeout (60 seconds by default). An operation that ends
// failed is an error and stops polling immediately.
func (c *Client) WaitOperation(ctx context.Context, opURL string) (string, error) {
	return c.WaitOperationWithInitialRetryAfter(ctx, opURL, 0)
}

// InspectOperation performs one bounded, read-only GET of an existing
// operation. It never waits, retries via POST, or changes provider state.
func (c *Client) InspectOperation(ctx context.Context, opURL string) (OperationInspection, error) {
	resp, err := c.Do(ctx, http.MethodGet, opURL, nil, ratelimit.CategoryOperations)
	if err != nil {
		return OperationInspection{}, fmt.Errorf("inspect operation: %w", err)
	}
	var op operationResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&op)
	closeErr := resp.Body.Close()
	if decodeErr != nil {
		if closeErr != nil {
			decodeErr = errors.Join(decodeErr, closeErr)
		}
		return OperationInspection{}, fmt.Errorf("decode operation inspection: %w", decodeErr)
	}
	if closeErr != nil {
		return OperationInspection{}, fmt.Errorf("close operation inspection: %w", closeErr)
	}
	if op.ID == "" || op.Status == "" {
		return OperationInspection{}, errors.New("inspect operation: provider returned incomplete operation")
	}
	return OperationInspection{Reference: opURL, Status: op.Status, ResourceURL: op.ResourceURL}, nil
}

// WaitOperationWithInitialRetryAfter polls an async operation, honoring the
// Retry-After delay returned with the operation's 202 response before the
// first poll. The delay is supplied per operation, not stored on Client.
func (c *Client) WaitOperationWithInitialRetryAfter(ctx context.Context, opURL string, initialRetryAfter time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, operationPollTimeout)
	defer cancel()
	if initialRetryAfter > 0 {
		if err := c.sleep(ctx, initialRetryAfter); err != nil {
			return "", fmt.Errorf("wait operation initial retry-after: %w", err)
		}
	}

	for {
		resp, err := c.Do(ctx, http.MethodGet, opURL, nil, ratelimit.CategoryOperations)
		if err != nil {
			return "", fmt.Errorf("poll operation: %w", err)
		}

		var op operationResponse
		decErr := json.NewDecoder(resp.Body).Decode(&op)
		retryAfter := resp.Header.Get("Retry-After")
		closeErr := resp.Body.Close()
		if decErr != nil {
			if closeErr != nil {
				decErr = errors.Join(decErr, closeErr)
			}
			return "", fmt.Errorf("decode operation response: %w", decErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close operation response: %w", closeErr)
		}

		switch op.Status {
		case "completed":
			return op.ResourceURL, nil
		case "failed":
			msg := "unknown error"
			if op.Error != nil && op.Error.Message != "" {
				msg = op.Error.Message
			}
			return "", &OperationFailedError{ID: op.ID, Message: msg}
		}

		// The limiter (1 request/sec for operations) already paces polls;
		// sleep additionally only when the server asks for a longer wait
		// via Retry-After.
		if d := retryAfterDelay(retryAfter); d > time.Second {
			if err := c.sleep(ctx, d); err != nil {
				return "", fmt.Errorf("wait operation: %w", err)
			}
		}
	}
}
