package app

import "fmt"

// ErrorKind classifies application-service failures without importing a public adapter.
type ErrorKind string

const (
	InvalidArgument    ErrorKind = "invalid_argument"
	NotFound           ErrorKind = "not_found"
	Conflict           ErrorKind = "conflict"
	AlreadyTerminal    ErrorKind = "already_terminal"
	AlreadyDecided     ErrorKind = "already_decided"
	WrongDecisionKind  ErrorKind = "wrong_decision_kind"
	StorageUnavailable ErrorKind = "storage_unavailable"
)

// Error carries stable service context for adapters to translate.
type Error struct {
	Kind   ErrorKind
	Op     string
	JobID  string
	ItemID string
	Cause  error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Op, e.Kind)
	}
	return fmt.Sprintf("%s: %s: %v", e.Op, e.Kind, e.Cause)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
