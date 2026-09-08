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

type operationError struct {
	Message string `json:"message"`
}

// WaitOperation polls an async operation URL until the operation
// completes and returns the resourceUrl of the mutated resource.
// Polling honors the Retry-After response header (defaulting to one
// second, matching the operations rate limit) and gives up after
// operationPollTimeout (60 seconds by default). An operation that ends
// failed is an error and stops polling immediately.
func (c *Client) WaitOperation(ctx context.Context, opURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, operationPollTimeout)
	defer cancel()

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
			return "", fmt.Errorf("operation %s failed: %s", op.ID, msg)
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
