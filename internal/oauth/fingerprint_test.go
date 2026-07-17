// Tests for spec 04§1.5 (credential_fingerprint).

package oauth

import (
	"strings"
	"testing"
)

func deref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestCredentialFingerprint(t *testing.T) {
	oldGen := `{"claudeAiOauth": {"accessToken": "sk-old", "refreshToken": "rt-1"}}`
	newGen := `{"claudeAiOauth": {"accessToken": "sk-new", "refreshToken": "rt-1", "expiresAt": 5}}`

	fpOld := CredentialFingerprint(oldGen)
	fpNew := CredentialFingerprint(newGen)
	if fpOld == nil || fpNew == nil {
		t.Fatalf("nil fingerprint for real bytes: old=%v new=%v", fpOld, fpNew)
	}
	if !strings.HasPrefix(*fpOld, "sha256:") {
		t.Errorf("refresh-token fingerprint = %q, want sha256: prefix", *fpOld)
	}
	if *fpOld != *fpNew {
		t.Errorf("rotation not stable: %q != %q", *fpOld, *fpNew)
	}

	diff := CredentialFingerprint(`{"claudeAiOauth": {"refreshToken": "rt-2"}}`)
	if diff == nil || *diff == *fpOld {
		t.Errorf("different refresh token should differ: %q vs %q", deref(diff), deref(fpOld))
	}

	raw := CredentialFingerprint("raw-token")
	if raw == nil || !strings.HasPrefix(*raw, "sha256-full:") {
		t.Errorf("raw string fingerprint = %q, want sha256-full: prefix", deref(raw))
	}

	setupToken := CredentialFingerprint(`{"claudeAiOauth": {"accessToken": "sk-ant-oat01-abc"}}`)
	if setupToken == nil || !strings.HasPrefix(*setupToken, "sha256-full:") {
		t.Errorf("setup-token fingerprint = %q, want sha256-full: prefix", deref(setupToken))
	}

	if got := CredentialFingerprint(""); got != nil {
		t.Errorf("empty input fingerprint = %q, want nil", *got)
	}
}

func TestCredentialFingerprintPrefixesNeverCollide(t *testing.T) {
	// A refresh-token hash (sha256:) and a full-content hash (sha256-full:)
	// carry distinct prefixes so they can never compare equal.
	withRefresh := CredentialFingerprint(`{"claudeAiOauth": {"refreshToken": "x"}}`)
	fullContent := CredentialFingerprint("x")
	if withRefresh == nil || fullContent == nil {
		t.Fatal("nil fingerprint")
	}
	if *withRefresh == *fullContent {
		t.Errorf("prefixes collided: %q == %q", *withRefresh, *fullContent)
	}
}
