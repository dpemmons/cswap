// Package cerr is the typed error set mirroring the Python exception hierarchy.
//
// Implements spec 08§11 (exceptions & exit-code mapping) and 02§18. Each Kind
// string is byte-identical to the Python class name because it is the external
// contract for the JSON error envelope's error.type field. Error() returns the
// message alone (matching Python str(exc)); wrapped errors travel via Unwrap.
package cerr

import (
	"errors"
	"fmt"
)

// Kind is the error's type tag; its string value equals the Python class name.
type Kind string

// Kind values, one per Python exception class.
const (
	KindConfig                Kind = "ConfigError"
	KindSwitch                Kind = "SwitchError"
	KindSession               Kind = "SessionError"
	KindValidation            Kind = "ValidationError"
	KindAccountNotFound       Kind = "AccountNotFoundError"
	KindCredential            Kind = "CredentialError"
	KindCredentialRead        Kind = "CredentialReadError"
	KindCredentialWrite       Kind = "CredentialWriteError"
	KindLock                  Kind = "LockError"
	KindClaudeCodeLockTimeout Kind = "ClaudeCodeLockTimeout"
	KindTransfer              Kind = "TransferError"
	KindMigration             Kind = "MigrationError"
	KindMigrationIncomplete   Kind = "MigrationIncomplete"
)

// Error is a domain error carrying a Kind and message.
type Error struct {
	Kind    Kind
	Msg     string
	wrapped error
}

// Error returns the message alone, matching Python's str(exc).
func (e *Error) Error() string { return e.Msg }

// Unwrap returns the wrapped cause, if any.
func (e *Error) Unwrap() error { return e.wrapped }

// Wrap attaches an underlying cause and returns the receiver for chaining, e.g.
// cerr.Config("Generated invalid JSON").Wrap(jsonErr).
func (e *Error) Wrap(cause error) *Error {
	e.wrapped = cause
	return e
}

func newError(kind Kind, format string, a ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, a...)}
}

// Config builds a ConfigError.
func Config(format string, a ...any) *Error { return newError(KindConfig, format, a...) }

// Switch builds a SwitchError.
func Switch(format string, a ...any) *Error { return newError(KindSwitch, format, a...) }

// Session builds a SessionError.
func Session(format string, a ...any) *Error { return newError(KindSession, format, a...) }

// Validation builds a ValidationError.
func Validation(format string, a ...any) *Error { return newError(KindValidation, format, a...) }

// AccountNotFound builds an AccountNotFoundError.
func AccountNotFound(format string, a ...any) *Error {
	return newError(KindAccountNotFound, format, a...)
}

// Credential builds a CredentialError.
func Credential(format string, a ...any) *Error { return newError(KindCredential, format, a...) }

// CredentialRead builds a CredentialReadError.
func CredentialRead(format string, a ...any) *Error {
	return newError(KindCredentialRead, format, a...)
}

// CredentialWrite builds a CredentialWriteError.
func CredentialWrite(format string, a ...any) *Error {
	return newError(KindCredentialWrite, format, a...)
}

// Lock builds a LockError.
func Lock(format string, a ...any) *Error { return newError(KindLock, format, a...) }

// ClaudeCodeLockTimeout builds a ClaudeCodeLockTimeout (a LockError subtype in
// Python; a distinct Kind here). Nothing has been mutated when it is raised.
func ClaudeCodeLockTimeout(format string, a ...any) *Error {
	return newError(KindClaudeCodeLockTimeout, format, a...)
}

// Transfer builds a TransferError.
func Transfer(format string, a ...any) *Error { return newError(KindTransfer, format, a...) }

// Migration builds a MigrationError.
func Migration(format string, a ...any) *Error { return newError(KindMigration, format, a...) }

// MigrationIncomplete builds a MigrationIncomplete.
func MigrationIncomplete(format string, a ...any) *Error {
	return newError(KindMigrationIncomplete, format, a...)
}

// IsClaudeSwitchError reports whether err is (or wraps) any *Error, mirroring an
// isinstance check against the ClaudeSwitchError base class.
func IsClaudeSwitchError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// TypeName returns the Kind string for the first *Error in err's chain (the
// JSON error.type), or "" if err is not a domain error.
func TypeName(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return string(e.Kind)
	}
	return ""
}
