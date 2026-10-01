package assurance

import (
	"errors"
	"fmt"
)

var (
	ErrNotReady          = errors.New("assurance: not ready")
	ErrInvalidTransition = errors.New("assurance: invalid state transition")
)

// StructuredError is the replay-safe public error envelope.
type StructuredError struct {
	Code                   string `json:"code"`
	Message                string `json:"message"`
	Retryable              bool   `json:"retryable"`
	ReconciliationRequired bool   `json:"reconciliation_required"`
	NLAuditID              string `json:"nl_audit_id,omitempty"`
}

// Error is a typed domain failure with a stable machine code.
type Error struct {
	Code                   string
	Message                string
	Retryable              bool
	ReconciliationRequired bool
	Cause                  error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Cause }

func domainError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func wrapError(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

func asStructured(err error, auditID string) StructuredError {
	var typed *Error
	if errors.As(err, &typed) {
		return StructuredError{
			Code: typed.Code, Message: typed.Message, Retryable: typed.Retryable,
			ReconciliationRequired: typed.ReconciliationRequired, NLAuditID: auditID,
		}
	}
	return StructuredError{Code: "internal_error", Message: fmt.Sprintf("assurance operation failed: %v", err), Retryable: false, NLAuditID: auditID}
}
