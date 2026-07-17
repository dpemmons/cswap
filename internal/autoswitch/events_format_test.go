package autoswitch

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEventJSONFloatsKeepTrailingDecimal locks byte-parity with Python's
// json.dumps for the autoswitch JSONL float fields. The poll event's
// threshold/headroomPct/windowsPct and the sleep event's seconds are always
// Python floats, so a whole-number value must serialize as "80.0", not "80"
// (spec 05§3 poll example headroomPct {"1": 40.0}, float threshold; 08§8).
// The CLI emits these via encoding/json.Marshal (compact), matched here.
func TestEventJSONFloatsKeepTrailingDecimal(t *testing.T) {
	h := 40.0
	pe := PollEvent{
		Ts:        "2026-07-17T00:00:00Z",
		Active:    map[string]any{"number": 1, "email": "a@x"},
		Order:     []string{"1"},
		Headroom:  map[string]*float64{"1": &h},
		Threshold: 80.0,
		Windows:   map[string][]WindowPct{"1": {{Name: "5h", Pct: 60.0}, {Name: "7d", Pct: 89.5}}},
	}
	b, err := json.Marshal(pe.JSON())
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"threshold":80.0`, `"1":40.0`, `"5h":60.0`, `"7d":89.5`} {
		if !strings.Contains(got, want) {
			t.Errorf("poll JSON = %s, want it to contain %s", got, want)
		}
	}
	if strings.Contains(got, `"threshold":80,`) || strings.Contains(got, `"threshold":80}`) {
		t.Errorf("poll JSON = %s, threshold lost its trailing decimal", got)
	}

	se := SleepEvent{Ts: "2026-07-17T00:00:00Z", Seconds: 80.0, Until: "2026-07-17T00:01:20Z"}
	b, err = json.Marshal(se.JSON())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); !strings.Contains(got, `"seconds":80.0`) {
		t.Errorf("sleep JSON = %s, want it to contain \"seconds\":80.0", got)
	}
}
