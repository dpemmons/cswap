// Usage-fetch error classification into stable string tokens.
//
// Implements spec 04§1.16 (_classify_usage_error). The precedence is
// HTTPError → timeout → network(URLError) → bad-response(JSONDecodeError) →
// type-name fallback (04§1.16 ordering note). Retry-After is parsed on the
// HTTPError path only: seconds form, negatives clamp to 0, HTTP-date form ->
// nil (04§1.16 / §7.5).

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// HTTPError is returned by Client.Usage for a non-2xx final response. It carries
// the status code, the (bounded) body, and the raw Retry-After header value so
// callers can classify and quarantine. Exported so fakes and tests can
// construct the 401/429 paths.
type HTTPError struct {
	Code       int
	Body       []byte
	RetryAfter string // raw Retry-After header value, "" if absent
}

// Error implements error.
func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

// classifyUsageError maps a usage-fetch error to (kind, retry_after_s).
func classifyUsageError(err error) (string, *float64) {
	var he *HTTPError
	if errors.As(err, &he) {
		return fmt.Sprintf("http-%d", he.Code), parseRetryAfter(he.RetryAfter)
	}
	if isTimeout(err) {
		return ErrTimeout, nil
	}
	// net/http surfaces transport failures as *url.Error, which satisfies
	// net.Error; a non-timeout net.Error is a network error (the URLError
	// branch in Python).
	var ne net.Error
	if errors.As(err, &ne) {
		return ErrNetwork, nil
	}
	// A decode failure on a 2xx body (never a net.Error) is a bad response.
	var synErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &synErr) || errors.As(err, &typeErr) || errors.Is(err, errBadResponse) {
		return ErrBadResponse, nil
	}
	return fmt.Sprintf("%T", err), nil
}

// parseRetryAfter parses a Retry-After header value: seconds form only,
// negatives clamped to 0, non-numeric (HTTP-date) -> nil.
func parseRetryAfter(raw string) *float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return nil
	}
	if f < 0 {
		f = 0
	}
	return &f
}

// isTimeout reports whether err is (or wraps) a timeout: a context deadline or
// a net.Error whose Timeout() is true. Mirrors the TimeoutError /
// URLError(TimeoutError) branches (04§1.16).
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// errBadResponse wraps a JSON-decode failure of a 2xx usage body so the
// classifier maps it to "bad-response" even after transport unwrapping.
var errBadResponse = errors.New("bad-response")
