package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestToolWatchdogConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits map[string]any
		valid  bool
	}{
		{"defaults", map[string]any{}, true},
		{"configured", map[string]any{"warn_after": "2m", "abort_after": "5m"}, true},
		{"warning only", map[string]any{"warn_after": "1m"}, true},
		{"abort only", map[string]any{"abort_after": "1h"}, true},
		{"zero", map[string]any{"warn_after": "0s"}, false},
		{"negative", map[string]any{"abort_after": "-1m"}, false},
		{"malformed", map[string]any{"warn_after": "tomorrow"}, false},
		{"overflow", map[string]any{"warn_after": "999999999999999h"}, false},
		{"equal", map[string]any{"warn_after": "10m", "abort_after": "10m"}, false},
		{"reversed", map[string]any{"warn_after": "20m", "abort_after": "10m"}, false},
		{"unknown key", map[string]any{"run_timeout": "1h"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			raw, err := json.Marshal(r.cfg)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]any
			if err := json.Unmarshal(raw, &values); err != nil {
				t.Fatal(err)
			}
			values["tool_watchdog"] = tc.limits
			raw, err = json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if err != nil && !strings.Contains(problems(err), "tool_watchdog") && !strings.Contains(problems(err), "run_timeout") {
				t.Fatalf("missing setting context: %v", err)
			}
		})
	}
}

func TestToolWatchdogDefaultAndConfiguredThresholdValues(t *testing.T) {
	for _, tc := range []struct {
		cfg         ToolWatchdog
		warn, abort time.Duration
	}{
		{ToolWatchdog{}, 10 * time.Minute, 30 * time.Minute},
		{ToolWatchdog{WarnAfter: "2m", AbortAfter: "5m"}, 2 * time.Minute, 5 * time.Minute},
	} {
		warn, abort, err := tc.cfg.Thresholds()
		if err != nil || warn != tc.warn || abort != tc.abort {
			t.Fatalf("thresholds=%v/%v, %v", warn, abort, err)
		}
	}
}
