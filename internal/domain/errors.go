package domain

import "errors"

// Domain errors. Adapters translate their library errors into these at their boundary,
// so that pgx.ErrNoRows or a *url.Error never reaches a use case.
var (
	// ErrNotFound reports that a requested entity does not exist.
	ErrNotFound = errors.New("domain: not found")

	// ErrConflict reports that an operation would violate uniqueness or a concurrent
	// modification was detected.
	ErrConflict = errors.New("domain: conflict")

	// ErrInvalidTransition reports an attempt to move a state machine along an edge
	// that does not exist.
	ErrInvalidTransition = errors.New("domain: invalid state transition")

	// ErrValidation reports that a value or entity failed an invariant check.
	ErrValidation = errors.New("domain: validation failed")

	// ErrNotPermitted reports that an operation is forbidden by policy, for example
	// dispatching a check against a source whose compliance status disallows it.
	ErrNotPermitted = errors.New("domain: not permitted by policy")
)

// ValidationError describes which field failed an invariant and why. It wraps
// ErrValidation so callers can use errors.Is for the class and a type assertion for
// the detail.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return "domain: validation failed: " + e.Message
	}
	return "domain: validation failed: " + e.Field + ": " + e.Message
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// invalid is a helper for constructing ValidationError values.
func invalid(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

// TransitionError describes a rejected state transition.
type TransitionError struct {
	Entity string
	From   string
	To     string
}

func (e *TransitionError) Error() string {
	return "domain: invalid state transition for " + e.Entity + ": " + e.From + " -> " + e.To
}

func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }
