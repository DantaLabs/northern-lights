package assurance

import "context"

type requestIDContextKey struct{}

// WithRequestID carries the server-generated ingress correlation into the
// assurance service boundary. It is never accepted from a tool payload.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

func RequestIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return value
}
