// Package cpeerr defines the typed error used across cpe-labs packages.
//
// The package is protocol-agnostic. The FaultCode field is reserved for
// protocol fault codes (CWMP for TR-069, USP error codes for TR-369) and
// must remain zero for non-protocol errors. Categorization rides on Kind;
// FaultCode is a value carrier, not a discriminator.
package cpeerr

import (
	"errors"
	"fmt"
)

// Kind categorizes errors. Adding a Kind is a deliberate change to this
// package; protocol-specific categorization belongs on FaultCode.
type Kind int

const (
	// KindUnknown is the zero value. Prefer a more specific Kind.
	KindUnknown Kind = iota
	// KindInvalidArgument means the caller supplied invalid input.
	KindInvalidArgument
	// KindNotFound means a requested resource does not exist.
	KindNotFound
	// KindInternal means an unexpected internal failure.
	KindInternal
)

// String returns the Kind name, useful for logs and error messages.
func (k Kind) String() string {
	switch k {
	case KindInvalidArgument:
		return "invalid_argument"
	case KindNotFound:
		return "not_found"
	case KindInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// Error is the typed error used across cpe-labs packages.
//
// FaultCode is reserved for protocol fault codes (CWMP / USP). Leave 0
// for non-protocol errors.
type Error struct {
	Op        string // operation name, e.g. "tree.Get"
	Kind      Kind   // category
	FaultCode int    // 0 means "not a protocol fault"
	Err       error  // wrapped cause; may be nil
}

// Error returns a stable, log-friendly representation.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	switch {
	case e.Err != nil && e.FaultCode != 0:
		return fmt.Sprintf("%s: %s [fault=%d]: %v", e.Op, e.Kind, e.FaultCode, e.Err)
	case e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Kind, e.Err)
	case e.FaultCode != 0:
		return fmt.Sprintf("%s: %s [fault=%d]", e.Op, e.Kind, e.FaultCode)
	default:
		return fmt.Sprintf("%s: %s", e.Op, e.Kind)
	}
}

// Unwrap returns the wrapped cause for use with errors.Is and errors.As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Wrap returns a new *Error with the given op, kind, and cause. Cause may
// be nil. The returned value is never nil.
func Wrap(op string, kind Kind, cause error) *Error {
	return &Error{Op: op, Kind: kind, Err: cause}
}

// WithFaultCode returns a copy of e with FaultCode set to code. Returns
// nil if e is nil.
func WithFaultCode(e *Error, code int) *Error {
	if e == nil {
		return nil
	}
	cp := *e
	cp.FaultCode = code
	return &cp
}

// Is reports whether err (or any error it wraps) is an *Error with the
// given Kind.
func Is(err error, kind Kind) bool {
	for err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			if typed.Kind == kind {
				return true
			}
			err = typed.Err
			continue
		}
		return false
	}
	return false
}
