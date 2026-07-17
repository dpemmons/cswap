package cerr

import (
	"errors"
	"fmt"
	"testing"
)

func TestTypeNameRoundTrip(t *testing.T) {
	cases := []struct {
		err  *Error
		want string
	}{
		{Config("boom"), "ConfigError"},
		{Switch("boom"), "SwitchError"},
		{Session("boom"), "SessionError"},
		{Validation("boom"), "ValidationError"},
		{AccountNotFound("boom"), "AccountNotFoundError"},
		{Credential("boom"), "CredentialError"},
		{CredentialRead("boom"), "CredentialReadError"},
		{CredentialWrite("boom"), "CredentialWriteError"},
		{Lock("boom"), "LockError"},
		{ClaudeCodeLockTimeout("boom"), "ClaudeCodeLockTimeout"},
		{Transfer("boom"), "TransferError"},
		{Migration("boom"), "MigrationError"},
		{MigrationIncomplete("boom"), "MigrationIncomplete"},
	}
	for _, tt := range cases {
		if got := TypeName(tt.err); got != tt.want {
			t.Errorf("TypeName(%v) = %q, want %q", tt.err.Kind, got, tt.want)
		}
		if !IsClaudeSwitchError(tt.err) {
			t.Errorf("IsClaudeSwitchError(%q) = false", tt.want)
		}
	}
}

func TestErrorMessageIsBare(t *testing.T) {
	// str(exc) parity: Error() is the message alone, no "Kind: " prefix.
	e := Switch("boom")
	if e.Error() != "boom" {
		t.Errorf("Error() = %q, want %q", e.Error(), "boom")
	}
	e2 := Config("Generated invalid JSON")
	if e2.Error() != "Generated invalid JSON" {
		t.Errorf("Error() = %q", e2.Error())
	}
}

func TestFormatting(t *testing.T) {
	e := AccountNotFound("no account %q at slot %d", "a@b.com", 3)
	if e.Error() != `no account "a@b.com" at slot 3` {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestWrapUnwrapAndErrorsAs(t *testing.T) {
	cause := fmt.Errorf("underlying")
	e := Config("Generated invalid JSON").Wrap(cause)
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is did not find wrapped cause")
	}
	var de *Error
	if !errors.As(e, &de) {
		t.Fatalf("errors.As(*Error) failed")
	}
	if de.Unwrap() != cause {
		t.Errorf("Unwrap() = %v, want %v", de.Unwrap(), cause)
	}
	// Wrapping a *Error still classifies via the outer error.
	wrapped := fmt.Errorf("context: %w", Lock("held"))
	if TypeName(wrapped) != "LockError" {
		t.Errorf("TypeName(wrapped) = %q, want LockError", TypeName(wrapped))
	}
	if !IsClaudeSwitchError(wrapped) {
		t.Errorf("IsClaudeSwitchError(wrapped) = false")
	}
}

func TestTypeNameNonDomainError(t *testing.T) {
	if got := TypeName(errors.New("plain")); got != "" {
		t.Errorf("TypeName(plain) = %q, want \"\"", got)
	}
	if IsClaudeSwitchError(errors.New("plain")) {
		t.Errorf("IsClaudeSwitchError(plain) = true")
	}
	if IsClaudeSwitchError(nil) {
		t.Errorf("IsClaudeSwitchError(nil) = true")
	}
}
