package reexec

import (
	"errors"
	"strconv"
)

// ExitError is an entrypoint error that carries the process exit status the
// child should terminate with when it is returned from an entrypoint run by
// [RunEntrypoint].
type ExitError struct {
	// Code is the exit status. It must be in the range 1..125 so that it cannot
	// be confused with a signal death or with a status the shell reserves.
	Code int
	// Err is the underlying cause. It may be nil.
	Err error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return "reexec: entrypoint exited with status " + strconv.Itoa(e.Code)
	}
	return "reexec: entrypoint exited with status " + strconv.Itoa(e.Code) + ": " + e.Err.Error()
}

// Unwrap returns the underlying cause.
func (e *ExitError) Unwrap() error { return e.Err }

// WithExitCode returns an error that makes [RunEntrypoint] exit with code.
func WithExitCode(code int, err error) error {
	if code <= 0 || code > 125 {
		// Out-of-range codes would be indistinguishable from a signal death, so
		// keep the original error and let RunEntrypoint use ExitFailure.
		return err
	}
	return &ExitError{Code: code, Err: err}
}

// exitCodeFor maps an entrypoint error to a process exit status.
func exitCodeFor(err error) int {
	var exitErr *ExitError
	if errors.As(err, &exitErr) && exitErr.Code > 0 && exitErr.Code <= 125 {
		return exitErr.Code
	}
	return ExitFailure
}
