// Package settings is the `<backup_root>/settings.json` config store: the
// autoswitch policy knobs (AutoSwitchSettings) and the `cswap config`
// get/set/unset machinery.
//
// Implements spec 08§8 (settings.py) and 05§2 (AutoSwitchSettings as consumed
// by the autoswitch engine): SETTING_SPECS as the single source of truth for
// bounds/choices/defaults (used by both the lenient clamp on load and the
// strict cswap-config-set validation), forgiving reads, strict writes,
// merged_with_cli, and parse_model_names.
package settings

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"git.dpemmons.com/dpemmons/cswap/internal/atomicfile"
	"git.dpemmons.com/dpemmons/cswap/internal/cerr"
)

// pyFloat is a float64 whose JSON form matches Python's json.dumps: a
// whole-number value keeps a trailing ".0" (80 -> "80.0"), unlike
// encoding/json which drops it (80 -> "80"). settings.json float leaves must
// stay byte-compatible with what the Python tool wrote (spec 08§8; DESIGN A1
// permits only key-order/indent to differ), so the write paths wrap KindFloat
// leaves in pyFloat before marshaling.
type pyFloat float64

// MarshalJSON renders the value with Go's shortest round-trip form, appending
// ".0" when it carries no fractional/exponent part so whole numbers match
// Python's repr (90.0, 40.0). Bounded settings/pcts never hit inf/NaN; the
// guard falls back to the default encoder if they somehow do.
func (f pyFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return json.Marshal(v)
	}
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return []byte(s), nil
}

// coercePyFloats wraps every present KindFloat leaf (in its section) as pyFloat
// so whole-number floats serialize with a trailing ".0". Values re-read from
// disk arrive as plain float64, so this also re-floats keys we merely
// preserved across a single-key write. Scoped to the known float specs to
// avoid touching int leaves (schemaVersion, unhealthyTicks) or unknown keys.
func coercePyFloats(data map[string]any) {
	for _, spec := range SettingSpecs {
		if spec.Kind != KindFloat {
			continue
		}
		section, ok := data[spec.Section].(map[string]any)
		if !ok {
			continue
		}
		if f, ok := section[spec.JSONKey].(float64); ok {
			section[spec.JSONKey] = pyFloat(f)
		}
	}
}

// SchemaVersion is settings.json's schemaVersion.
const SchemaVersion = 1

// Filename is the settings store's filename under the backup root.
const Filename = "settings.json"

// AutoSwitchSettings is the frozen policy-knob value for the autoswitch
// engine (`cswap auto`). See spec 08§8.2 / 05§2 for the field-by-field
// rationale.
type AutoSwitchSettings struct {
	Threshold             float64
	IntervalSeconds       float64
	CooldownSeconds       float64
	HysteresisPct         float64
	Strategy              string
	IncludeAPIKeyAccounts bool
	UnhealthyTicks        int
	// Model is a comma-separated model display name list (e.g. "Fable" or
	// "Fable,Opus"), "all", or nil (account-wide 5h/7d only, the default).
	Model *string
}

// Default returns the dataclass defaults: threshold 90, intervalSeconds 60,
// cooldownSeconds 300, hysteresisPct 10, strategy "best",
// includeApiKeyAccounts false, unhealthyTicks 3, model nil.
func Default() AutoSwitchSettings {
	return AutoSwitchSettings{
		Threshold:             90.0,
		IntervalSeconds:       60.0,
		CooldownSeconds:       300.0,
		HysteresisPct:         10.0,
		Strategy:              "best",
		IncludeAPIKeyAccounts: false,
		UnhealthyTicks:        3,
		Model:                 nil,
	}
}

// Kind is a setting's value kind, driving both the lenient load-time clamp
// and the strict `cswap config set` parser.
type Kind string

// Kind values, one per settings.json value shape.
const (
	KindFloat  Kind = "float"
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
	KindChoice Kind = "choice"
	KindString Kind = "string"
)

// Spec is one settings.json key's metadata: single source of truth for
// bounds/choices/defaults, shared by the lenient clamp on load and the
// strict `parse_setting_value` used by `cswap config set`.
type Spec struct {
	Section string // top-level JSON section ("autoswitch")
	JSONKey string // camelCase key inside the section
	Field   string // the AutoSwitchSettings Go field name
	Kind    Kind
	Lo, Hi  float64  // used for float/int kinds
	Choices []string // used for the choice kind
	Default any      // float64 | int | bool | string | nil; matches Default()'s field
	Help    string
}

// Dotted returns "section.jsonKey", e.g. "autoswitch.threshold".
func (s Spec) Dotted() string { return s.Section + "." + s.JSONKey }

// SettingSpecs is the single source of truth for every settings.json key, in
// registry order. It must cover every AutoSwitchSettings field, and each
// spec's Default must equal Default()'s corresponding field (both enforced
// by tests).
var SettingSpecs = []Spec{
	{Section: "autoswitch", JSONKey: "threshold", Field: "Threshold", Kind: KindFloat,
		Lo: 50.0, Hi: 99.9, Default: 90.0,
		Help: "Switch when the binding 5h/7d window reaches this pct"},
	{Section: "autoswitch", JSONKey: "intervalSeconds", Field: "IntervalSeconds", Kind: KindFloat,
		Lo: 15.0, Hi: 3600.0, Default: 60.0,
		Help: "Poll interval for the cswap auto loop, in seconds"},
	{Section: "autoswitch", JSONKey: "cooldownSeconds", Field: "CooldownSeconds", Kind: KindFloat,
		Lo: 0.0, Hi: 86400.0, Default: 300.0,
		Help: "Minimum seconds between proactive switches"},
	{Section: "autoswitch", JSONKey: "hysteresisPct", Field: "HysteresisPct", Kind: KindFloat,
		Lo: 0.0, Hi: 50.0, Default: 10.0,
		Help: "A target must beat the active account by this many pct"},
	{Section: "autoswitch", JSONKey: "strategy", Field: "Strategy", Kind: KindChoice,
		Choices: []string{"best"}, Default: "best",
		Help: "How auto-switch picks the target account"},
	{Section: "autoswitch", JSONKey: "includeApiKeyAccounts", Field: "IncludeAPIKeyAccounts", Kind: KindBool,
		Default: false,
		Help:    "Allow rotating onto managed API-key accounts (bill per token)"},
	{Section: "autoswitch", JSONKey: "unhealthyTicks", Field: "UnhealthyTicks", Kind: KindInt,
		Lo: 1, Hi: 100, Default: 3,
		Help: "Consecutive failed polls before an account is unhealthy"},
	{Section: "autoswitch", JSONKey: "model", Field: "Model", Kind: KindString,
		Default: nil,
		Help:    "Also switch on these models' weekly limits (e.g. Fable, Fable,Opus, or all)"},
}

// SpecFor looks up a spec by dotted key; an unknown key returns a
// cerr.Config listing every valid dotted key, in registry order.
func SpecFor(dotted string) (Spec, error) {
	for _, s := range SettingSpecs {
		if s.Dotted() == dotted {
			return s, nil
		}
	}
	keys := make([]string, len(SettingSpecs))
	for i, s := range SettingSpecs {
		keys[i] = s.Dotted()
	}
	return Spec{}, cerr.Config("unknown setting '%s'\nValid keys: %s", dotted, strings.Join(keys, ", "))
}

// SettingsPath returns <root>/settings.json.
func SettingsPath(root string) string { return filepath.Join(root, Filename) }

// --- reading -----------------------------------------------------------

// readRaw is the forgiving read used by Load/Save: a missing file, any read
// error, invalid JSON, or a non-object root all degrade to an empty map
// rather than raising.
func readRaw(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// readRawForWrite is the strict read used by SetSetting/UnsetSetting: a
// corrupt or non-object file errors instead of silently degrading to {},
// since a read-modify-write starting from {} would replace a malformed (and
// maybe hand-recoverable) file with a near-empty one.
func readRawForWrite(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, cerr.Config("could not read %s: %v", path, err)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, cerr.Config("%s is not valid JSON (%v); fix or delete it before changing settings", path, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, cerr.Config("%s is not a JSON object; fix or delete it before changing settings", path)
	}
	return m, nil
}

// Load loads the autoswitch section; a missing/corrupt file, a non-object
// "autoswitch" section, out-of-range numeric values, or bad-type values all
// degrade to (clamped) defaults rather than raising.
func Load(root string) AutoSwitchSettings {
	raw := readRaw(SettingsPath(root))
	section, ok := raw["autoswitch"].(map[string]any)
	if !ok {
		return Default()
	}
	fields := map[string]any{}
	for _, spec := range SettingSpecs {
		if v, present := section[spec.JSONKey]; present {
			fields[spec.Field] = v
		}
	}
	return clamp(fields)
}

// --- clamping ------------------------------------------------------------

// clamp builds an AutoSwitchSettings from a field-name → raw-value map
// (values sourced from JSON, from an existing AutoSwitchSettings via
// fieldsOf, or absent). A present-but-absent field falls back to the spec
// default (already valid, so clamping it is a no-op).
func clamp(fields map[string]any) AutoSwitchSettings {
	out := AutoSwitchSettings{}
	for _, spec := range SettingSpecs {
		v, ok := fields[spec.Field]
		if !ok {
			v = spec.Default
		}
		applyField(&out, spec.Field, clampValue(spec, v))
	}
	return out
}

// clampValue clamps one raw value into spec's valid range/type, falling back
// to spec.Default on a bad type/choice (mirrors settings.py's `_clamped`).
func clampValue(spec Spec, value any) any {
	switch spec.Kind {
	case KindFloat, KindInt:
		f, ok := asNumber(value)
		if !ok {
			return spec.Default
		}
		clamped := math.Min(math.Max(f, spec.Lo), spec.Hi)
		if spec.Kind == KindInt {
			return int(clamped)
		}
		return clamped
	case KindBool:
		return truthy(value)
	case KindString:
		if s, ok := value.(string); ok && s != "" {
			return s
		}
		return spec.Default
	case KindChoice:
		if s, ok := value.(string); ok {
			for _, c := range spec.Choices {
				if c == s {
					return s
				}
			}
		}
		return spec.Default
	default:
		return spec.Default
	}
}

// asNumber accepts float64/int (JSON numbers or Go-typed CLI overrides) and
// explicitly rejects bool (Python: isinstance(value, bool) is checked before
// the numeric check, since bool is a subclass of int).
func asNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	default:
		return 0, false
	}
}

// truthy mirrors Python's bool(value) coercion for JSON-decoded types.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case int:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) != 0
	case map[string]any:
		return len(t) != 0
	default:
		return true
	}
}

// applyField assigns a clamped value into out's field, matching the type
// clampValue produces for each Kind.
func applyField(out *AutoSwitchSettings, field string, value any) {
	switch field {
	case "Threshold":
		out.Threshold = value.(float64)
	case "IntervalSeconds":
		out.IntervalSeconds = value.(float64)
	case "CooldownSeconds":
		out.CooldownSeconds = value.(float64)
	case "HysteresisPct":
		out.HysteresisPct = value.(float64)
	case "Strategy":
		out.Strategy = value.(string)
	case "IncludeAPIKeyAccounts":
		out.IncludeAPIKeyAccounts = value.(bool)
	case "UnhealthyTicks":
		out.UnhealthyTicks = value.(int)
	case "Model":
		if value == nil {
			out.Model = nil
		} else {
			m := value.(string)
			out.Model = &m
		}
	}
}

// fieldsOf projects an AutoSwitchSettings into a field-name → value map (the
// inverse of applyField/clamp), used by MergedWithCLI and Save.
func fieldsOf(s AutoSwitchSettings) map[string]any {
	var model any
	if s.Model != nil {
		model = *s.Model
	}
	return map[string]any{
		"Threshold":             s.Threshold,
		"IntervalSeconds":       s.IntervalSeconds,
		"CooldownSeconds":       s.CooldownSeconds,
		"HysteresisPct":         s.HysteresisPct,
		"Strategy":              s.Strategy,
		"IncludeAPIKeyAccounts": s.IncludeAPIKeyAccounts,
		"UnhealthyTicks":        s.UnhealthyTicks,
		"Model":                 model,
	}
}

// --- writing ---------------------------------------------------------------

// Save writes every known key + schemaVersion, preserving unknown
// keys/sections. Used by non-config callers (e.g. the TUI); NOT used by
// SetSetting, which would otherwise freeze current defaults into the file.
func Save(root string, s AutoSwitchSettings) error {
	path := SettingsPath(root)
	raw := readRaw(path)
	if _, ok := raw["schemaVersion"]; !ok {
		raw["schemaVersion"] = SchemaVersion
	}
	section, ok := raw["autoswitch"].(map[string]any)
	if !ok {
		section = map[string]any{}
	}
	fields := fieldsOf(s)
	for _, spec := range SettingSpecs {
		section[spec.JSONKey] = fields[spec.Field]
	}
	raw["autoswitch"] = section
	coercePyFloats(raw)
	return atomicfile.WriteJSON(path, raw, atomicfile.Opts{})
}

// SetSetting validates and persists one key for `cswap config set`, writing
// only that key (plus schemaVersion if absent) so a single set never
// freezes every other default into the file. Unknown keys/sections in the
// file survive. Returns the parsed value.
func SetSetting(root, dotted, raw string) (any, error) {
	spec, err := SpecFor(dotted)
	if err != nil {
		return nil, err
	}
	value, err := ParseSettingValue(spec, raw)
	if err != nil {
		return nil, err
	}
	path := SettingsPath(root)
	data, err := readRawForWrite(path)
	if err != nil {
		return nil, err
	}
	if _, ok := data["schemaVersion"]; !ok {
		data["schemaVersion"] = SchemaVersion
	}
	section, ok := data[spec.Section].(map[string]any)
	if !ok {
		section = map[string]any{}
	}
	section[spec.JSONKey] = value
	data[spec.Section] = section
	coercePyFloats(data)
	if err := atomicfile.WriteJSON(path, data, atomicfile.Opts{}); err != nil {
		return nil, err
	}
	return value, nil
}

// UnsetSetting removes one key from settings.json; if its section becomes
// empty the whole section is deleted. Returns false (and does not write)
// when the key wasn't present.
func UnsetSetting(root, dotted string) (bool, error) {
	spec, err := SpecFor(dotted)
	if err != nil {
		return false, err
	}
	path := SettingsPath(root)
	data, err := readRawForWrite(path)
	if err != nil {
		return false, err
	}
	section, ok := data[spec.Section].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, present := section[spec.JSONKey]; !present {
		return false, nil
	}
	if _, ok := data["schemaVersion"]; !ok {
		data["schemaVersion"] = SchemaVersion
	}
	delete(section, spec.JSONKey)
	if len(section) == 0 {
		delete(data, spec.Section)
	} else {
		data[spec.Section] = section
	}
	coercePyFloats(data)
	if err := atomicfile.WriteJSON(path, data, atomicfile.Opts{}); err != nil {
		return false, err
	}
	return true, nil
}

// Effective is one setting's effective value alongside whether it was
// explicitly present in the raw file.
type Effective struct {
	Spec  Spec
	Value any
	// IsSet reports whether the json_key is present in the raw file — an
	// explicit value equal to the default still counts (presence, not value
	// equality).
	IsSet bool
}

// EffectiveSettings returns (spec, effective value, explicitly set?) for
// every key, in registry order.
func EffectiveSettings(root string) []Effective {
	raw := readRaw(SettingsPath(root))
	fields := fieldsOf(Load(root))
	out := make([]Effective, 0, len(SettingSpecs))
	for _, spec := range SettingSpecs {
		isSet := false
		if section, ok := raw[spec.Section].(map[string]any); ok {
			_, isSet = section[spec.JSONKey]
		}
		out = append(out, Effective{Spec: spec, Value: fields[spec.Field], IsSet: isSet})
	}
	return out
}

// --- CLI merge / model names -------------------------------------------

// CLIOverrides holds the optional `cswap auto` flag overrides; a nil field
// means "not passed on the command line".
type CLIOverrides struct {
	Threshold             *float64
	IntervalSeconds       *float64
	CooldownSeconds       *float64
	IncludeAPIKeyAccounts *bool
	Model                 *string
}

func (o CLIOverrides) isEmpty() bool {
	return o.Threshold == nil && o.IntervalSeconds == nil && o.CooldownSeconds == nil &&
		o.IncludeAPIKeyAccounts == nil && o.Model == nil
}

// MergedWithCLI overlays o's non-nil overrides onto s, then re-clamps (so
// out-of-range CLI values are clamped too). With no overrides at all, s is
// returned unchanged.
func MergedWithCLI(s AutoSwitchSettings, o CLIOverrides) AutoSwitchSettings {
	if o.isEmpty() {
		return s
	}
	fields := fieldsOf(s)
	if o.Threshold != nil {
		fields["Threshold"] = *o.Threshold
	}
	if o.IntervalSeconds != nil {
		fields["IntervalSeconds"] = *o.IntervalSeconds
	}
	if o.CooldownSeconds != nil {
		fields["CooldownSeconds"] = *o.CooldownSeconds
	}
	if o.IncludeAPIKeyAccounts != nil {
		fields["IncludeAPIKeyAccounts"] = *o.IncludeAPIKeyAccounts
	}
	if o.Model != nil {
		fields["Model"] = *o.Model
	}
	return clamp(fields)
}

// ParseModelNames splits a comma-separated model list, trims each entry, and
// case-insensitively dedupes (first spelling wins). A nil or empty value
// returns an empty (non-nil) slice.
func ParseModelNames(v *string) []string {
	out := []string{}
	if v == nil || *v == "" {
		return out
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(*v, ",") {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

// --- strict parsing for `cswap config set` ------------------------------

var boolWords = map[string]bool{
	"true": true, "1": true, "yes": true,
	"false": false, "0": false, "no": false,
}

// ParseSettingValue strictly parses a CLI-provided string for
// `cswap config set`. Unlike the forgiving clamp on load, out-of-range or
// mistyped values return a cerr.Config so the user learns about the problem
// immediately rather than via silently degraded auto-switch behavior.
func ParseSettingValue(spec Spec, rawValue string) (any, error) {
	switch spec.Kind {
	case KindBool:
		key := strings.ToLower(strings.TrimSpace(rawValue))
		v, ok := boolWords[key]
		if !ok {
			return nil, cerr.Config("%s expects true or false (or 1/0, yes/no), got '%s'", spec.Dotted(), rawValue)
		}
		return v, nil
	case KindChoice:
		for _, c := range spec.Choices {
			if c == rawValue {
				return rawValue, nil
			}
		}
		return nil, cerr.Config("%s must be one of: %s", spec.Dotted(), strings.Join(spec.Choices, ", "))
	case KindString:
		value := strings.TrimSpace(rawValue)
		if value == "" {
			return nil, cerr.Config("%s expects a non-empty value; use 'cswap config unset %s' to clear it", spec.Dotted(), spec.Dotted())
		}
		return value, nil
	case KindInt:
		n, err := strconv.Atoi(rawValue)
		if err != nil {
			return nil, cerr.Config("%s expects an integer, got '%s'", spec.Dotted(), rawValue)
		}
		if float64(n) < spec.Lo || float64(n) > spec.Hi {
			return nil, cerr.Config("%s must be between %s and %s", spec.Dotted(), FormatSettingValue(spec.Lo), FormatSettingValue(spec.Hi))
		}
		return n, nil
	case KindFloat:
		f, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			return nil, cerr.Config("%s expects a number, got '%s'", spec.Dotted(), rawValue)
		}
		if f < spec.Lo || f > spec.Hi {
			return nil, cerr.Config("%s must be between %s and %s", spec.Dotted(), FormatSettingValue(spec.Lo), FormatSettingValue(spec.Hi))
		}
		return f, nil
	default:
		return nil, cerr.Config("unknown setting kind for %s", spec.Dotted())
	}
}

// FormatSettingValue renders a settings value the way settings.json (and
// config-set error messages) present it: nil -> "(none)"; bool ->
// "true"/"false"; a whole-number float -> its integer form (90.0 -> "90");
// anything else -> its natural string form.
func FormatSettingValue(value any) string {
	if value == nil {
		return "(none)"
	}
	switch t := value.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatFloat(t, 'f', 0, 64)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}
