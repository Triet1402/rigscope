package storage

import (
	"errors"
	"fmt"
	"time"
)

// ErrorCode classifies a storage failure for the HTTP/WS layer. The string
// values are stable: callers may map them directly to API error codes.
type ErrorCode string

const (
	// CodeInvalid means the caller supplied invalid input.
	CodeInvalid ErrorCode = "invalid"
	// CodeNotFound means the requested row does not exist.
	CodeNotFound ErrorCode = "not-found"
	// CodeConflict means the operation violates a durable invariant, such as
	// removing the last active administrator or enrolling an existing
	// collector without a replacement grant.
	CodeConflict ErrorCode = "conflict"
	// CodeForbidden means a storage-level invariant requires an actor the
	// caller did not prove. Repository methods that need an actor report it.
	CodeForbidden ErrorCode = "forbidden"
	// CodeUnauthenticated means the originating browser session is no longer
	// present or has expired. Mutations answer 401 rather than acting on a
	// session that was revoked mid-request.
	CodeUnauthenticated ErrorCode = "unauthenticated"
	// CodeUnavailable means storage cannot currently serve the operation for a
	// reason that is neither caller error nor pressure (for example I/O or a
	// corrupt page).
	CodeUnavailable ErrorCode = "unavailable"
	// CodeBusy means the bounded write path or SQLite lock is saturated. A
	// collector should keep its batch and retry.
	CodeBusy ErrorCode = "busy"
	// CodeStorageFull means the database size budget or the filesystem is
	// full. It is also a retry/busy condition for collectors, and the hub must
	// surface it as a degraded state.
	CodeStorageFull ErrorCode = "storage-full"
	// CodeStoragePressure means the configured physical budget (main file plus
	// WAL) is exhausted even after a checkpoint attempt. The write did not run
	// and the caller should retry after retention makes room; the hub surfaces
	// it as the same visible degraded state as CodeStorageFull.
	CodeStoragePressure ErrorCode = "storage-pressure"
	// CodeClosed means the database or telemetry store is shutting down.
	CodeClosed ErrorCode = "closed"
)

// Error is a classified storage failure. Errors are values: callers may use
// errors.As/errors.Is and CodeOf instead of matching strings.
type Error struct {
	Code       ErrorCode
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil && e.Message == "" {
		return e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf reports the storage classification of err, or "" when err is not a
// storage error. It looks through wrapping.
func CodeOf(err error) ErrorCode {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

// RetryAfter reports the minimum retry delay for a busy/full error. It returns
// zero for every other error.
func RetryAfter(err error) time.Duration {
	var target *Error
	if errors.As(err, &target) {
		return target.RetryAfter
	}
	return 0
}

// IsBusy reports whether err asks the caller to retry the same work later.
func IsBusy(err error) bool {
	code := CodeOf(err)
	return code == CodeBusy || code == CodeStorageFull || code == CodeStoragePressure
}

func storageError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrapStorageError(code ErrorCode, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}

func invalidError(format string, args ...any) *Error {
	return storageError(CodeInvalid, format, args...)
}

func notFoundError(format string, args ...any) *Error {
	return storageError(CodeNotFound, format, args...)
}

func conflictError(format string, args ...any) *Error {
	return storageError(CodeConflict, format, args...)
}

func forbiddenError(format string, args ...any) *Error {
	return storageError(CodeForbidden, format, args...)
}

func unauthenticatedError(format string, args ...any) *Error {
	return storageError(CodeUnauthenticated, format, args...)
}
