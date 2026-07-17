// Numeric/JSON coercion helpers shared across the package.
//
// Implements the isinstance(x, (int, float)) and truthiness checks from
// spec 04§1 (oauth.py) in Go terms: raw usage/credential maps decode with
// json.Number (to preserve the int-vs-float distinction for A1 fidelity), so
// numeric probes must accept json.Number as well as native Go numbers.

package oauth

import "encoding/json"

// numFloat reports whether v is a JSON number (json.Number) or a native Go
// numeric type, returning its float64 value. Matches Python isinstance(v,
// (int, float)) — strings and bools are rejected.
func numFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

// isNumber reports whether v is a JSON/Go number.
func isNumber(v any) bool {
	_, ok := numFloat(v)
	return ok
}

// truthyStr reports whether v is a non-empty string (Python truthiness for the
// string-typed fields this package reads: refreshToken, resets_at, scope).
func truthyStr(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// truthyMap reports whether v is a non-empty map (Python `if some_dict:`).
func truthyMap(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) > 0
}
