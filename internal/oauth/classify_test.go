// Tests for spec 04§1.16 (_classify_usage_error) and the Retry-After parsing
// rules (04§7.5).

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
)

// timeoutNetErr is a net.Error whose Timeout() is true (the URLError(TimeoutError)
// analogue).
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return false }

func badJSONErr(t *testing.T) error {
	t.Helper()
	var m map[string]any
	err := json.Unmarshal([]byte("{not json"), &m)
	if err == nil {
		t.Fatal("expected a json error")
	}
	return err
}

func TestClassifyUsageError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantKind  string
		wantRetry *float64
	}{
		{"http 429 with retry-after", &HTTPError{Code: 429, RetryAfter: "42"}, "http-429", f64(42)},
		{"http 500 no retry-after", &HTTPError{Code: 500}, "http-500", nil},
		{"context deadline is timeout", context.DeadlineExceeded, "timeout", nil},
		{"net timeout is timeout", timeoutNetErr{}, "timeout", nil},
		{"conn refused is network", &net.OpError{Op: "dial", Err: errors.New("refused")}, "network", nil},
		{"json decode is bad-response", badJSONErr(t), "bad-response", nil},
		{"wrapped bad-response sentinel", fmt.Errorf("usage decode: %w", errBadResponse), "bad-response", nil},
		{"fallback type name", errors.New("weird"), "*errors.errorString", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, retry := classifyUsageError(tc.err)
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if !eqFloatPtr(retry, tc.wantRetry) {
				t.Errorf("retry = %v, want %v", ptrStr(retry), ptrStr(tc.wantRetry))
			}
		})
	}
}

func TestClassifyPrecedenceTimeoutBeforeNetwork(t *testing.T) {
	// A url.Error-style wrapper around a timeout must classify as timeout, not
	// network (04§1.16 ordering).
	wrapped := fmt.Errorf("Get: %w", timeoutNetErr{})
	if kind, _ := classifyUsageError(wrapped); kind != "timeout" {
		t.Errorf("wrapped timeout kind = %q, want timeout", kind)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		raw  string
		want *float64
	}{
		{"42", f64(42)},
		{"0", f64(0)},
		{"-5", f64(0)}, // negatives clamp to 0
		{"  30  ", f64(30)},
		{"Fri, 04 Jul 2026 12:00:00 GMT", nil}, // HTTP-date form ignored
		{"", nil},                              // missing
		{"notanumber", nil},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got := parseRetryAfter(tc.raw)
			if !eqFloatPtr(got, tc.want) {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.raw, ptrStr(got), ptrStr(tc.want))
			}
		})
	}
}

func f64(v float64) *float64 { return &v }

func eqFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func ptrStr(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%g", *p)
}
