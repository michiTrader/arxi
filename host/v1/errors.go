package v1

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable machine-readable failure category. Error messages and
// wrapped causes are diagnostic and are not compatibility promises.
type ErrorCode string

const (
	CodeInvalidArgument       ErrorCode = "invalid_argument"
	CodeNotFound              ErrorCode = "not_found"
	CodeConflict              ErrorCode = "conflict"
	CodeAlreadyTerminal       ErrorCode = "already_terminal"
	CodeAlreadyDecided        ErrorCode = "already_decided"
	CodeWrongDecisionKind     ErrorCode = "wrong_decision_kind"
	CodePermissionDenied      ErrorCode = "permission_denied"
	CodeCapabilityUnavailable ErrorCode = "capability_unavailable"
	CodeSlowConsumer          ErrorCode = "slow_consumer"
	CodeStorageUnavailable    ErrorCode = "storage_unavailable"
	CodeInternal              ErrorCode = "internal"
)

// Error is a host failure with stable machine-readable context. Cause is
// available through errors.Is/errors.As but omitted from serialized output.
type Error struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	Operation string    `json:"operation,omitempty"`
	JobID     JobID     `json:"job_id,omitempty"`
	ItemID    ItemID    `json:"item_id,omitempty"`
	AfterSeq  int64     `json:"after_seq,omitempty"`
	cause     error
}

// NewError constructs a stable host error. Callers may add JobID, ItemID, or
// AfterSeq to the returned value when those fields are relevant.
func NewError(code ErrorCode, operation, message string, cause error) *Error {
	return &Error{Code: code, Operation: operation, Message: message, cause: cause}
}

func newError(code ErrorCode, operation, message string, cause error) *Error {
	return NewError(code, operation, message, cause)
}

// Error implements error.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	prefix := string(e.Code)
	if e.Operation != "" {
		prefix = e.Operation + ": " + prefix
	}
	if e.Message == "" {
		return prefix
	}
	return fmt.Sprintf("%s: %s", prefix, e.Message)
}

// Unwrap exposes a private cause without publishing it as data.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// ErrorCodeOf returns a stable host code, or CodeInternal for an unclassified
// error. Nil has no code.
func ErrorCodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var hostErr *Error
	if errors.As(err, &hostErr) {
		return hostErr.Code
	}
	return CodeInternal
}

// IsCode reports whether err, including a wrapped host error, has code.
func IsCode(err error, code ErrorCode) bool { return ErrorCodeOf(err) == code }
